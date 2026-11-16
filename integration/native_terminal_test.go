package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"incus-for-devs/internal/controlapi"
	"incus-for-devs/internal/core"
	"incus-for-devs/internal/incus"
)

func TestNativeBrowserTerminal(t *testing.T) {
	project := os.Getenv("IFD_NATIVE_TERMINAL_PROJECT")
	if project == "" {
		t.Skip("requires an isolated native terminal project")
	}
	p := core.Principal{Name: "test", Project: project}
	b, err := incus.New(incus.Config{Binary: "/usr/bin/incus", Profile: "ifd", Image: "local:unused", OCIRemote: "docker", AdmissionDirectory: filepath.Join(t.TempDir(), "admission"), Limits: core.Limits{RunningMemory: "1GiB", CPU: 1, Memory: "512MiB", Processes: 128}, OperationTimeout: 180 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := b.CreateApp(ctx, p, core.AppOptions{Name: "terminal-qa", Image: "alpine:latest", Memory: "256MiB", DevelopmentBox: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := b.Remove(ctx, p, "terminal-qa"); err != nil {
			t.Error(err)
		}
	})
	origin := "https://terminal.example.com"
	tokens, err := controlapi.OpenTokens(filepath.Join(t.TempDir(), "api"), origin, p)
	if err != nil {
		t.Fatal(err)
	}
	defer tokens.Close()
	status, err := tokens.Manage(p, "enable")
	if err != nil {
		t.Fatal(err)
	}
	handler := controlapi.NewHandler(tokens, b, nil, 180*time.Second, 8, "native-test", "")
	var handlers sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		r.Host = "terminal.example.com"
		handler.ServeHTTP(w, r)
	}))
	defer func() { server.Close(); handlers.Wait() }()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/vms/terminal-qa/terminal", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{origin}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	auth, _ := json.Marshal(map[string]any{"type": "auth", "token": status.Token, "cols": 80, "rows": 24})
	if err := conn.Write(ctx, websocket.MessageText, auth); err != nil {
		t.Fatal(err)
	}
	readUntil := func(needle string) {
		readCtx, stop := context.WithTimeout(ctx, 8*time.Second)
		defer stop()
		output := ""
		for !strings.Contains(output, needle) {
			kind, data, err := conn.Read(readCtx)
			if err != nil {
				t.Fatalf("waiting for %q: %v; output %q", needle, err, output)
			}
			if kind == websocket.MessageBinary {
				output += string(data)
				if err := conn.Write(readCtx, websocket.MessageText, []byte(`{"type":"ack"}`)); err != nil {
					t.Fatal(err)
				}
			} else {
				output += string(data)
			}
		}
	}
	send := func(value string) {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageBinary, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	readUntil(`"ready"`)
	send("printf 'NATIVE_%s\\n' 'READY'\r")
	readUntil("NATIVE_READY")
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":100,"rows":35}`)); err != nil {
		t.Fatal(err)
	}
	send("stty size\r")
	readUntil("35 100")
	send("printf 'SLEEP_%s\\n' 'BEGIN'; sleep 30\r")
	readUntil("SLEEP_BEGIN")
	send("\x03")
	send("printf 'CTRL_%s\\n' 'OK'\r")
	readUntil("CTRL_OK")
	send("printf 'UNICODE_%s\\n' 'héllo'\r")
	readUntil("UNICODE_héllo")
	send("exit 7\r")
	readUntil(`"code":7`)
	t.Log("native browser PTY passed shell I/O, resize, Ctrl-C, Unicode and exit status")
}
