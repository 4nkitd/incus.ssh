package gateway

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

const (
	histPrompt = "\x1b[36mincus> \x1b[0m"
	escUp      = "\x1b[A"
	escDown    = "\x1b[B"
	ss3Up      = "\x1bOA"
	ss3Down    = "\x1bOB"
)

// historySession opens a real SSH PTY REPL, feeds stdin, and returns its output.
func historySession(t *testing.T, handler core.Handler, stdin string) string {
	t.Helper()
	cfg, host, key := testConfig(t, handler)
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	session.Stdin = strings.NewReader(stdin)
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
	return output.String()
}

func collectCommands(t *testing.T, commands <-chan string) []string {
	t.Helper()
	var got []string
	for {
		select {
		case command := <-commands:
			got = append(got, command)
		default:
			return got
		}
	}
}

func TestHistoryUpDownDraftAndBounds(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	commands := make(chan string, historyLimit+16)
	handler := handlerFunc(func(_ context.Context, _ core.Principal, command string, _ core.Streams) int {
		commands <- command
		return 0
	})
	var stdin strings.Builder
	for i := 0; i < historyLimit+5; i++ {
		stdin.WriteString("cmd" + strconv.Itoa(i) + "\r")
	}
	stdin.WriteString("draft")
	for i := 0; i < historyLimit+1; i++ {
		if i%2 == 0 {
			stdin.WriteString(escUp)
		} else {
			stdin.WriteString(ss3Up)
		}
	}
	for i := 0; i < historyLimit+1; i++ {
		if i%2 == 0 {
			stdin.WriteString(escDown)
		} else {
			stdin.WriteString(ss3Down)
		}
	}
	stdin.WriteString("\rexit\r")
	output := historySession(t, handler, stdin.String())

	oldest := "cmd" + strconv.Itoa(5)
	if !strings.Contains(output, "\r\x1b[2K"+histPrompt+oldest) {
		t.Fatalf("history bound did not stop at %q: %q", oldest, output)
	}
	if strings.Contains(output, "\r\x1b[2K"+histPrompt+"cmd0") {
		t.Fatalf("history kept entry beyond the bound: %q", output)
	}
	if !strings.Contains(output, "\r\x1b[2K"+histPrompt+"draft") {
		t.Fatalf("draft was not restored past the newest entry: %q", output)
	}
	got := collectCommands(t, commands)
	if len(got) != historyLimit+6 {
		t.Fatalf("dispatched %d commands, want %d", len(got), historyLimit+6)
	}
	if got[0] != "cmd0" || got[len(got)-2] != "cmd"+strconv.Itoa(historyLimit+4) || got[len(got)-1] != "draft" {
		t.Fatalf("dispatched commands = %q", got)
	}
}

func TestHistoryIsSessionOnly(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	commands := make(chan string, 4)
	handler := handlerFunc(func(_ context.Context, _ core.Principal, command string, _ core.Streams) int {
		commands <- command
		return 0
	})
	cfg, host, key := testConfig(t, handler)
	addr, _, _ := startServer(t, cfg)
	client := dial(t, addr, host, key)
	for _, stdin := range []string{"alpha\rexit\r", escUp + "\rexit\r"} {
		session := newSession(t, client)
		session.Stdin = strings.NewReader(stdin)
		if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
			t.Fatal(err)
		}
		if err := session.Shell(); err != nil {
			t.Fatal(err)
		}
		if err := session.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	got := collectCommands(t, commands)
	if len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("history leaked between sessions: %q", got)
	}
}

func TestCtrlLClearsAndPreservesDraft(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	commands := make(chan string, 2)
	handler := handlerFunc(func(_ context.Context, _ core.Principal, command string, _ core.Streams) int {
		commands <- command
		return 0
	})
	output := historySession(t, handler, "draft\x0c\rexit\r")
	if !strings.Contains(output, "\x1b[H\x1b[2J"+histPrompt+"draft") {
		t.Fatalf("Ctrl-L did not redraw the prompt and draft: %q", output)
	}
	got := collectCommands(t, commands)
	if len(got) != 1 || got[0] != "draft" {
		t.Fatalf("Ctrl-L lost the draft: %q", got)
	}
}

func TestUnsupportedCSIStaysOutOfCommandLine(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	commands := make(chan string, 2)
	handler := handlerFunc(func(_ context.Context, _ core.Principal, command string, _ core.Streams) int {
		commands <- command
		return 0
	})
	output := historySession(t, handler, "hi\x1b[52;8u\x1b[1;5C\rexit\r")
	got := collectCommands(t, commands)
	if len(got) != 1 || got[0] != "hi" {
		t.Fatalf("CSI bytes leaked into the command line: %q", got)
	}
	if strings.Contains(output, "52;8u") || strings.Contains(output, "1;5C") {
		t.Fatalf("CSI bytes were echoed: %q", output)
	}
}

func TestBackspaceEditsUTF8(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	commands := make(chan string, 2)
	handler := handlerFunc(func(_ context.Context, _ core.Principal, command string, _ core.Streams) int {
		commands <- command
		return 0
	})
	output := historySession(t, handler, "héllo\x7f!\rexit\r")
	got := collectCommands(t, commands)
	if len(got) != 1 || got[0] != "héll!" {
		t.Fatalf("UTF-8 backspace produced %q", got)
	}
	if !strings.Contains(output, "\b \b") {
		t.Fatalf("backspace was not echoed: %q", output)
	}
}

func TestGuestStdinPassesThroughEditor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	commands := make(chan string, 4)
	guest := make(chan string, 1)
	handler := handlerFunc(func(_ context.Context, _ core.Principal, command string, streams core.Streams) int {
		commands <- command
		if command != "guest" {
			return 0
		}
		input := make([]byte, len(escUp))
		if _, err := io.ReadFull(streams.In, input); err != nil {
			t.Errorf("guest stdin: %v", err)
		}
		guest <- string(input)
		return 0
	})
	historySession(t, handler, "guest\r"+escUp+"exit\r")
	select {
	case got := <-guest:
		if got != escUp {
			t.Fatalf("guest stdin = %q, want raw %q", got, escUp)
		}
	default:
		t.Fatal("guest command did not run")
	}
	got := collectCommands(t, commands)
	if len(got) != 1 || got[0] != "guest" {
		t.Fatalf("editor stole guest input: %q", got)
	}
}
