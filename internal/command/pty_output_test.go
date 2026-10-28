package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
)

func ptyStreams() (core.Streams, *bytes.Buffer, *bytes.Buffer) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	return core.Streams{Out: out, Err: errOut, PTY: true}, out, errOut
}

func assertCRLF(t *testing.T, text string) {
	t.Helper()
	if text == "" {
		t.Fatal("expected output, got none")
	}
	if !strings.HasSuffix(text, "\r\n") {
		t.Fatalf("missing final CRLF: %q", text)
	}
	if strings.Contains(text, "\r\r") {
		t.Fatalf("doubled carriage return: %q", text)
	}
	for i, r := range text {
		if r == '\n' && (i == 0 || text[i-1] != '\r') {
			t.Fatalf("bare newline at offset %d: %q", i, text)
		}
		if r == '\r' && (i+1 == len(text) || text[i+1] != '\n') {
			t.Fatalf("bare carriage return at offset %d: %q", i, text)
		}
	}
}

func TestHelpUsesCRLFOnPTY(t *testing.T) {
	streams, out, errOut := ptyStreams()
	status := (Handler{}).Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, "help", streams)
	if status != 0 || errOut.Len() != 0 {
		t.Fatalf("status=%d stderr=%q", status, errOut.String())
	}
	assertCRLF(t, out.String())
	assertHelpText(t, strings.ReplaceAll(out.String(), "\r\n", "\n"))
	if !strings.Contains(out.String(), "help [all|COMMAND]") {
		t.Fatalf("missing usage: %q", out.String())
	}
	if !strings.HasSuffix(out.String(), "\r\n") {
		t.Fatalf("missing final CRLF: %q", out.String())
	}
}

func TestTextAndErrorsUseCRLFOnPTY(t *testing.T) {
	streams, out, errOut := ptyStreams()
	h := Handler{Backend: &fakeBackend{instances: []core.Instance{{Name: "demo", Status: "Running"}}}}
	if status := h.Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, "ls", streams); status != 0 {
		t.Fatalf("ls status=%d", status)
	}
	assertCRLF(t, out.String())

	out.Reset()
	h = Handler{Backend: &fakeBackend{listError: errors.New("refused")}}
	if status := h.Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, "ls", streams); status == 0 {
		t.Fatal("expected failure")
	}
	assertCRLF(t, errOut.String())
	if !strings.HasSuffix(errOut.String(), "\r\n") {
		t.Fatalf("missing final CRLF: %q", errOut.String())
	}
}

func TestJSONErrorUsesCRLFOnPTY(t *testing.T) {
	streams, out, errOut := ptyStreams()
	h := Handler{Backend: &fakeBackend{listError: errors.New("refused")}}
	if status := h.Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, "ls --json", streams); status == 0 {
		t.Fatal("expected failure")
	}
	assertCRLF(t, out.String())
	var decoded map[string]string
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if decoded["error"] != "refused" || errOut.Len() != 0 {
		t.Fatalf("wrong payload: %v stderr=%q", decoded, errOut.String())
	}
}

func TestJSONRemainsValidOnPTY(t *testing.T) {
	streams, out, errOut := ptyStreams()
	if status := (Handler{}).Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, "whoami --json", streams); status != 0 {
		t.Fatalf("status=%d stderr=%q", status, errOut.String())
	}
	assertCRLF(t, out.String())
	want := "{\"name\":\"alice\",\"project\":\"dev-alice\"}\r\n"
	if out.String() != want {
		t.Fatalf("output=%q want=%q", out.String(), want)
	}
	var decoded map[string]string
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestRemoveErrorUsesCRLFOnPTY(t *testing.T) {
	streams, out, errOut := ptyStreams()
	h := Handler{Backend: &fakeBackend{removeError: map[string]error{"bad": errors.New("not found")}}}
	if status := h.Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, "rm good bad", streams); status == 0 {
		t.Fatal("expected failure")
	}
	if out.String() != "removed good\r\n" {
		t.Fatalf("stdout=%q", out.String())
	}
	assertCRLF(t, errOut.String())
	if !strings.Contains(errOut.String(), "error:") {
		t.Fatalf("missing diagnostic: %q", errOut.String())
	}
}

type execWriter struct{ *fakeBackend }

func (e execWriter) Exec(ctx context.Context, p core.Principal, name, command string, s core.Streams) (int, error) {
	_, _ = s.Out.Write([]byte("a\nb\n"))
	return 0, nil
}

func TestJSONWriteFailureUsesCRLFDiagnostic(t *testing.T) {
	var stderr bytes.Buffer
	streams := core.Streams{Out: brokenWriter{}, Err: &stderr, PTY: true}
	if status := (Handler{}).Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, "bogus --json", streams); status == 0 {
		t.Fatal("expected failure")
	}
	assertCRLF(t, stderr.String())
	if !strings.Contains(stderr.String(), "closed pipe") {
		t.Fatalf("missing diagnostic: %q", stderr.String())
	}
}

func TestGuestOutputIsNotTranslated(t *testing.T) {
	streams, out, _ := ptyStreams()
	backend := execWriter{&fakeBackend{}}
	status := (Handler{Backend: backend}).Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, "ssh demo", streams)
	if status != 0 {
		t.Fatalf("status=%d", status)
	}
	if out.String() != "a\nb\n" {
		t.Fatalf("guest output was rewritten: %q", out.String())
	}
}

func TestWriteOutputTo(t *testing.T) {
	tests := []struct {
		name string
		pty  bool
		in   string
		want string
	}{
		{"plain", false, "a\nb", "a\nb"},
		{"pty LF", true, "a\nb", "a\r\nb"},
		{"pty CRLF", true, "a\r\nb", "a\r\nb"},
		{"pty mixed", true, "a\r\nb\nc", "a\r\nb\r\nc"},
		{"pty empty", true, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := writeOutputTo(&b, tc.pty, tc.in); err != nil {
				t.Fatal(err)
			}
			if b.String() != tc.want {
				t.Fatalf("got %q want %q", b.String(), tc.want)
			}
		})
	}
	if err := writeOutputTo(nil, true, "x"); err != nil {
		t.Fatalf("nil writer: %v", err)
	}
}

func TestNonPTYKeepsLF(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	streams := core.Streams{Out: out, Err: errOut}
	if status := (Handler{}).Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, "help", streams); status != 0 {
		t.Fatal(status)
	}
	if strings.Contains(out.String(), "\r") {
		t.Fatalf("non-PTY output contains CR: %q", out.String())
	}
}
