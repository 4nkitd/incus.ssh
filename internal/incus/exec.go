package incus

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"

	"incus-for-devs/internal/core"
)

var termPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._-]{0,63}$`)

func (b *Backend) Exec(ctx context.Context, principal core.Principal, name, command string, streams core.Streams) (int, error) {
	checkCtx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	if err := b.lock(checkCtx); err != nil {
		cancel()
		return -1, err
	}
	locked := true
	unlock := func() {
		if locked {
			<-b.gate
			locked = false
		}
	}
	defer unlock()
	err := b.checkOwner(checkCtx, principal, name)
	cancel()
	if err != nil {
		return -1, err
	}
	if len(command) > 128<<10 {
		return -1, errors.New("command exceeds size limit")
	}
	args := []string{"exec", name, "--mode=non-interactive"}
	if streams.PTY {
		args[2] = "--mode=interactive"
	}
	term := "dumb"
	if streams.Term != "" {
		if !termPattern.MatchString(streams.Term) {
			return -1, errors.New("invalid terminal type")
		}
		term = streams.Term
	}
	args = append(args, "--env", "TERM="+term, "--", "/bin/sh")
	if command == "" {
		args = append(args, "-l")
	} else {
		args = append(args, "-lc", command)
	}
	cmd := b.command(ctx, principal, args...)
	cmd.Cancel = func() error {
		// Incus forwards TERM to the container before the local process group is reaped.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		return killGroup(cmd)
	}
	if streams.Out == nil {
		streams.Out = io.Discard
	}
	if streams.Err == nil {
		streams.Err = io.Discard
	}
	if streams.PTY {
		// StartWithSize creates a session and controlling terminal; Setpgid conflicts with Setsid.
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		terminal, err := pty.StartWithSize(cmd, terminalSize(streams.Window))
		if err != nil {
			return -1, launchError(ctx)
		}
		if err := nonblockingTerminal(terminal); err != nil {
			_ = killGroup(cmd)
			_ = terminal.Close()
			_ = cmd.Wait()
			return -1, errors.New("could not configure exec terminal")
		}
		unlock()
		return b.execPTY(ctx, cmd, streams, terminal)
	}
	cmd.Stdout, cmd.Stderr = streams.Out, streams.Err
	var inputRead, inputWrite *os.File
	if streams.In != nil {
		inputRead, inputWrite, err = os.Pipe()
		if err != nil {
			return -1, errors.New("could not open exec input pipe")
		}
		defer inputRead.Close()
		defer inputWrite.Close()
		cmd.Stdin = inputRead
	}
	if err := cmd.Start(); err != nil {
		return -1, launchError(ctx)
	}
	unlock()
	if inputRead != nil {
		_ = inputRead.Close()
	}
	input := copyInput(ctx, streams.In, inputWrite, true)
	stop := watchProcess(ctx, cmd, streams, nil)
	err = cmd.Wait()
	input.cancel()
	if inputWrite != nil {
		_ = inputWrite.Close()
	}
	input.join(ctx)
	stop()
	return execResult(ctx, err)
}

type inputCopy struct {
	reader io.Reader
	cancel context.CancelFunc
	done   chan struct{}
}

type contextInput struct {
	ctx    context.Context
	reader core.ContextReader
}

func (r contextInput) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.ReadContext(r.ctx, data)
}

func copyInput(ctx context.Context, reader io.Reader, destination *os.File, closeOnEOF bool) *inputCopy {
	ctx, cancel := context.WithCancel(ctx)
	copy := &inputCopy{reader: reader, cancel: cancel, done: make(chan struct{})}
	if reader == nil {
		close(copy.done)
		return copy
	}
	if contextual, ok := reader.(core.ContextReader); ok {
		reader = contextInput{ctx: ctx, reader: contextual}
	}
	go func() {
		defer close(copy.done)
		if closeOnEOF {
			defer destination.Close()
		}
		_, _ = io.Copy(destination, reader)
	}()
	return copy
}

func (copy *inputCopy) join(ctx context.Context) {
	if _, ok := copy.reader.(core.ContextReader); ok {
		<-copy.done
		return
	}
	if ctx.Err() != nil {
		if closer, ok := copy.reader.(io.Closer); ok {
			_ = closer.Close()
			<-copy.done
		}
	}
}

func launchError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("could not start incus exec")
}

func execResult(ctx context.Context, err error) (int, error) {
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			return code, nil
		}
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
	}
	return -1, errors.New("incus exec stream failed")
}

func terminalSize(window core.Window) *pty.Winsize {
	width, height := window.Width, window.Height
	if width == 0 {
		width = 80
	}
	if height == 0 {
		height = 24
	}
	if width > 65535 {
		width = 65535
	}
	if height > 65535 {
		height = 65535
	}
	return &pty.Winsize{Cols: uint16(width), Rows: uint16(height)}
}

func nonblockingTerminal(terminal *os.File) error {
	connection, err := terminal.SyscallConn()
	if err != nil {
		return err
	}
	var configureErr error
	err = connection.Control(func(fd uintptr) {
		configureErr = syscall.SetNonblock(int(fd), true)
	})
	if err != nil {
		return err
	}
	return configureErr
}

func resizeTerminal(terminal *os.File, window core.Window) error {
	connection, err := terminal.SyscallConn()
	if err != nil {
		return err
	}
	size := terminalSize(window)
	var resizeErr syscall.Errno
	// pty.Setsize calls File.Fd, which restores blocking I/O and prevents cancellation.
	err = connection.Control(func(fd uintptr) {
		_, _, resizeErr = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(size)))
	})
	if err != nil {
		return err
	}
	if resizeErr != 0 {
		return resizeErr
	}
	return nil
}

type terminalReader struct {
	terminal *os.File
}

func (r terminalReader) Read(data []byte) (int, error) {
	n, err := r.terminal.Read(data)
	if errors.Is(err, syscall.EIO) {
		err = io.EOF
	}
	return n, err
}

func (b *Backend) execPTY(ctx context.Context, cmd *exec.Cmd, streams core.Streams, terminal *os.File) (int, error) {
	defer terminal.Close()
	stop := watchProcess(ctx, cmd, streams, terminal)
	input := copyInput(ctx, streams.In, terminal, false)
	drained := make(chan struct{})
	var abortOnce sync.Once
	abort := func() {
		if streams.Abort != nil {
			abortOnce.Do(streams.Abort)
		}
	}
	var outputErr error
	go func() {
		defer close(drained)
		_, outputErr = io.Copy(streams.Out, terminalReader{terminal: terminal})
		if outputErr != nil {
			abort()
		}
	}()
	err := cmd.Wait()
	input.cancel()
	stop()
	var deadline <-chan time.Time
	if streams.Abort != nil {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		deadline = timer.C
	}
	var drainErr error
	select {
	case <-drained:
	case <-ctx.Done():
		drainErr = ctx.Err()
	case <-deadline:
		drainErr = errors.New("incus exec PTY output drain timed out")
	}
	if drainErr != nil {
		abort()
		_ = terminal.Close()
		// Closing the PTY releases reads; Abort must release writes to the transport.
		<-drained
	}
	_ = terminal.Close()
	if drainErr != nil || outputErr != nil {
		failedCtx, failedCancel := context.WithCancel(ctx)
		failedCancel()
		input.join(failedCtx)
	} else {
		input.join(ctx)
	}
	if drainErr != nil {
		return -1, drainErr
	}
	if outputErr != nil {
		return -1, errors.New("incus exec PTY output failed")
	}
	return execResult(ctx, err)
}

func watchProcess(ctx context.Context, cmd *exec.Cmd, streams core.Streams, terminal *os.File) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		resize, signals := streams.Resize, streams.Signals
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case window, ok := <-resize:
				if !ok {
					resize = nil
					continue
				}
				if terminal != nil {
					_ = resizeTerminal(terminal, window)
					_ = cmd.Process.Signal(syscall.SIGWINCH)
				}
			case name, ok := <-signals:
				if !ok {
					signals = nil
					continue
				}
				if signal, ok := allowedSignal(name); ok {
					_ = cmd.Process.Signal(signal)
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

func allowedSignal(name string) (syscall.Signal, bool) {
	switch name {
	case "HUP":
		return syscall.SIGHUP, true
	case "INT":
		return syscall.SIGINT, true
	case "QUIT":
		return syscall.SIGQUIT, true
	case "TERM":
		return syscall.SIGTERM, true
	case "USR1":
		return syscall.SIGUSR1, true
	case "USR2":
		return syscall.SIGUSR2, true
	}
	return 0, false
}
