package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"incus-for-devs/internal/core"
)

func TestExecStreamingExitCodeAndShellBoundary(t *testing.T) {
	payload := []byte{0, 1, 2, '\n', '\r', 255}
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Echo: true, Exit: 17, Stderr: []byte("container stderr")})
	var stdout, stderr bytes.Buffer
	marker := filepath.Join(f.dir, "must-not-exist")
	command := "printf hello; touch " + marker + "; $(id)\n--project other"
	code, err := f.backend.Exec(context.Background(), alice, "mine", command, core.Streams{In: bytes.NewReader(payload), Out: &stdout, Err: &stderr})
	if err != nil || code != 17 {
		t.Fatalf("Exec = %d, %v", code, err)
	}
	if !bytes.Equal(stdout.Bytes(), payload) || stderr.String() != "container stderr" {
		t.Fatalf("streams changed: stdout %v stderr %q", stdout.Bytes(), stderr.String())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("container shell command ran on host")
	}
	calls := f.calls(t)
	want := []string{"exec", "mine", "--mode=non-interactive", "--env", "TERM=dumb", "--", "/bin/sh", "-lc", command}
	if got := calls[len(calls)-1][4:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("exec args = %q, want %q", got, want)
	}
}

func TestExecDefaultShellAndMinimalEnvironment(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/secret/ssh-agent")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "do-not-forward")
	t.Setenv("INCUS_REMOTE", "untrusted")
	t.Setenv("INCUS_PROJECT", "other")
	t.Setenv("TERM", "host-secret-terminal")
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{})
	code, err := f.backend.Exec(context.Background(), alice, "mine", "", core.Streams{})
	if err != nil || code != 0 {
		t.Fatalf("Exec = %d, %v", code, err)
	}
	calls := f.calls(t)
	args := calls[len(calls)-1]
	if !reflect.DeepEqual(args[len(args)-3:], []string{"--", "/bin/sh", "-l"}) {
		t.Fatalf("default shell args = %q", args)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "env.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "INCUS_REMOTE", "INCUS_PROJECT", "host-secret-terminal"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Errorf("host environment leaked %s", forbidden)
		}
	}
}

func TestExecDoesNotWaitForInputEOFToReportExit(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Exit: 23})
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	code, err := f.backend.Exec(ctx, alice, "mine", "exit 23", core.Streams{In: reader})
	if err != nil || code != 23 {
		t.Fatalf("waiting on SSH stdin lost exit code: %d, %v", code, err)
	}
}

func TestPTYStreamsOutputAndPreservesExitCode(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Stdout: []byte("terminal output"), Stderr: []byte("terminal error"), Exit: 42})
	var output bytes.Buffer
	code, err := f.backend.Exec(context.Background(), alice, "mine", "exit 42", core.Streams{PTY: true, Out: &output})
	if err != nil || code != 42 {
		t.Fatalf("PTY exit = %d, %v", code, err)
	}
	if output.String() != "terminal outputterminal error" {
		t.Fatalf("PTY output = %q", output.String())
	}
}

type blockedOutput struct {
	entered chan struct{}
	release chan struct{}
	started sync.Once
	stop    sync.Once
	active  atomic.Int32
	fail    bool
	data    bytes.Buffer
}

func newBlockedOutput(fail bool) *blockedOutput {
	return &blockedOutput{entered: make(chan struct{}), release: make(chan struct{}), fail: fail}
}

func (w *blockedOutput) Write(data []byte) (int, error) {
	w.active.Add(1)
	defer w.active.Add(-1)
	w.started.Do(func() { close(w.entered) })
	<-w.release
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return w.data.Write(data)
}

func (w *blockedOutput) unblock() {
	w.stop.Do(func() { close(w.release) })
}

type execOutcome struct {
	code int
	err  error
}

func TestPTYWaitsForBlockedOutputWithoutAbort(t *testing.T) {
	payload := []byte("trailing PTY bytes")
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Stdout: payload, Exit: 23})
	output := newBlockedOutput(false)
	defer output.unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := make(chan execOutcome, 1)
	go func() {
		code, err := f.backend.Exec(ctx, alice, "mine", "exit 23", core.Streams{PTY: true, Out: output})
		done <- execOutcome{code, err}
	}()
	waitRead(t, output.entered)
	select {
	case outcome := <-done:
		t.Fatalf("returned before downstream output drained: %#v", outcome)
	case <-time.After(1200 * time.Millisecond):
	}
	output.unblock()
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.code != 23 || output.active.Load() != 0 || !bytes.Equal(output.data.Bytes(), payload) {
			t.Fatalf("incomplete PTY output: outcome %#v, active %d, data %q", outcome, output.active.Load(), output.data.Bytes())
		}
	case <-ctx.Done():
		t.Fatal("released writer did not finish")
	}
}

func TestPTYDrainTimeoutAbortsAndJoinsBothPumps(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Stdout: []byte("blocked output"), WaitForExecRelease: true})
	input := &persistentInput{data: make(chan []byte), entered: make(chan struct{}, 1)}
	output := newBlockedOutput(true)
	defer output.unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var aborts atomic.Int32
	done := make(chan execOutcome, 1)
	go func() {
		code, err := f.backend.Exec(ctx, alice, "mine", "true", core.Streams{PTY: true, In: input, Out: output, Abort: func() {
			aborts.Add(1)
			cancel()
			output.unblock()
		}})
		done <- execOutcome{code, err}
	}()
	waitRead(t, input.entered)
	if err := os.WriteFile(filepath.Join(f.dir, "exec.release"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	waitRead(t, output.entered)
	select {
	case outcome := <-done:
		if outcome.code == 0 || outcome.err == nil || !strings.Contains(outcome.err.Error(), "drain timed out") {
			t.Fatalf("truncated output reported success: %#v", outcome)
		}
		if aborts.Load() != 1 || output.active.Load() != 0 || input.active.Load() != 0 || input.closed.Load() {
			t.Fatalf("pumps not joined: aborts %d, writes %d, reads %d, input closed %v", aborts.Load(), output.active.Load(), input.active.Load(), input.closed.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain timeout did not abort and join output")
	}
}

func TestPTYDrainTimeoutClosesInheritedTerminalRead(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := nonblockingTerminal(master); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := f.backend.command(ctx, alice, "exec", "mine", "--", "/bin/sh", "-l")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var aborts atomic.Int32
	done := make(chan execOutcome, 1)
	go func() {
		code, err := f.backend.execPTY(ctx, cmd, core.Streams{Out: io.Discard, Abort: func() {
			aborts.Add(1)
			cancel()
		}}, master)
		done <- execOutcome{code, err}
	}()
	select {
	case outcome := <-done:
		if outcome.code == 0 || outcome.err == nil || !strings.Contains(outcome.err.Error(), "drain timed out") || aborts.Load() != 1 {
			t.Fatalf("inherited terminal result: %#v, aborts %d", outcome, aborts.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inherited terminal kept the output reader alive")
	}
}

type failedOutput struct{}

func (failedOutput) Write([]byte) (int, error) {
	return 0, syscall.EIO
}

func TestPTYWriterErrorIsNotTreatedAsTerminalEOF(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Stdout: []byte("output")})
	code, err := f.backend.Exec(context.Background(), alice, "mine", "true", core.Streams{PTY: true, Out: failedOutput{}})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "output failed") {
		t.Fatalf("writer EIO treated as terminal EOF: %d, %v", code, err)
	}
}

func TestPTYDrainsAllTrailingBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("trailing-PTY-data-0123456789"), 8192)
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Stdout: payload})
	var output bytes.Buffer
	var aborted atomic.Bool
	code, err := f.backend.Exec(context.Background(), alice, "mine", "true", core.Streams{PTY: true, Out: &output, Abort: func() { aborted.Store(true) }})
	if code != 0 || err != nil || aborted.Load() || !bytes.Equal(output.Bytes(), payload) {
		t.Fatalf("trailing bytes lost: code %d, error %v, aborted %v, bytes %d/%d", code, err, aborted.Load(), output.Len(), len(payload))
	}
}

func TestExecCancellationReapsProcessGroup(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(strconv.FormatBool(terminal), func(t *testing.T) {
			f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Block: "exec", Child: true})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			input := &trackedInput{reader: reader, entered: make(chan struct{}, 1)}
			done := make(chan error, 1)
			go func() {
				_, err := f.backend.Exec(ctx, alice, "mine", "sleep forever", core.Streams{PTY: terminal, In: input})
				done <- err
			}()
			waitRead(t, input.entered)
			pid, err := strconv.Atoi(string(waitFile(t, filepath.Join(f.dir, "ready.pid"))))
			if err != nil {
				t.Fatal(err)
			}
			child, err := strconv.Atoi(string(waitFile(t, filepath.Join(f.dir, "child.pid"))))
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel = %v", err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("Exec did not return after cancellation")
			}
			assertProcessGone(t, pid)
			assertProcessGone(t, child)
			if !input.closed.Load() || input.active.Load() != 0 {
				t.Fatal("canceled exec did not close and join its generic input copy")
			}
		})
	}
}

type trackedInput struct {
	reader  *io.PipeReader
	entered chan struct{}
	active  atomic.Int32
	closed  atomic.Bool
}

func (r *trackedInput) Read(data []byte) (int, error) {
	r.active.Add(1)
	defer r.active.Add(-1)
	select {
	case r.entered <- struct{}{}:
	default:
	}
	return r.reader.Read(data)
}

func (r *trackedInput) Close() error {
	r.closed.Store(true)
	return r.reader.Close()
}

type persistentInput struct {
	data    chan []byte
	entered chan struct{}
	active  atomic.Int32
	closed  atomic.Bool
}

func (r *persistentInput) Read([]byte) (int, error) {
	return 0, errors.New("persistent input requires ReadContext")
}

func (r *persistentInput) ReadContext(ctx context.Context, data []byte) (int, error) {
	r.active.Add(1)
	defer r.active.Add(-1)
	select {
	case r.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case value := <-r.data:
		return copy(data, value), nil
	}
}

func (r *persistentInput) Close() error {
	r.closed.Store(true)
	return nil
}

func waitRead(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("input copy did not begin reading")
	}
}

func TestSequentialExecsReleasePersistentContextReader(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(strconv.FormatBool(terminal), func(t *testing.T) {
			f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Block: "exec"})
			input := &persistentInput{data: make(chan []byte, 1), entered: make(chan struct{}, 1)}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			for i := 0; i < 2; i++ {
				signals := make(chan string, 1)
				done := make(chan error, 1)
				go func() {
					code, err := f.backend.Exec(ctx, alice, "mine", "wait", core.Streams{PTY: terminal, In: input, Signals: signals})
					if err == nil && code != 130 {
						err = errors.New("unexpected guest exit code")
					}
					done <- err
				}()
				waitRead(t, input.entered)
				waitFile(t, filepath.Join(f.dir, "ready.pid"))
				signals <- "INT"
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("guest exit did not release persistent input")
				}
				if input.active.Load() != 0 || input.closed.Load() {
					t.Fatal("exec returned with an active reader or closed the management input")
				}
				if err := os.Remove(filepath.Join(f.dir, "ready.pid")); err != nil {
					t.Fatal(err)
				}
			}
			input.data <- []byte("help\n")
			var command [16]byte
			n, err := input.ReadContext(ctx, command[:])
			if err != nil || string(command[:n]) != "help\n" {
				t.Fatalf("management reader after execs = %q, %v", command[:n], err)
			}
		})
	}
}

func TestCanceledExecJoinsContextReaderWithoutClosingIt(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(strconv.FormatBool(terminal), func(t *testing.T) {
			f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Block: "exec"})
			input := &persistentInput{data: make(chan []byte, 1), entered: make(chan struct{}, 1)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := f.backend.Exec(ctx, alice, "mine", "wait", core.Streams{PTY: terminal, In: input})
				done <- err
			}()
			waitRead(t, input.entered)
			waitFile(t, filepath.Join(f.dir, "ready.pid"))
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel = %v", err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("canceled exec did not join its context reader")
			}
			if input.active.Load() != 0 || input.closed.Load() {
				t.Fatal("canceled exec leaked a read or closed persistent input")
			}
			input.data <- []byte("list\n")
			readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
			defer readCancel()
			var command [16]byte
			n, err := input.ReadContext(readCtx, command[:])
			if err != nil || string(command[:n]) != "list\n" {
				t.Fatalf("input reuse after cancellation = %q, %v", command[:n], err)
			}
		})
	}
}

func TestPTYCloseInterruptsReadAfterResize(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := nonblockingTerminal(master); err != nil {
		t.Fatal(err)
	}
	if err := resizeTerminal(master, core.Window{Width: 100, Height: 30}); err != nil {
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		_, _ = io.Copy(io.Discard, master)
		close(done)
	}()
	<-started
	if err := master.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closing the resized PTY did not release its read")
	}
}

func TestPTYResizeAndSignalForwarding(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Block: "exec"})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resize := make(chan core.Window, 1)
	signals := make(chan string, 2)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := f.backend.Exec(ctx, alice, "mine", "", core.Streams{PTY: true, Term: "xterm-256color", Window: core.Window{Width: 100, Height: 30}, Resize: resize, Signals: signals})
		done <- result{code, err}
	}()
	waitFile(t, filepath.Join(f.dir, "ready.pid"))
	var size pty.Winsize
	if err := json.Unmarshal(waitFile(t, filepath.Join(f.dir, "size.json")), &size); err != nil || size.Cols != 100 || size.Rows != 30 {
		t.Fatalf("initial terminal size = %#v, %v", size, err)
	}
	signals <- "KILL"
	resize <- core.Window{Width: 120, Height: 40}
	if err := json.Unmarshal(waitFile(t, filepath.Join(f.dir, "resized.json")), &size); err != nil || size.Cols != 120 || size.Rows != 40 {
		t.Fatalf("resized terminal = %#v, %v", size, err)
	}
	signals <- "INT"
	select {
	case result := <-done:
		if result.err != nil || result.code != 130 {
			t.Fatalf("signal result = %d, %v", result.code, result.err)
		}
	case <-ctx.Done():
		t.Fatal("signal did not reach process")
	}
	calls := f.calls(t)
	if args := calls[len(calls)-1]; args[6] != "--mode=interactive" || !strings.Contains(strings.Join(args, " "), "TERM=xterm-256color") {
		t.Fatalf("interactive args = %q", args)
	}
}

func TestExecDoesNotHoldLifecycleLock(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{Block: "exec"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.backend.Exec(ctx, alice, "mine", "wait", core.Streams{})
	}()
	waitFile(t, filepath.Join(f.dir, "ready.pid"))
	removeCtx, removeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer removeCancel()
	if err := f.backend.Remove(removeCtx, alice, "mine"); err != nil {
		t.Fatalf("exec blocked lifecycle: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("exec did not stop")
	}
}

func TestExecRejectsTerminalInjection(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("mine", "alice")}, scenario{})
	if _, err := f.backend.Exec(context.Background(), alice, "mine", "true", core.Streams{PTY: true, Term: "xterm\nSECRET=x"}); err == nil {
		t.Fatal("accepted terminal injection")
	}
	for _, call := range f.calls(t) {
		if call[4] != "list" {
			t.Fatalf("invalid terminal reached exec: %q", call)
		}
	}
}

func TestSignalAllowlistAndTerminalBounds(t *testing.T) {
	for _, name := range []string{"", "KILL", "STOP", "SIGINT", "INT;id", "99"} {
		if _, ok := allowedSignal(name); ok {
			t.Errorf("accepted signal %q", name)
		}
	}
	for _, name := range []string{"INT", "TERM", "HUP", "QUIT", "USR1", "USR2"} {
		if _, ok := allowedSignal(name); !ok {
			t.Errorf("rejected signal %q", name)
		}
	}
	if size := terminalSize(core.Window{}); size.Cols != 80 || size.Rows != 24 {
		t.Fatalf("default terminal size = %#v", size)
	}
	if size := terminalSize(core.Window{Width: ^uint32(0), Height: ^uint32(0)}); size.Cols != 65535 || size.Rows != 65535 {
		t.Fatalf("overflowed terminal size = %#v", size)
	}
}
