package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

const (
	ansiReset   = "\x1b[0m"
	ansiCyan    = "\x1b[36m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	clearScreen = "\x1b[2J\x1b[H"
)

func TestSSHColorHelpOnlyOnInteractiveTerminal(t *testing.T) {
	clearNoColor(t)
	s := startGateway(t)
	client := s.connect(t, alice)

	ptyHelp := runPTY(t, client, "help", nil, 0)
	if !strings.Contains(ptyHelp.out, ansiCyan) || !strings.Contains(ptyHelp.out, ansiReset) {
		t.Fatalf("xterm PTY help lacks cyan styling: %q", ptyHelp.out)
	}
	if !strings.Contains(ptyHelp.out, "help [all|COMMAND]") {
		t.Fatalf("xterm PTY help lost its usage text: %q", ptyHelp.out)
	}
	if ptyHelp.err != "" {
		t.Fatalf("xterm PTY help stderr: %q", ptyHelp.err)
	}

	plainHelp := run(t, client, "help", nil, 0)
	assertNoANSI(t, plainHelp.out)
	if !strings.Contains(plainHelp.out, "help [all|COMMAND]") {
		t.Fatalf("plain help lost its usage text: %q", plainHelp.out)
	}

	dumbHelp := runTerm(t, client, "help", "dumb", 0)
	assertNoANSI(t, dumbHelp.out)
	if !strings.Contains(dumbHelp.out, "help [all|COMMAND]") {
		t.Fatalf("TERM=dumb PTY help lost its usage text: %q", dumbHelp.out)
	}
}

func TestSSHColorJSONStaysRaw(t *testing.T) {
	clearNoColor(t)
	s := startGateway(t)
	client := s.connect(t, alice)

	var help struct {
		Commands []struct {
			Usage string `json:"usage"`
		} `json:"commands"`
	}
	assertRawJSON(t, runPTY(t, client, "help --json", nil, 0).out, &help)
	if len(help.Commands) != 12 {
		t.Fatalf("help JSON lists %d commands, want 12 including image and clear: %+v", len(help.Commands), help.Commands)
	}
	found, hasClear, hasImage := false, false, false
	for _, command := range help.Commands {
		found = found || strings.Contains(command.Usage, "help")
		hasClear = hasClear || strings.HasPrefix(strings.TrimSpace(command.Usage), "clear")
		hasImage = hasImage || strings.HasPrefix(strings.TrimSpace(command.Usage), "image ")
	}
	if !found {
		t.Fatalf("PTY help JSON lost its command usage: %+v", help.Commands)
	}
	if !hasClear {
		t.Fatalf("PTY help JSON omits the clear command: %+v", help.Commands)
	}
	if !hasImage {
		t.Fatalf("PTY help JSON omits the image command: %+v", help.Commands)
	}

	var whoami core.Principal
	assertRawJSON(t, runPTY(t, client, "whoami --json", nil, 0).out, &whoami)
	if whoami != alice {
		t.Fatalf("PTY whoami JSON = %+v, want %+v", whoami, alice)
	}

	run(t, client, "new --name=demo --json", nil, 0)
	var listed struct {
		VMs []map[string]json.RawMessage `json:"vms"`
	}
	assertRawJSON(t, runPTY(t, client, "ls --json", nil, 0).out, &listed)
	if len(listed.VMs) != 1 {
		t.Fatalf("PTY ls JSON = %+v, want one instance", listed.VMs)
	}
}

func TestSSHColorListShowsStatus(t *testing.T) {
	clearNoColor(t)
	s := startGateway(t)
	client := s.connect(t, alice)
	run(t, client, "new --name=demo --json", nil, 0)

	running := runPTY(t, client, "ls", nil, 0).out
	assertStatusColor(t, running, "Running", ansiGreen)
	if strings.Contains(running, ansiYellow) {
		t.Fatalf("running listing used the amber status style: %q", running)
	}

	run(t, client, "stop demo --json", nil, 0)
	stopped := runPTY(t, client, "ls", nil, 0).out
	assertStatusColor(t, stopped, "Stopped", ansiYellow)
	if strings.Contains(stopped, ansiGreen) {
		t.Fatalf("stopped listing used the green status style: %q", stopped)
	}

	assertNoANSI(t, run(t, client, "ls", nil, 0).out)
}

func TestSSHColorGuestOutputUnchanged(t *testing.T) {
	clearNoColor(t)
	s := startGateway(t)
	client := s.connect(t, alice)
	run(t, client, "new --name=demo --json", nil, 0)

	guest := runPTY(t, client, "ssh demo "+binaryCommand, nil, 37)
	wantOut := []byte{'o', 'u', 't', 0, 255}
	if !bytes.Equal([]byte(guest.out), wantOut) {
		t.Fatalf("PTY guest stdout changed: got %d bytes %v, want %v", len(guest.out), []byte(guest.out), wantOut)
	}
	if strings.Contains(guest.out, "\x1b") || strings.Contains(guest.err, "\x1b") {
		t.Fatalf("guest streams gained escape bytes: stdout=%q stderr=%q", guest.out, guest.err)
	}
	if guest.err != "err\x00\xfe" {
		t.Fatalf("PTY guest stderr changed: %q", guest.err)
	}
}

func TestSSHColorClearCommand(t *testing.T) {
	clearNoColor(t)
	s := startGateway(t)
	client := s.connect(t, alice)
	before := s.backend.callCount()

	cleared := runPTY(t, client, "clear", nil, 0)
	if cleared.out != clearScreen || cleared.err != "" {
		t.Fatalf("xterm PTY clear = %q stderr %q; want exactly %q", cleared.out, cleared.err, clearScreen)
	}

	plain := run(t, client, "clear", nil, 1)
	if plain.out != "" || plain.err == "" {
		t.Fatalf("non-PTY clear = %+v; want status 1 and a diagnostic on stderr", plain)
	}
	assertNoANSI(t, plain.err)

	dumb := runTerm(t, client, "clear", "dumb", 1)
	if dumb.out != "" || dumb.err == "" {
		t.Fatalf("TERM=dumb clear = %+v; want status 1 and a diagnostic on stderr", dumb)
	}
	assertNoANSI(t, dumb.err)

	var rejected struct {
		Error string `json:"error"`
	}
	jsonClear := runPTY(t, client, "clear --json", nil, 1)
	assertRawJSON(t, jsonClear.out, &rejected)
	if rejected.Error == "" || jsonClear.err != "" {
		t.Fatalf("clear --json = %+v; want a JSON error on stdout only", jsonClear)
	}

	if got := s.backend.callCount(); got != before {
		t.Fatalf("clear reached the Incus backend: calls %d -> %d", before, got)
	}
}

// clearNoColor pins the documented default: only a nonempty NO_COLOR disables
// styling, so tests run against the same color decision a typical operator sees.
func clearNoColor(t *testing.T) {
	t.Helper()
	value, had := os.LookupEnv("NO_COLOR")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv("NO_COLOR", value)
		}
	})
}

func runTerm(t *testing.T, client *ssh.Client, line, term string, wantStatus int) response {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	if err := session.RequestPty(term, 24, 80, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		t.Fatalf("%q TERM=%s request PTY: %v", line, term, err)
	}
	status := 0
	if err := session.Run(line); err != nil {
		var exit *ssh.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%q TERM=%s transport error: %v", line, term, err)
		}
		status = exit.ExitStatus()
	}
	if status != wantStatus {
		t.Fatalf("%q TERM=%s exit=%d want=%d stdout=%q stderr=%q", line, term, status, wantStatus, stdout.String(), stderr.String())
	}
	return response{stdout.String(), stderr.String()}
}

func assertNoANSI(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, "\x1b") {
		t.Fatalf("plain output contains an escape byte: %q", text)
	}
}

func assertRawJSON(t *testing.T, out string, target any) {
	t.Helper()
	assertNoANSI(t, out)
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		t.Fatalf("JSON output has extra decorations: %q", out)
	}
	if err := json.Unmarshal([]byte(out), target); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
}

func assertStatusColor(t *testing.T, text, status, code string) {
	t.Helper()
	if !strings.Contains(text, code+status+ansiReset) {
		t.Fatalf("%s status is not styled with %q: %q", status, code, text)
	}
}
