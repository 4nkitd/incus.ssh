package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"incus-for-devs/internal/command"
	"incus-for-devs/internal/core"
)

type terminalMessage struct {
	Type  string `json:"type"`
	Token string `json:"token,omitempty"`
	Cols  uint32 `json:"cols,omitempty"`
	Rows  uint32 `json:"rows,omitempty"`
}

func terminalDimensions(cols, rows uint32) bool {
	return cols >= 2 && cols <= 500 && rows >= 1 && rows <= 200
}

func decodeTerminalMessage(data []byte) (terminalMessage, error) {
	var message terminalMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&message)
	if err == nil && decoder.Decode(new(any)) != io.EOF {
		err = errors.New("invalid message")
	}
	return message, err
}

func (s service) terminal(w http.ResponseWriter, r *http.Request, tokens *Tokens, gate chan struct{}, operationTimeout time.Duration) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/vms/"), "/terminal")
	if r.Method != http.MethodGet || command.ValidateName(name) != nil {
		writeError(w, 400, "invalid terminal request")
		return
	}
	if r.Header.Get("Origin") != tokens.origin {
		writeError(w, 403, "terminal origin is not allowed")
		return
	}
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		writeError(w, 429, "too many concurrent API requests")
		return
	}
	// Host and exact Origin were checked above, including trusted-proxy origin translation.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 10)
	authCtx, authCancel := context.WithTimeout(r.Context(), 5*time.Second)
	kind, data, err := conn.Read(authCtx)
	authCancel()
	if err != nil {
		return
	}
	auth, err := decodeTerminalMessage(data)
	if err != nil || kind != websocket.MessageText || auth.Type != "auth" || !terminalDimensions(auth.Cols, auth.Rows) {
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid terminal authentication")
		return
	}
	p, epoch, ok := tokens.authenticate(auth.Token)
	auth.Token = ""
	if !ok {
		_ = conn.Close(websocket.StatusPolicyViolation, "token rejected or revoked")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	stop := context.AfterFunc(epoch, cancel)
	defer stop()
	if epoch.Err() != nil {
		return
	}
	checkCtx, checkCancel := context.WithTimeout(ctx, operationTimeout)
	items, err := s.backend.List(checkCtx, p)
	checkCancel()
	allowed := false
	for _, item := range items {
		if item.Name == name && item.Status == "Running" {
			allowed = true
		}
	}
	if err != nil || !allowed {
		_ = conn.Close(websocket.StatusPolicyViolation, "choose an owned running container")
		return
	}
	input, feed := io.Pipe()
	defer input.Close()
	defer feed.Close()
	stopIO := context.AfterFunc(ctx, func() { _ = input.Close(); _ = feed.Close(); _ = conn.CloseNow() })
	defer stopIO()
	resize := make(chan core.Window, 1)
	output := &terminalOutput{ctx: ctx, conn: conn, credits: make(chan struct{}, 8)}
	for range cap(output.credits) {
		output.credits <- struct{}{}
	}
	if err := terminalEvent(ctx, conn, map[string]any{"type": "ready"}); err != nil {
		return
	}
	s.activity.record("terminal_open", name, true)
	readDone := make(chan struct{})
	inputQueue := make(chan []byte, 8)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		defer cancel()
		for {
			select {
			case data := <-inputQueue:
				if _, err := feed.Write(data); err != nil {
					return
				}
				if err := terminalEvent(ctx, conn, map[string]any{"type": "input_ack"}); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer close(readDone)
		defer cancel()
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				select {
				case inputQueue <- data:
				default:
					return
				}
				continue
			}
			message, err := decodeTerminalMessage(data)
			if err != nil || message.Token != "" {
				return
			}
			switch message.Type {
			case "resize":
				if !terminalDimensions(message.Cols, message.Rows) {
					return
				}
				select {
				case <-resize:
				default:
				}
				select {
				case resize <- core.Window{Width: message.Cols, Height: message.Rows}:
				case <-ctx.Done():
					return
				}
			case "ack":
				select {
				case output.credits <- struct{}{}:
				default:
					return
				}
			default:
				return
			}
		}
	}()
	code, execErr := s.backend.Exec(ctx, p, name, "", core.Streams{In: input, Out: output, Err: output, PTY: true, Term: "xterm-256color", Window: core.Window{Width: auth.Cols, Height: auth.Rows}, Resize: resize, Abort: cancel})
	if execErr != nil {
		_ = terminalEvent(ctx, conn, map[string]any{"type": "error", "message": "Terminal ended. Check container state and reconnect."})
	} else {
		_ = terminalEvent(ctx, conn, map[string]any{"type": "exit", "code": code})
	}
	cancel()
	_ = conn.CloseNow()
	_ = feed.Close()
	_ = input.Close()
	<-readDone
	<-inputDone
	s.activity.record("terminal_close", name, execErr == nil)
}

func terminalEvent(ctx context.Context, conn *websocket.Conn, event any) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

type terminalOutput struct {
	ctx     context.Context
	conn    *websocket.Conn
	credits chan struct{}
	mu      sync.Mutex
}

func (w *terminalOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := 0
	for len(data) > 0 {
		n := min(len(data), 8192)
		ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
		select {
		case <-w.credits:
		case <-ctx.Done():
			cancel()
			return written, ctx.Err()
		}
		err := w.conn.Write(ctx, websocket.MessageBinary, data[:n])
		cancel()
		if err != nil {
			return written, err
		}
		written += n
		data = data[n:]
	}
	return written, nil
}
