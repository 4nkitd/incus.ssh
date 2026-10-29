package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/terminal"
)

var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(text string) string {
	return ansiPattern.ReplaceAllString(text, "")
}

func colorStreams() (core.Streams, *bytes.Buffer, *bytes.Buffer) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	return core.Streams{Out: out, Err: errOut, PTY: true, Term: "xterm"}, out, errOut
}

func TestHelpColorsUsageAfterWrapping(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	streams, out, errOut := colorStreams()
	if status := (Handler{}).Handle(context.Background(), principal, "help all", streams); status != 0 {
		t.Fatalf("status=%d stderr=%q", status, errOut.String())
	}
	plainOut := &bytes.Buffer{}
	plain := core.Streams{Out: plainOut}
	if status := (Handler{}).Handle(context.Background(), principal, "help all", plain); status != 0 {
		t.Fatal(status)
	}

	colored := out.String()
	unwrapped := strings.ReplaceAll(colored, "\r\n", "\n")
	if !strings.Contains(colored, terminal.Cyan) || !strings.Contains(colored, terminal.Reset) {
		t.Fatalf("help is not colored: %q", colored)
	}
	if !strings.Contains(colored, terminal.Bold+"Commands:") {
		t.Fatalf("heading is not bold: %q", colored)
	}
	if !strings.Contains(colored, terminal.Cyan+wrapHelp(commands[0].Usage, "  ", "      ", helpWidth)) {
		t.Fatalf("usage is not cyan: %q", colored)
	}
	if !strings.Contains(unwrapped, terminal.Yellow+wrapHelp(unsupportedDescription, "", "", helpWidth)) {
		t.Fatalf("unsupported notice is not amber: %q", colored)
	}
	stripped := stripANSI(unwrapped)
	if stripped != plainOut.String() {
		t.Fatalf("coloring changed layout:\ncolored=%q\nplain=%q", stripped, plainOut.String())
	}
	assertHelpText(t, stripped)
	for i, line := range strings.Split(strings.TrimSuffix(stripped, "\n"), "\n") {
		if len(line) > helpWidth {
			t.Fatalf("line %d is %d columns after stripping ANSI: %q", i+1, len(line), line)
		}
	}
}

func TestJSONHasNoANSIOnPTY(t *testing.T) {
	streams, out, errOut := colorStreams()
	cases := []struct {
		line  string
		h     Handler
		fails bool
	}{
		{"help --json", Handler{}, false},
		{"whoami --json", Handler{}, false},
		{"ls --json", Handler{Backend: &fakeBackend{instances: []core.Instance{{Name: "demo", Status: "Running"}}}}, false},
		{"ls --json", Handler{Backend: &fakeBackend{listError: errors.New("refused")}}, true},
		{"image list --json", Handler{Backend: &fakeBackend{images: []core.ImageInfo{{Fingerprint: "0123456789abcdef", Aliases: []string{"ubuntu/24.04"}, OS: "Ubuntu", Release: "24.04"}}}}, false},
		{"image list --json", Handler{Backend: &fakeBackend{imagesError: errors.New("refused")}}, true},
		{"new --json", Handler{Backend: &fakeBackend{createError: errors.New("refused")}}, true},
		{"rm --json good bad", Handler{Backend: &fakeBackend{removeError: map[string]error{"bad": errors.New("refused")}}}, true},
	}
	for _, tc := range cases {
		out.Reset()
		errOut.Reset()
		status := tc.h.Handle(context.Background(), principal, tc.line, streams)
		if (status != 0) != tc.fails {
			t.Fatalf("%s: status=%d stderr=%q", tc.line, status, errOut.String())
		}
		if strings.Contains(out.String(), "\x1b") || strings.Contains(errOut.String(), "\x1b") {
			t.Fatalf("%s emitted an ANSI escape: out=%q stderr=%q", tc.line, out.String(), errOut.String())
		}
		var decoded map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(bytes.ReplaceAll(out.Bytes(), []byte("\r\n"), []byte("\n"))), &decoded); err != nil {
			t.Fatalf("%s: invalid JSON: %v", tc.line, err)
		}
	}
}

func TestNonPTYHasNoANSI(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	streams := core.Streams{Out: out, Err: errOut, Term: "xterm"}
	h := Handler{Backend: &fakeBackend{instances: []core.Instance{{Name: "demo", Status: "Running"}}, images: []core.ImageInfo{{Fingerprint: "0123456789abcdef", Aliases: []string{"ubuntu/24.04"}, OS: "Ubuntu", Release: "24.04"}}}}
	for _, line := range []string{"help", "ls", "image list", "start demo", "stop demo", "rm demo", "ls missing"} {
		out.Reset()
		errOut.Reset()
		h.Handle(context.Background(), principal, line, streams)
		if strings.Contains(out.String()+errOut.String(), "\x1b") {
			t.Fatalf("%s colored non-PTY output: out=%q stderr=%q", line, out.String(), errOut.String())
		}
	}
}

func TestGuestOutputKeepsBytesOnColorTerminal(t *testing.T) {
	streams, out, _ := colorStreams()
	status := (Handler{Backend: execWriter{&fakeBackend{}}}).Handle(context.Background(), principal, "ssh demo", streams)
	if status != 0 {
		t.Fatalf("status=%d", status)
	}
	if out.String() != "a\nb\n" {
		t.Fatalf("guest output was rewritten: %q", out.String())
	}
}

func TestHumanStatusAndLifecycleColors(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	streams, out, errOut := colorStreams()
	h := Handler{Backend: &fakeBackend{
		instances: []core.Instance{{Name: "up", Status: "Running"}, {Name: "down", Status: "Stopped"}, {Name: "odd", Status: "Frozen"}},
	}}
	if status := h.Handle(context.Background(), principal, "ls", streams); status != 0 {
		t.Fatalf("status=%d stderr=%q", status, errOut.String())
	}
	want := terminal.Style(streams, terminal.Bold+terminal.Cyan, "up") + "\t" + terminal.Green + "Running" + terminal.Reset + "\r\n" +
		terminal.Style(streams, terminal.Bold+terminal.Cyan, "down") + "\t" + terminal.Yellow + "Stopped" + terminal.Reset + "\r\n" +
		terminal.Style(streams, terminal.Bold+terminal.Cyan, "odd") + "\t" + terminal.Yellow + "Frozen" + terminal.Reset + "\r\n"
	if out.String() != want {
		t.Fatalf("ls output=%q want=%q", out.String(), want)
	}
	assertCRLF(t, out.String())

	for _, tc := range []struct {
		line string
		want string
	}{
		{"start demo", terminal.Green + "started" + terminal.Reset + " demo\r\n"},
		{"stop demo", terminal.Yellow + "stopped" + terminal.Reset + " demo\r\n"},
		{"rm demo", terminal.Yellow + "removed demo" + terminal.Reset + "\r\n"},
		{"new --name=demo", "demo\t" + terminal.Green + "Running" + terminal.Reset + "\r\n"},
	} {
		out.Reset()
		errOut.Reset()
		h := Handler{Backend: &fakeBackend{created: core.Instance{Name: "demo", Status: "Running"}}}
		if status := h.Handle(context.Background(), principal, tc.line, streams); status != 0 {
			t.Fatalf("%s: status=%d stderr=%q", tc.line, status, errOut.String())
		}
		if out.String() != tc.want {
			t.Fatalf("%s: output=%q want=%q", tc.line, out.String(), tc.want)
		}
	}
}

func TestLifecycleFailureIsRedOnPTY(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	streams, out, errOut := colorStreams()
	h := Handler{Backend: &fakeBackend{startError: errors.New("refused")}}
	if status := h.Handle(context.Background(), principal, "start demo", streams); status == 0 {
		t.Fatal("expected failure")
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected stdout: %q", out.String())
	}
	want := "error: " + terminal.Red + "refused" + terminal.Reset + "\r\n"
	if errOut.String() != want {
		t.Fatalf("stderr=%q want=%q", errOut.String(), want)
	}
}
