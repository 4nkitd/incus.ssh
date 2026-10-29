package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxCapturedOutput   = 4 << 10
	maxQueryOutput      = 256 << 10
	maxSimulationOutput = 1 << 20
	maxDetailRunes      = 600
)

// cancelGrace is a variable so tests can shorten it. It is never treated as a
// safe amount of time for an interrupted package database.
var cancelGrace = 60 * time.Second

type execFunc func(ctx context.Context, name string, args, env []string, stdout, stderr io.Writer) error

var execute execFunc = executeCommand

type execResult struct {
	stdout    string
	stderr    string
	err       error
	canceled  bool
	truncated bool
}

func (r execResult) detail() string {
	return boundedText(r.stderr, r.stdout)
}

func boundedText(primary, fallback string) string {
	text := primary
	if strings.TrimSpace(text) == "" {
		text = fallback
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxDetailRunes {
		return string(runes[:maxDetailRunes]) + "..."
	}
	return text
}

func runOne(ctx context.Context, timeout time.Duration, capture int, out, errOut io.Writer, name string, args, env []string) execResult {
	if err := validateCommand(name, args); err != nil {
		return execResult{err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr := newTailBuffer(capture), newTailBuffer(capture)
	err := execute(ctx, name, args, env, io.MultiWriter(writerOrDiscard(out), stdout), io.MultiWriter(writerOrDiscard(errOut), stderr))
	result := execResult{
		stdout:    stdout.String(),
		stderr:    stderr.String(),
		err:       err,
		truncated: stdout.truncated || stderr.truncated,
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		result.canceled = true
		reason := "was cancelled"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			reason = fmt.Sprintf("did not finish within %s", timeout)
		}
		if err == nil || errors.Is(err, ctxErr) {
			result.err = errors.New(reason)
		} else {
			result.err = fmt.Errorf("%s: %w", reason, err)
		}
	}
	return result
}

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}

func queryArgs() []string {
	return []string{"--force-local", "query", "/1.0"}
}

func updateArgs() []string {
	return []string{"update"}
}

const (
	incusPackage          = "incus"
	incusClientPackage    = "incus-client"
	caCertificatesPackage = "ca-certificates"
	dnsmasqBasePackage    = "dnsmasq-base"
)

var requestedPackages = []string{incusPackage, incusClientPackage, caCertificatesPackage, dnsmasqBasePackage}

func packageActionArgs(simulate bool) []string {
	args := []string{"install"}
	if simulate {
		args = append(args, "--simulate")
	}
	args = append(args, "--no-remove", "--no-install-recommends", "--yes")
	return append(args, requestedPackages...)
}

func simulateArgs() []string {
	return packageActionArgs(true)
}

func installArgs() []string {
	return packageActionArgs(false)
}

// validateCommand keeps the installer's command surface exact: one read-only
// daemon metadata query and the three approved apt-get invocations.
func validateCommand(name string, args []string) error {
	switch name {
	case incusBinary:
		if sameArgs(args, queryArgs()) {
			return nil
		}
	case aptGet:
		if sameArgs(args, updateArgs()) || sameArgs(args, simulateArgs()) || sameArgs(args, installArgs()) {
			return nil
		}
	}
	return fmt.Errorf("refusing unexpected command %q %q", name, strings.Join(args, " "))
}

func sameArgs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// lingeringProcessError means cancellation grace expired while the package
// manager was still running. The process is deliberately left alone: force
// killing apt or dpkg can corrupt the package database. The Wait goroutine
// keeps reaping the child if it eventually exits while this process is alive.
type lingeringProcessError struct {
	name  string
	pid   int
	grace time.Duration
}

func (e *lingeringProcessError) Error() string {
	return fmt.Sprintf("%s is still running after SIGTERM (pid %d, %s grace); it was not force-killed because apt and dpkg state must not be interrupted", e.name, e.pid, e.grace)
}

// executeCommand runs an absolute trusted binary with empty stdin. Cancellation
// signals the whole process group with SIGTERM and waits a bounded grace
// period. It never sends SIGKILL: an interrupted package manager may still
// need repair once it has exited.
func executeCommand(ctx context.Context, name string, args, env []string, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	grace := time.NewTimer(cancelGrace)
	defer grace.Stop()
	select {
	case err := <-done:
		if err != nil {
			return err
		}
		return ctx.Err()
	case <-grace.C:
		return &lingeringProcessError{name: name, pid: cmd.Process.Pid, grace: cancelGrace}
	}
}

type tailBuffer struct {
	mu        sync.Mutex
	max       int
	buf       []byte
	truncated bool
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, data...)
	if len(b.buf) > b.max {
		b.truncated = true
		trimmed := make([]byte, b.max)
		copy(trimmed, b.buf[len(b.buf)-b.max:])
		b.buf = trimmed
	}
	return len(data), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
