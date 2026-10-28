package command

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/terminal"
)

func TestClearWritesExactBytesOnInteractiveTerminal(t *testing.T) {
	for _, term := range []string{"xterm", "xterm-256color", "screen"} {
		t.Run(term, func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			backend := &fakeBackend{}
			streams, out, errOut := colorStreams()
			streams.Term = term
			status := (Handler{Backend: backend}).Handle(context.Background(), principal, "clear", streams)
			if status != 0 || errOut.Len() != 0 {
				t.Fatalf("status=%d stderr=%q", status, errOut.String())
			}
			if out.String() != clearSequence {
				t.Fatalf("clear wrote %q, want %q", out.String(), clearSequence)
			}
			if strings.ContainsAny(out.String(), "\r\n") {
				t.Fatalf("clear added a line ending: %q", out.String())
			}
			if len(backend.calls) != 0 {
				t.Fatalf("clear called the backend: %v", backend.calls)
			}
		})
	}
}

func TestClearWorksWithNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	streams, out, errOut := colorStreams()
	if status := (Handler{}).Handle(context.Background(), principal, "clear", streams); status != 0 {
		t.Fatalf("status=%d stderr=%q", status, errOut.String())
	}
	if out.String() != clearSequence {
		t.Fatalf("NO_COLOR suppressed clear: %q", out.String())
	}
}

func TestClearRequiresInteractiveTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	cases := []struct {
		name string
		s    core.Streams
	}{
		{"not a pty", core.Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Term: "xterm"}},
		{"empty term", core.Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, PTY: true}},
		{"dumb term", core.Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, PTY: true, Term: "dumb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeBackend{}
			status := (Handler{Backend: backend}).Handle(context.Background(), principal, "clear", tc.s)
			out := tc.s.Out.(*bytes.Buffer)
			errOut := tc.s.Err.(*bytes.Buffer)
			if status == 0 || out.Len() != 0 || len(backend.calls) != 0 {
				t.Fatalf("status=%d out=%q calls=%v", status, out.String(), backend.calls)
			}
			if !strings.Contains(errOut.String(), "clear requires an interactive terminal") {
				t.Fatalf("missing diagnostic: %q", errOut.String())
			}
			if strings.Contains(out.String()+errOut.String(), "\x1b") {
				t.Fatalf("non-interactive clear emitted ANSI: %q", out.String()+errOut.String())
			}
		})
	}
}

func TestClearRejectsArgumentsAndJSONFlags(t *testing.T) {
	for _, line := range []string{"clear now", "clear --json", "clear --json now", "clear --json=true", "clear -x"} {
		t.Run(line, func(t *testing.T) {
			backend := &fakeBackend{}
			status, out, stderr := run(Handler{Backend: backend}, line)
			if status == 0 || len(backend.calls) != 0 {
				t.Fatalf("status=%d out=%q stderr=%q calls=%v", status, out, stderr, backend.calls)
			}
			if line == "clear --json" || line == "clear --json now" {
				var payload map[string]string
				if err := json.Unmarshal([]byte(out), &payload); err != nil || payload["error"] == "" || stderr != "" {
					t.Fatalf("invalid JSON error: out=%q stderr=%q", out, stderr)
				}
			} else if out != "" || !strings.Contains(stderr, "clear") {
				t.Fatalf("unexpected plain diagnostic: out=%q stderr=%q", out, stderr)
			}
			if strings.Contains(out+stderr, "\x1b") {
				t.Fatalf("unexpected rejection: out=%q stderr=%q", out, stderr)
			}
		})
	}
}

func TestHelpListsClearWithColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	streams, out, errOut := colorStreams()
	if status := (Handler{}).Handle(context.Background(), principal, "help all", streams); status != 0 {
		t.Fatalf("status=%d stderr=%q", status, errOut.String())
	}
	if !strings.Contains(out.String(), terminal.Cyan+"  clear"+terminal.Reset) {
		t.Fatalf("clear usage is not cyan: %q", out.String())
	}
	if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), "Fails without an interactive terminal.") {
		t.Fatalf("clear description missing: %q", out.String())
	}
	status, helpOut, stderr := run(Handler{}, "help clear")
	if status != 0 || !strings.Contains(helpOut, "clear") || stderr != "" {
		t.Fatalf("status=%d out=%q stderr=%q", status, helpOut, stderr)
	}
}
