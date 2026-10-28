package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

func testSessionInput(t *testing.T, source io.Reader) *sessionInput {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	input := newSessionInput(ctx, source)
	t.Cleanup(func() {
		cancel()
		if closer, ok := source.(io.Closer); ok {
			closer.Close()
		}
		select {
		case <-input.done:
		case <-time.After(time.Second):
			t.Error("input pump did not stop")
		}
	})
	return input
}

type countedInput struct {
	io.Reader
	count atomic.Int64
}

func (r *countedInput) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.count.Add(int64(n))
	return n, err
}

func TestInputPumpBoundsReadAhead(t *testing.T) {
	source := &countedInput{Reader: strings.NewReader(strings.Repeat("x", maxInputBuffer*4))}
	input := testSessionInput(t, source)
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		input.mu.Lock()
		buffered, changed := len(input.buffer), input.changed
		input.mu.Unlock()
		if buffered == maxInputBuffer {
			break
		}
		select {
		case <-changed:
		case <-timeout.C:
			t.Fatal("input pump did not fill its bounded buffer")
		}
	}
	if got := source.count.Load(); got != maxInputBuffer {
		t.Fatalf("read-ahead=%d want=%d", got, maxInputBuffer)
	}
}

func TestCanceledReadPreservesBufferedInputAndEOF(t *testing.T) {
	want := strings.Repeat("buffered input\n", maxInputBuffer)
	input := testSessionInput(t, strings.NewReader(want))
	command, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := input.ReadContext(command, make([]byte, 4096)); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %d, %v", n, err)
	}
	got, err := io.ReadAll(input)
	if err != nil || string(got) != want {
		t.Fatalf("remaining input length=%d want=%d err=%v", len(got), len(want), err)
	}
	if n, err := input.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("repeated EOF = %d, %v", n, err)
	}
	select {
	case <-input.done:
	case <-time.After(time.Second):
		t.Fatal("input pump did not stop at EOF")
	}
}

func TestCanceledPendingReadLeavesSingleInputConsumer(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	source, writer := io.Pipe()
	input := newSessionInput(ctx, source)
	defer func() {
		stop()
		source.Close()
		writer.Close()
		select {
		case <-input.done:
		case <-time.After(time.Second):
			t.Error("input pump did not stop")
		}
	}()
	command, cancel := context.WithCancel(ctx)
	reader := newREPLReader(ctx, input)
	reader.skipLF = true
	result := make(chan error, 1)
	go func() {
		n, err := reader.ReadContext(command, make([]byte, 1024))
		if n != 0 {
			result <- errors.New("canceled read consumed input")
			return
		}
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("command read = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("command read did not cancel")
	}
	go func() {
		io.WriteString(writer, "\nhelp\n")
		writer.Close()
	}()
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != "help\n" {
		t.Fatalf("remaining input = %q, %v", got, err)
	}
}

type stalledInputChannel struct {
	source  io.Reader
	started chan struct{}
	once    sync.Once
}

func (c *stalledInputChannel) Read(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.source.Read(p)
}

func (c *stalledInputChannel) Write(p []byte) (int, error)                    { return len(p), nil }
func (c *stalledInputChannel) Close() error                                   { return nil }
func (c *stalledInputChannel) CloseWrite() error                              { return nil }
func (c *stalledInputChannel) SendRequest(string, bool, []byte) (bool, error) { return true, nil }
func (c *stalledInputChannel) Stderr() io.ReadWriter {
	return struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), io.Discard}
}

func TestSessionTeardownStopsPumpWhenPeerIgnoresChannelClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source, writer := io.Pipe()
	channel := &stalledInputChannel{source: source, started: make(chan struct{})}
	requests := make(chan *ssh.Request)
	closedConnection, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		serveSession(ctx, channel, requests, core.Principal{}, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }), func() error {
			close(closedConnection)
			return source.Close()
		})
	}()
	t.Cleanup(func() {
		cancel()
		source.Close()
		writer.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("session teardown did not finish")
		}
	})
	select {
	case <-channel.started:
	case <-time.After(time.Second):
		t.Fatal("input pump did not start")
	}
	close(requests)
	select {
	case <-closedConnection:
	case <-time.After(3 * time.Second):
		t.Fatal("stalled connection was not closed")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked input pump survived teardown")
	}
}
