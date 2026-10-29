package incus

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"incus-for-devs/internal/core"
)

const sftpCommand = `for server in /usr/lib/openssh/sftp-server /usr/libexec/openssh/sftp-server /usr/lib/ssh/sftp-server; do
if [ -x "$server" ]; then exec "$server"; fi
done
printf '%s\n' 'Install openssh-sftp-server inside this container to use SCP/SFTP.' >&2
exit 127`

type sftpProcess struct {
	reader *os.File
	writer *os.File
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

var _ core.FileBackend = (*Backend)(nil)

func (b *Backend) OpenSFTP(ctx context.Context, p core.Principal, name string) (io.ReadWriteCloser, error) {
	checkCtx, cancelCheck := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancelCheck()
	if err := validatePrincipal(p); err != nil || !validName(name) {
		return nil, errors.New("invalid file transfer scope")
	}
	if err := b.lock(checkCtx); err != nil {
		return nil, err
	}
	defer func() { <-b.gate }()
	items, err := b.list(checkCtx, p)
	if err != nil {
		return nil, err
	}
	item, err := findOwned(items, p, name)
	if err != nil {
		return nil, err
	}
	if item.Status != "Running" {
		return nil, errors.New("file transfer requires a running owned container")
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := b.command(ctx, p, "exec", name, "--mode=non-interactive", "--env", "LC_ALL=C", "--", "/bin/sh", "-c", sftpCommand)
	cmd.Cancel = func() error {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		return killGroup(cmd)
	}
	stdin, writer, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, errors.New("cannot open container file input")
	}
	reader, stdout, err := os.Pipe()
	if err != nil {
		cancel()
		stdin.Close()
		writer.Close()
		return nil, errors.New("cannot open container file output")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		writer.Close()
		reader.Close()
		stdout.Close()
		return nil, launchError(ctx)
	}
	stdin.Close()
	stdout.Close()
	process := &sftpProcess{reader: reader, writer: writer, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(process.done); cmd.Wait() }()
	return process, nil
}

func (p *sftpProcess) Read(data []byte) (int, error)  { return p.reader.Read(data) }
func (p *sftpProcess) Write(data []byte) (int, error) { return p.writer.Write(data) }
func (p *sftpProcess) Close() error {
	p.once.Do(func() {
		p.cancel()
		p.writer.Close()
		p.reader.Close()
		<-p.done
	})
	return nil
}
