package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

type pausedReadConn struct {
	net.Conn
	paused atomic.Bool
	resume chan struct{}
	once   sync.Once
}

func (c *pausedReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.paused.Load() {
		<-c.resume
	}
	return n, err
}

func (c *pausedReadConn) Close() error {
	c.once.Do(func() { close(c.resume) })
	return c.Conn.Close()
}

func TestPipelinedRequestsDoNotStrandShutdown(t *testing.T) {
	finish := make(chan struct{})
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, _ string, _ core.Streams) int {
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return 0
	}))
	addr, cancel, stopped := startServer(t, cfg)
	raw, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn := &pausedReadConn{Conn: raw, resume: make(chan struct{})}
	t.Cleanup(func() { conn.Close() })
	clientConn, channels, requests, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User: "alice", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(clientConn, channels, requests)
	t.Cleanup(func() { client.Close() })
	session := newSession(t, client)
	if _, err := session.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := session.Start("quick"); err != nil {
		t.Fatal(err)
	}
	// Hold close acknowledgements so requests can still be pipelined after the command exits.
	conn.paused.Store(true)
	sent := make(chan int, 1)
	go func() {
		for i := 0; i < 4096; i++ {
			if i == 128 {
				close(finish)
			}
			if _, err := session.SendRequest("ignored-after-completion", false, nil); err != nil {
				sent <- i
				return
			}
		}
		sent <- 4096
	}()
	select {
	case count := <-sent:
		if count < 256 {
			t.Fatalf("only %d requests were pipelined", count)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pipelined request sender stalled")
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request dispatch stranded server shutdown")
	}
}

func waitPrompt(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		var tail string
		for {
			b, err := reader.ReadByte()
			if err != nil {
				done <- err
				return
			}
			tail += string(b)
			if len(tail) > len("incus> ") {
				tail = tail[1:]
			}
			if tail == "incus> " {
				done <- nil
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("management prompt did not arrive")
	}
}

func openREPL(t *testing.T, client *ssh.Client) (*ssh.Session, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	session := newSession(t, client)
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	waitPrompt(t, reader)
	return session, stdin, reader
}

func writeCommand(t *testing.T, stdin io.Writer, command string) {
	t.Helper()
	if _, err := io.WriteString(stdin, command+"\n"); err != nil {
		t.Fatal(err)
	}
}

func TestREPLNextGuestUsesConsumedWindowSize(t *testing.T) {
	started := make(chan core.Window, 2)
	resized := make(chan core.Window, 1)
	finish := make(chan struct{})
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, command string, streams core.Streams) int {
		started <- streams.Window
		if command == "ssh first" {
			select {
			case window := <-streams.Resize:
				resized <- window
			case <-ctx.Done():
				return 1
			}
			select {
			case <-finish:
			case <-ctx.Done():
				return 1
			}
		}
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	session, stdin, output := openREPL(t, dial(t, addr, host, key))
	writeCommand(t, stdin, "ssh first")
	select {
	case window := <-started:
		if window != (core.Window{Width: 80, Height: 24}) {
			t.Fatalf("first window=%v", window)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first guest did not start")
	}
	payload := ssh.Marshal(struct{ Width, Height, PixelWidth, PixelHeight uint32 }{120, 40, 0, 0})
	if ok, err := session.SendRequest("window-change", true, payload); err != nil || !ok {
		t.Fatalf("resize accepted=%v err=%v", ok, err)
	}
	select {
	case window := <-resized:
		if window != (core.Window{Width: 120, Height: 40}) {
			t.Fatalf("resize=%v", window)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first guest did not consume resize")
	}
	close(finish)
	waitPrompt(t, output)
	writeCommand(t, stdin, "ssh second")
	select {
	case window := <-started:
		if window != (core.Window{Width: 120, Height: 40}) {
			t.Fatalf("second window=%v", window)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second guest did not start")
	}
	waitPrompt(t, output)
	writeCommand(t, stdin, "exit")
	io.Copy(io.Discard, output)
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestREPLSignalsBelongOnlyToActiveCommand(t *testing.T) {
	started := make(chan core.Streams, 2)
	finishFirst := make(chan struct{})
	received := make(chan string, 1)
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, command string, streams core.Streams) int {
		started <- streams
		if command == "ssh first" {
			select {
			case <-finishFirst:
			case <-ctx.Done():
			}
		} else {
			select {
			case signal := <-streams.Signals:
				received <- signal
			case <-ctx.Done():
			}
		}
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	session, stdin, output := openREPL(t, dial(t, addr, host, key))
	signal := func(name string, want bool) {
		t.Helper()
		ok, err := session.SendRequest("signal", true, ssh.Marshal(struct{ Signal string }{name}))
		if err != nil || ok != want {
			t.Fatalf("signal %s accepted=%v want=%v err=%v", name, ok, want, err)
		}
	}
	signal("TERM", false)
	writeCommand(t, stdin, "ssh first")
	var first core.Streams
	select {
	case first = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first guest did not start")
	}
	signal("USR1", true)
	close(finishFirst)
	waitPrompt(t, output)
	select {
	case stale := <-first.Signals:
		t.Fatalf("finished command retained signal %q", stale)
	default:
	}
	signal("TERM", false)
	writeCommand(t, stdin, "ssh second")
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("second guest did not start")
	}
	signal("INT", true)
	select {
	case got := <-received:
		if got != "INT" {
			t.Fatalf("next guest received stale signal %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active signal was not delivered")
	}
	waitPrompt(t, output)
	writeCommand(t, stdin, "exit")
	io.Copy(io.Discard, output)
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestAbortClosesOnlyCurrentSession(t *testing.T) {
	aborted := make(chan error, 1)
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, command string, streams core.Streams) int {
		if command == "abort" {
			if streams.Abort == nil {
				aborted <- errors.New("missing Abort hook")
				return 1
			}
			streams.Abort()
			aborted <- ctx.Err()
			return 0
		}
		io.WriteString(streams.Out, "still connected")
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	client := dial(t, addr, host, key)
	other := dial(t, addr, host, key)
	session := newSession(t, client)
	if err := session.Start("abort"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-aborted:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("abort context=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("abort did not cancel handler")
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("aborted session reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("aborted session did not close")
	}
	for _, live := range []*ssh.Client{client, other} {
		output, err := newSession(t, live).Output("help")
		if err != nil || strings.TrimSpace(string(output)) != "still connected" {
			t.Fatalf("unrelated session output=%q err=%v", output, err)
		}
	}
}
