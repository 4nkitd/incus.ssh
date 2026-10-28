package gateway

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

const (
	colorPrompt = "\x1b[36mincus> \x1b[0m"
	colorJSON   = `{"status":"ok"}`
)

func TestColorPromptPTY(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	commands := make(chan string, 1)
	cfg, host, key := testConfig(t, handlerFunc(func(_ context.Context, _ core.Principal, command string, streams core.Streams) int {
		commands <- command
		io.WriteString(streams.Out, colorJSON)
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	session.Stdin = strings.NewReader("json\rexit\r")
	var output bytes.Buffer
	session.Stdout = &output
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case command := <-commands:
		if command != "json" {
			t.Fatalf("command=%q", command)
		}
	default:
		t.Fatal("handler not called")
	}
	got := output.String()
	if !strings.Contains(got, "\x1b[1mIncus development gateway.\x1b[0m Type help for commands; exit to disconnect.\r\n") {
		t.Fatalf("colored banner missing: %q", got)
	}
	if strings.Count(got, colorPrompt) != 2 {
		t.Fatalf("prompt count=%d output=%q", strings.Count(got, colorPrompt), got)
	}
	if !strings.Contains(got, colorJSON+colorPrompt) {
		t.Fatalf("JSON output was styled or prompt did not follow guest: %q", got)
	}
}

func TestColorPlainWithoutPTY(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	cfg, host, key := testConfig(t, handlerFunc(func(_ context.Context, _ core.Principal, _ string, streams core.Streams) int {
		io.WriteString(streams.Out, colorJSON)
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	session.Stdin = strings.NewReader("json\nexit\n")
	var output bytes.Buffer
	session.Stdout = &output
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "\x1b") {
		t.Fatalf("plain session emitted ANSI: %q", got)
	}
	if !strings.Contains(got, "Incus development gateway. Type help for commands; exit to disconnect.\n") {
		t.Fatalf("plain banner missing: %q", got)
	}
	if !strings.Contains(got, colorJSON+"incus> ") {
		t.Fatalf("plain prompt or JSON missing: %q", got)
	}
}

func TestColorPlainWithDumbTerm(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	cfg, host, key := testConfig(t, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	session.Stdin = strings.NewReader("exit\r")
	var output bytes.Buffer
	session.Stdout = &output
	if err := session.RequestPty("dumb", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "\x1b") {
		t.Fatalf("dumb terminal emitted ANSI: %q", got)
	}
	if !strings.Contains(got, "incus> ") || !strings.Contains(got, "\r\n") {
		t.Fatalf("plain dumb terminal output: %q", got)
	}
}

func TestColorExecJSONNotStyled(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	cfg, host, key := testConfig(t, handlerFunc(func(_ context.Context, _ core.Principal, _ string, streams core.Streams) int {
		io.WriteString(streams.Out, colorJSON)
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	var output bytes.Buffer
	session.Stdout = &output
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Run("json"); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "\x1b") || !strings.Contains(got, colorJSON) {
		t.Fatalf("exec output styled by gateway: %q", got)
	}
}

func TestColorErrorsOnlyOnPTY(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	handler := handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 })
	long := strings.Repeat("x", maxCommandLength+1) + "\n"
	for _, tc := range []struct {
		name    string
		streams func(*bytes.Buffer) core.Streams
		want    string
	}{
		{"unavailable PTY", func(errOut *bytes.Buffer) core.Streams {
			return core.Streams{In: strings.NewReader(""), Out: io.Discard, Err: errOut, PTY: true, Term: "xterm"}
		}, "\x1b[31msession input is unavailable\x1b[0m\r\n"},
		{"unavailable plain", func(errOut *bytes.Buffer) core.Streams {
			return core.Streams{In: strings.NewReader(""), Out: io.Discard, Err: errOut}
		}, "session input is unavailable\n"},
		{"too long PTY", func(errOut *bytes.Buffer) core.Streams {
			return core.Streams{In: testSessionInput(t, strings.NewReader(long)), Out: io.Discard, Err: errOut, PTY: true, Term: "xterm"}
		}, "\x1b[31mcommand too long\x1b[0m\r\n"},
		{"too long plain", func(errOut *bytes.Buffer) core.Streams {
			return core.Streams{In: testSessionInput(t, strings.NewReader(long)), Out: io.Discard, Err: errOut}
		}, "command too long\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			code := repl(context.Background(), core.Principal{}, handler, tc.streams(&errOut))
			if code != 1 || !strings.Contains(errOut.String(), tc.want) {
				t.Fatalf("code=%d stderr=%q want=%q", code, errOut.String(), tc.want)
			}
		})
	}
}
