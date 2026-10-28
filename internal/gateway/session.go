package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

const maxCommandLength = 16 * 1024

type lockedWriter struct {
	mu     *sync.Mutex
	writer io.Writer
}

func (w lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

func serveSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, principal core.Principal, handler core.Handler, closeConnection func() error) {
	ctx, cancel := context.WithCancel(ctx)
	teardownDone, closeDone := make(chan struct{}), make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(closeDone)
		// Cancellation must release blocked replies before teardown can drain requests.
		deadline := time.AfterFunc(time.Second, func() { closeConnection() })
		defer deadline.Stop()
		channel.Close()
		<-teardownDone
	})
	input := newSessionInput(ctx, channel)
	var running sync.WaitGroup
	defer func() {
		// SSH dispatch must keep consuming requests until mux closes the channel.
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for range requests {
			}
		}()
		cancel()
		channel.Close()
		running.Wait()
		<-input.done
		<-drained
		close(teardownDone)
		if !stop() {
			<-closeDone
		}
	}()
	var writeMu sync.Mutex
	managed := &sessionHandler{handler: handler}
	streams := core.Streams{
		In:    input,
		Out:   lockedWriter{&writeMu, channel},
		Err:   lockedWriter{&writeMu, channel.Stderr()},
		Abort: cancel,
	}
	result := make(chan int, 1)
	started := false
	for {
		select {
		case <-ctx.Done():
			return
		case code := <-result:
			if ctx.Err() != nil {
				return
			}
			if code < 0 || uint64(code) > uint64(^uint32(0)) {
				code = 1
			}
			channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
			return
		case request, ok := <-requests:
			if !ok {
				return
			}
			accepted := false
			start := false
			command := ""
			shell := false
			if len(request.Payload) <= maxCommandLength+1024 {
				switch request.Type {
				case "pty-req":
					var p struct {
						Term                                   string
						Width, Height, PixelWidth, PixelHeight uint32
						Modes                                  string
					}
					if !started && !streams.PTY && ssh.Unmarshal(request.Payload, &p) == nil && validTerm(p.Term) && validWindow(p.Width, p.Height, p.PixelWidth, p.PixelHeight) && validModes(p.Modes) {
						streams.PTY, streams.Term = true, p.Term
						if streams.Term == "" {
							streams.Term = "dumb"
						}
						streams.Window = core.Window{Width: p.Width, Height: p.Height}
						managed.resize(streams.Window)
						accepted = true
					}
				case "window-change":
					var p struct{ Width, Height, PixelWidth, PixelHeight uint32 }
					if streams.PTY && ssh.Unmarshal(request.Payload, &p) == nil && validWindow(p.Width, p.Height, p.PixelWidth, p.PixelHeight) {
						managed.resize(core.Window{Width: p.Width, Height: p.Height})
						accepted = true
					}
				case "signal":
					var p struct{ Signal string }
					if started && ssh.Unmarshal(request.Payload, &p) == nil && validSignal(p.Signal) {
						accepted = managed.signal(p.Signal)
					}
				case "exec":
					var p struct{ Command string }
					if !started && ssh.Unmarshal(request.Payload, &p) == nil && len(p.Command) <= maxCommandLength {
						accepted, start, command = true, true, p.Command
					}
				case "shell":
					if !started && len(request.Payload) == 0 {
						accepted, start, shell = true, true, true
					}
				case "env", "subsystem", "auth-agent-req@openssh.com", "x11-req":
					accepted = false
				}
			}
			commandStreams := streams
			var finish func()
			if start && !shell {
				commandStreams, finish = managed.begin(streams)
			}
			if err := request.Reply(accepted, nil); err != nil {
				if finish != nil {
					finish()
				}
				return
			}
			if start {
				started = true
				running.Add(1)
				go func(streams core.Streams, command string, shell bool, finish func()) {
					defer running.Done()
					if shell {
						result <- repl(ctx, principal, managed, streams)
					} else {
						code := handler.Handle(ctx, principal, command, streams)
						finish()
						result <- code
					}
				}(commandStreams, command, shell, finish)
			}
		}
	}
}

func validWindow(width, height, pixelWidth, pixelHeight uint32) bool {
	return width <= 65535 && height <= 65535 && pixelWidth <= 1<<20 && pixelHeight <= 1<<20
}

func validTerm(term string) bool {
	if len(term) > 64 {
		return false
	}
	for _, c := range term {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._+-", c)) {
			return false
		}
	}
	return true
}

func validModes(modes string) bool {
	if len(modes) > 1024 {
		return false
	}
	for len(modes) > 0 {
		if modes[0] == 0 {
			return len(modes) == 1
		}
		if modes[0] >= 160 || len(modes) < 5 {
			return false
		}
		modes = modes[5:]
	}
	return false
}

func validSignal(signal string) bool {
	switch signal {
	case "HUP", "INT", "QUIT", "TERM", "USR1", "USR2":
		return true
	}
	return false
}

type replReader struct {
	ctx    context.Context
	reader core.ContextReader
	gate   chan struct{}
	skipLF bool
}

func newREPLReader(ctx context.Context, reader core.ContextReader) *replReader {
	r := &replReader{ctx: ctx, reader: reader, gate: make(chan struct{}, 1)}
	r.gate <- struct{}{}
	return r
}

func (r *replReader) Read(p []byte) (int, error) {
	return r.ReadContext(r.ctx, p)
}

func (r *replReader) ReadContext(ctx context.Context, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.gate:
	}
	defer func() { r.gate <- struct{}{} }()
	return r.read(ctx, p)
}

func (r *replReader) sourceRead(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.ReadContext(ctx, p)
}

func (r *replReader) read(ctx context.Context, p []byte) (int, error) {
	if r.skipLF {
		var b [1]byte
		n, err := r.sourceRead(ctx, b[:])
		if n == 0 {
			return 0, err
		}
		r.skipLF = false
		if b[0] != '\n' {
			p[0] = b[0]
			return 1, err
		}
		if err != nil {
			return 0, err
		}
	}
	return r.sourceRead(ctx, p)
}

func (r *replReader) line(ctx context.Context, echo io.Writer) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.gate:
	}
	defer func() { r.gate <- struct{}{} }()
	var line []byte
	for {
		var data [1]byte
		n, err := r.read(ctx, data[:])
		if n == 0 && err != nil {
			return line, err
		}
		if n == 0 {
			continue
		}
		b := data[0]
		if b == '\n' || b == '\r' {
			r.skipLF = b == '\r'
			if echo != nil {
				if _, err := io.WriteString(echo, "\r\n"); err != nil {
					return nil, err
				}
			}
			return line, nil
		}
		if echo != nil {
			if b == 8 || b == 127 {
				if len(line) > 0 {
					_, width := utf8.DecodeLastRune(line)
					line = line[:len(line)-width]
					if _, err := io.WriteString(echo, "\b \b"); err != nil {
						return nil, err
					}
				}
				continue
			}
			if b == 4 && len(line) == 0 {
				return nil, io.EOF
			}
			if b < 32 || b == 127 {
				continue
			}
		}
		if len(line) == maxCommandLength {
			return nil, bufio.ErrBufferFull
		}
		line = append(line, b)
		if echo != nil {
			if _, err := echo.Write(data[:]); err != nil {
				return nil, err
			}
		}
		if err != nil {
			return line, err
		}
	}
}

func repl(ctx context.Context, principal core.Principal, handler core.Handler, streams core.Streams) int {
	newline := "\n"
	if streams.PTY {
		newline = "\r\n"
	}
	// One session pump owns read-ahead; command cancellation never leaves a second reader behind.
	input, ok := streams.In.(core.ContextReader)
	if !ok {
		io.WriteString(streams.Err, "session input is unavailable"+newline)
		return 1
	}
	reader := newREPLReader(ctx, input)
	streams.In = reader
	if _, err := io.WriteString(streams.Out, "Incus development gateway. Type help for commands; exit to disconnect."+newline); err != nil {
		return 1
	}
	var echo io.Writer
	if streams.PTY {
		echo = streams.Out
		if _, err := io.WriteString(streams.Out, "Line editing supports backspace only; arrow keys are unsupported."+newline); err != nil {
			return 1
		}
	}
	for ctx.Err() == nil {
		if _, err := io.WriteString(streams.Out, "incus> "); err != nil {
			return 1
		}
		line, err := reader.line(ctx, echo)
		if errors.Is(err, bufio.ErrBufferFull) {
			io.WriteString(streams.Err, "command too long"+newline)
			return 1
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 1
		}
		command := strings.TrimSpace(string(line))
		if command == "exit" || command == "quit" {
			return 0
		}
		if command != "" {
			handler.Handle(ctx, principal, command, streams)
		}
		if errors.Is(err, io.EOF) {
			return 0
		}
	}
	return 1
}
