package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

func TestPTYResizeSignalsAndSingleStart(t *testing.T) {
	ready := make(chan core.Streams, 1)
	release := make(chan struct{})
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, _ string, streams core.Streams) int {
		ready <- streams
		select {
		case <-release:
		case <-ctx.Done():
		}
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	pty := func(term string, width, height uint32, modes string) []byte {
		return ssh.Marshal(struct {
			Term                                   string
			Width, Height, PixelWidth, PixelHeight uint32
			Modes                                  string
		}{term, width, height, 0, 0, modes})
	}
	for _, payload := range [][]byte{
		pty("xterm;BAD=1", 80, 24, "\x00"),
		pty("xterm", 65536, 24, "\x00"),
		pty("xterm", 80, 65536, "\x00"),
		pty("xterm", 80, 24, "\x01"),
		{0, 1},
	} {
		if ok, err := session.SendRequest("pty-req", true, payload); err != nil || ok {
			t.Fatalf("invalid PTY accepted=%v err=%v", ok, err)
		}
	}
	if err := session.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		t.Fatal(err)
	}
	if ok, err := session.SendRequest("pty-req", true, pty("xterm", 80, 24, "\x00")); err != nil || ok {
		t.Fatalf("duplicate PTY accepted=%v err=%v", ok, err)
	}
	window := func(width, height uint32) []byte {
		return ssh.Marshal(struct{ Width, Height, PixelWidth, PixelHeight uint32 }{width, height, 0, 0})
	}
	if ok, err := session.SendRequest("window-change", true, window(100, 30)); err != nil || !ok {
		t.Fatalf("pre-start resize accepted=%v err=%v", ok, err)
	}
	if err := session.Start("ssh demo"); err != nil {
		t.Fatal(err)
	}
	var streams core.Streams
	select {
	case streams = <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	if !streams.PTY || streams.Term != "xterm-256color" || streams.Window != (core.Window{Width: 100, Height: 30}) {
		t.Fatalf("PTY streams = %#v", streams)
	}
	for _, request := range []struct {
		kind    string
		payload []byte
	}{
		{"shell", nil},
		{"exec", ssh.Marshal(struct{ Command string }{"help"})},
		{"pty-req", pty("xterm", 80, 24, "\x00")},
		{"window-change", window(65536, 10)},
		{"signal", ssh.Marshal(struct{ Signal string }{"SIGTERM"})},
	} {
		if ok, err := session.SendRequest(request.kind, true, request.payload); err != nil || ok {
			t.Fatalf("invalid request %s accepted=%v err=%v", request.kind, ok, err)
		}
	}
	for i := uint32(1); i <= 50; i++ {
		if ok, err := session.SendRequest("window-change", true, window(100+i, 40)); err != nil || !ok {
			t.Fatalf("resize accepted=%v err=%v", ok, err)
		}
	}
	for _, signal := range []string{"ABRT", "ALRM", "FPE", "ILL", "KILL", "PIPE", "SEGV", "WINCH", "TSTP", "CONT", "STOP"} {
		if ok, err := session.SendRequest("signal", true, ssh.Marshal(struct{ Signal string }{signal})); err != nil || ok {
			t.Fatalf("unsupported signal %s accepted=%v err=%v", signal, ok, err)
		}
	}
	for _, signal := range []string{"HUP", "INT", "QUIT", "TERM", "USR1", "USR2"} {
		if ok, err := session.SendRequest("signal", true, ssh.Marshal(struct{ Signal string }{signal})); err != nil || !ok {
			t.Fatalf("supported signal %s accepted=%v err=%v", signal, ok, err)
		}
		select {
		case got := <-streams.Signals:
			if got != signal {
				t.Fatalf("signal=%q want=%q", got, signal)
			}
		case <-time.After(time.Second):
			t.Fatalf("signal %s was not delivered", signal)
		}
	}
	select {
	case latest := <-streams.Resize:
		if latest != (core.Window{Width: 150, Height: 40}) {
			t.Fatalf("resize = %#v", latest)
		}
	case <-time.After(time.Second):
		t.Fatal("no resize delivered")
	}
	for i := 0; i < 20; i++ {
		ok, err := session.SendRequest("signal", true, ssh.Marshal(struct{ Signal string }{"INT"}))
		if err != nil || ok != (i < 8) {
			t.Fatalf("signal %d accepted=%v err=%v", i, ok, err)
		}
	}
	for i := 0; i < 8; i++ {
		select {
		case signal := <-streams.Signals:
			if signal != "INT" {
				t.Fatalf("signal = %q", signal)
			}
		case <-time.After(time.Second):
			t.Fatal("no signal delivered")
		}
	}
	close(release)
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestExecCommandLength(t *testing.T) {
	cfg, host, key := testConfig(t, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	if ok, err := session.SendRequest("exec", true, ssh.Marshal(struct{ Command string }{strings.Repeat("x", maxCommandLength+1)})); err != nil || ok {
		t.Fatalf("oversize exec accepted=%v err=%v", ok, err)
	}
	if err := session.Run(strings.Repeat("x", maxCommandLength)); err != nil {
		t.Fatal(err)
	}
}

func TestREPLUsesLFWithoutPTY(t *testing.T) {
	out, errOut := &strings.Builder{}, &strings.Builder{}
	streams := core.Streams{In: newSessionInput(context.Background(), strings.NewReader("")), Out: out, Err: errOut}
	status := repl(context.Background(), core.Principal{Name: "demo", Project: "demo"}, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }), streams)
	if status != 0 {
		t.Fatalf("status=%d stderr=%q", status, errOut.String())
	}
	if strings.Contains(out.String(), "\r") {
		t.Fatalf("non-PTY REPL emitted CR: %q", out.String())
	}
	if !strings.Contains(out.String(), "exit to disconnect.\n") {
		t.Fatalf("missing banner: %q", out.String())
	}
}

func TestREPLUsesCRLFWithPTY(t *testing.T) {
	out, _ := &strings.Builder{}, &strings.Builder{}
	streams := core.Streams{In: newSessionInput(context.Background(), strings.NewReader("")), Out: out, PTY: true}
	status := repl(context.Background(), core.Principal{Name: "demo", Project: "demo"}, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }), streams)
	if status != 0 {
		t.Fatalf("status=%d", status)
	}
	got := out.String()
	if !strings.Contains(got, "exit to disconnect.\r\n") || !strings.Contains(got, "arrow keys are unsupported.\r\n") {
		t.Fatalf("missing CRLF banner: %q", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("PTY REPL emitted bare LF: %q", got)
	}
}

func TestREPLPreservesBufferedContainerInput(t *testing.T) {
	for _, tc := range []struct{ name, eol string }{{"LF", "\n"}, {"CR", "\r"}, {"CRLF", "\r\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			observed := make(chan string, 1)
			cfg, host, key := testConfig(t, handlerFunc(func(_ context.Context, _ core.Principal, command string, streams core.Streams) int {
				input := make([]byte, len("container input\n"))
				_, err := io.ReadFull(streams.In, input)
				if err != nil {
					t.Errorf("container stdin: %v", err)
				}
				observed <- command + ":" + string(input)
				io.WriteString(streams.Out, "container output\r\n")
				return 0
			}))
			addr, _, _ := startServer(t, cfg)
			session := newSession(t, dial(t, addr, host, key))
			var output bytes.Buffer
			session.Stdin = strings.NewReader("ssh demo" + tc.eol + "container input\nexit" + tc.eol)
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
			case got := <-observed:
				if got != "ssh demo:container input\n" {
					t.Fatalf("handler received %q", got)
				}
			case <-time.After(time.Second):
				t.Fatal("handler not called")
			}
			if !strings.Contains(output.String(), "container output") || strings.Count(output.String(), "ssh demo") != 1 || strings.Contains(output.String(), "container input") || strings.Count(output.String(), "incus> ") != 2 {
				t.Fatalf("REPL output = %q", output.String())
			}
		})
	}
}

func TestREPLSplitCRLFDoesNotStealContainerInput(t *testing.T) {
	input, writer := io.Pipe()
	defer input.Close()
	go func() {
		defer writer.Close()
		io.WriteString(writer, "ssh demo\r")
		io.WriteString(writer, "\ncontainer\r\ninput\nexit\r\n")
	}()
	const want = "container\r\ninput\n"
	called := false
	code := repl(context.Background(), core.Principal{}, handlerFunc(func(_ context.Context, _ core.Principal, command string, streams core.Streams) int {
		called = true
		input := make([]byte, len(want))
		if _, err := io.ReadFull(streams.In, input); err != nil {
			t.Error(err)
		}
		if command != "ssh demo" || string(input) != want {
			t.Errorf("command=%q input=%q", command, input)
		}
		return 0
	}), core.Streams{In: testSessionInput(t, input), Out: io.Discard, Err: io.Discard})
	if code != 0 || !called {
		t.Fatalf("code=%d called=%v", code, called)
	}
}

func TestREPLRejectsLongLines(t *testing.T) {
	called := false
	var stderr bytes.Buffer
	code := repl(context.Background(), core.Principal{}, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int {
		called = true
		return 0
	}), core.Streams{In: testSessionInput(t, strings.NewReader(strings.Repeat("x", maxCommandLength+1)+"\n")), Out: io.Discard, Err: &stderr})
	if code != 1 || called || !strings.Contains(stderr.String(), "command too long") {
		t.Fatalf("code=%d called=%v stderr=%q", code, called, stderr.String())
	}
}

func TestREPLCommandReadCancellationReturnsInputToPrompt(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	readResult := make(chan error, 1)
	commands := make(chan string, 2)
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, command string, streams core.Streams) int {
		commands <- command
		if command != "ssh demo" {
			return 0
		}
		reader, ok := streams.In.(core.ContextReader)
		if !ok {
			readResult <- errors.New("REPL stdin does not implement core.ContextReader")
			close(started)
			return 1
		}
		commandCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		copied := make(chan error, 1)
		go func() {
			close(started)
			n, err := reader.ReadContext(commandCtx, make([]byte, 4096))
			if n != 0 {
				copied <- errors.New("canceled command stole prompt input")
				return
			}
			copied <- err
		}()
		select {
		case <-finish:
		case <-ctx.Done():
		}
		cancel()
		readResult <- <-copied
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	session.Stdout = &output
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "ssh demo\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("command did not start")
	}
	close(finish)
	select {
	case err := <-readResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("command read = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command read did not cancel")
	}
	if _, err := io.WriteString(stdin, "\nhelp\rexit\r"); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("REPL failed to resume after canceled command")
	}
	for _, want := range []string{"ssh demo", "help"} {
		select {
		case got := <-commands:
			if got != want {
				t.Fatalf("command=%q want=%q", got, want)
			}
		default:
			t.Fatalf("missing command %q", want)
		}
	}
	if strings.Count(output.String(), "incus> ") != 3 {
		t.Fatalf("REPL output=%q", output.String())
	}
}

func TestPTYREPLEchoAndBackspaceStopAtContainerInput(t *testing.T) {
	commands := make(chan string, 2)
	cfg, host, key := testConfig(t, handlerFunc(func(_ context.Context, _ core.Principal, command string, streams core.Streams) int {
		commands <- command
		if command == "ssh demo" {
			input := make([]byte, len("PAYLOAD\r\n"))
			if _, err := io.ReadFull(streams.In, input); err != nil {
				t.Error(err)
			}
			streams.Out.Write(input)
		}
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	session.Stdin = strings.NewReader("helx\x7fp\rssh demx\bo\rPAYLOAD\r\nexit\r")
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
	for _, want := range []string{"help", "ssh demo"} {
		select {
		case got := <-commands:
			if got != want {
				t.Fatalf("command=%q want=%q", got, want)
			}
		default:
			t.Fatalf("missing command %q", want)
		}
	}
	got := output.String()
	if !strings.Contains(got, "helx\b \bp\r\n") || !strings.Contains(got, "ssh demx\b \bo\r\n") || strings.Count(got, "PAYLOAD") != 1 {
		t.Fatalf("PTY output=%q", got)
	}
}

func TestInputEOFAllowsRemainingOutputAndExitStatus(t *testing.T) {
	eof, finish := make(chan struct{}), make(chan struct{})
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, _ string, streams core.Streams) int {
		input, err := io.ReadAll(streams.In)
		if err != nil || string(input) != "input" {
			t.Errorf("stdin=%q err=%v", input, err)
		}
		close(eof)
		select {
		case <-ctx.Done():
			return 99
		case <-finish:
		}
		io.WriteString(streams.Out, "output after EOF")
		io.WriteString(streams.Err, "stderr after EOF")
		return 23
	}))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	session.Stdin = strings.NewReader("input")
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	if err := session.Start("ssh demo"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-eof:
	case <-time.After(3 * time.Second):
		t.Fatal("stdin EOF not delivered")
	}
	close(finish)
	var exit *ssh.ExitError
	if err := session.Wait(); !errors.As(err, &exit) || exit.ExitStatus() != 23 {
		t.Fatalf("exit=%v", err)
	}
	if stdout.String() != "output after EOF" || stderr.String() != "stderr after EOF" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
