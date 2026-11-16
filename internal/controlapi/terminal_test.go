package controlapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"incus-for-devs/internal/core"
)

func terminalServer(t *testing.T, f fixture) *httptest.Server {
	t.Helper()
	var handlers sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		r.Host = strings.TrimPrefix(testOrigin, "https://")
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { srv.Close(); handlers.Wait() })
	return srv
}

func dialTerminal(t *testing.T, srv *httptest.Server, name, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/vms/"+name+"/terminal", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{testOrigin}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	data, _ := json.Marshal(terminalMessage{Type: "auth", Token: token, Cols: 100, Rows: 30})
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	return conn
}

func terminalRead(t *testing.T, conn *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kind, data, err := conn.Read(ctx)
	for err == nil && kind == websocket.MessageText && strings.Contains(string(data), `"type":"input_ack"`) {
		kind, data, err = conn.Read(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	return kind, data
}

func TestTerminalStreamsResizeAndExit(t *testing.T) {
	f := newFixture(t, time.Second, 2)
	f.backend.execFn = func(ctx context.Context, streams core.Streams) (int, error) {
		if !streams.PTY || streams.Term != "xterm-256color" || streams.Window.Width != 100 || streams.Window.Height != 30 {
			return -1, io.ErrUnexpectedEOF
		}
		select {
		case size := <-streams.Resize:
			if size.Width != 120 || size.Height != 40 {
				return -1, io.ErrUnexpectedEOF
			}
		case <-ctx.Done():
			return -1, ctx.Err()
		}
		input := make([]byte, 4)
		if _, err := io.ReadFull(streams.In, input); err != nil {
			return -1, err
		}
		_, err := streams.Out.Write(input)
		return 0, err
	}
	conn := dialTerminal(t, terminalServer(t, f), "demo", f.token)
	_, ready := terminalRead(t, conn)
	if !strings.Contains(string(ready), `"ready"`) {
		t.Fatal(string(ready))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("test")); err != nil {
		t.Fatal(err)
	}
	kind, output := terminalRead(t, conn)
	if kind != websocket.MessageBinary || string(output) != "test" {
		t.Fatal(kind, string(output))
	}
	_, exit := terminalRead(t, conn)
	if !strings.Contains(string(exit), `"code":0`) {
		t.Fatal(string(exit))
	}
}

func TestTerminalRejectsTokenAndForeignContainer(t *testing.T) {
	for _, name := range []string{"invalid-token", "foreign"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, time.Second, 2)
			token, container := f.token, "demo"
			if name == "invalid-token" {
				token = "invalid"
			} else {
				container = "foreign"
			}
			conn := dialTerminal(t, terminalServer(t, f), container, token)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _, err := conn.Read(ctx)
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatal(err)
			}
			f.backend.mu.Lock()
			defer f.backend.mu.Unlock()
			for _, call := range f.backend.calls {
				if strings.HasPrefix(call, "exec:") {
					t.Fatal("unauthorized exec", call)
				}
			}
		})
	}
}

func TestTerminalOriginAndQueryGuards(t *testing.T) {
	f := newFixture(t, time.Second, 1)
	for _, tc := range []struct {
		origin, path string
		status       int
	}{{"", "/api/v1/vms/demo/terminal", 403}, {"https://evil.example", "/api/v1/vms/demo/terminal", 403}, {testOrigin, "/api/v1/vms/demo/terminal?token=secret", 400}, {testOrigin, "/api/v1/vms/a/b/terminal", 400}} {
		r := f.request("GET", tc.path, "")
		r.Header.Set("Origin", tc.origin)
		if w := f.send(r); w.Code != tc.status {
			t.Fatal(tc, w.Code)
		}
	}
}

func TestTerminalRevocationAndDisconnectCancelExec(t *testing.T) {
	for _, action := range []string{"revoke", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t, time.Second, 1)
			started, stopped := make(chan struct{}), make(chan struct{})
			f.backend.execFn = func(ctx context.Context, s core.Streams) (int, error) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return -1, ctx.Err()
			}
			conn := dialTerminal(t, terminalServer(t, f), "demo", f.token)
			terminalRead(t, conn)
			<-started
			if err := conn.Write(context.Background(), websocket.MessageBinary, []byte("input the guest will not consume")); err != nil {
				t.Fatal(err)
			}
			if w := f.send(f.request("GET", "/api/v1/vms", "")); w.Code != 429 {
				t.Fatal("terminal did not share concurrency bound", w.Code)
			}
			if action == "revoke" {
				if _, err := f.tokens.Manage(testPrincipal, "rotate"); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = conn.CloseNow()
			}
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("exec not cancelled")
			}
		})
	}
}

func TestTerminalOutputAcknowledgementWindow(t *testing.T) {
	f := newFixture(t, time.Second, 1)
	f.backend.execFn = func(ctx context.Context, s core.Streams) (int, error) {
		_, err := s.Out.Write([]byte(strings.Repeat("x", 9*8192)))
		return 0, err
	}
	conn := dialTerminal(t, terminalServer(t, f), "demo", f.token)
	terminalRead(t, conn)
	for range 8 {
		kind, data := terminalRead(t, conn)
		if kind != websocket.MessageBinary || len(data) != 8192 {
			t.Fatal(kind, len(data))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ack"}`)); err != nil {
		t.Fatal(err)
	}
	kind, data := terminalRead(t, conn)
	if kind != websocket.MessageBinary || len(data) != 8192 {
		t.Fatal(kind, len(data))
	}
	_, exit := terminalRead(t, conn)
	if !strings.Contains(string(exit), `"code":0`) {
		t.Fatal(string(exit))
	}
}
