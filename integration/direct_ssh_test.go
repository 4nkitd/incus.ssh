package integration_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestDirectSSHStockClient(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client unavailable")
	}
	s := startGateway(t)
	run(t, s.connect(t, alice), "new --name=demo", nil, 0)
	host, port, err := net.SplitHostPort(s.address)
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, []byte(knownhosts.Line([]string{knownhosts.Normalize(s.address)}, s.host)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshPath, "-F", "/dev/null", "-T", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+known, "-o", "GlobalKnownHostsFile=/dev/null",
		"-p", port, "-i", s.keyFiles[alice.Name], "alice+demo@"+host, binaryCommand)
	input := bytes.Repeat([]byte{0, 255, '\n', '\r'}, 16385)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(input), &stdout, &stderr
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 37 {
		t.Fatalf("ssh exit: %v; stderr=%q", err, stderr.Bytes())
	}
	if !bytes.Equal(stdout.Bytes(), append([]byte("out\x00\xff"), input...)) || stderr.String() != "err\x00\xfe" {
		t.Fatal("non-PTY SSH altered guest output")
	}
}

func TestDirectSSHScope(t *testing.T) {
	s := startGateway(t)
	for _, login := range []string{"bob+demo", "unknown+demo", "alice+", "alice+../demo", "alice+demo+more"} {
		client, err := s.dial(login, s.keys[alice.Name], s.host)
		if err == nil {
			client.Close()
			t.Fatalf("unexpected login as %q", login)
		}
	}
	if s.backend.callCount() != 0 {
		t.Fatal("authentication reached the guest backend")
	}
	management := s.connect(t, alice)
	run(t, s.connect(t, bob), "new --name=demo", nil, 0)
	client, err := s.dial("alice+demo", s.keys[alice.Name], s.host)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if r := run(t, client, binaryCommand, nil, 1); !strings.Contains(r.err, "not owned") {
		t.Fatalf("guest selection skipped ownership check: %+v", r)
	}
	run(t, management, "new --name=demo", nil, 0)
	for _, raw := range []string{"rm demo", "  'quoted program'  ", "printf '%s\\n' --json\npwd"} {
		// The fixture doesn't execute arbitrary commands; inspect what reached Exec.
		run(t, client, raw, nil, 1)
		call := s.backend.lastExec()
		if call.command != raw || call.name != "demo" || call.principal != alice {
			t.Fatalf("direct exec changed command or scope: %+v", call)
		}
	}
	run(t, client, "", nil, 0)
	if call := s.backend.lastExec(); call.command != ":" {
		t.Fatalf("empty exec opened a guest shell: %+v", call)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.RequestSubsystem("sftp"); err == nil {
		t.Fatal("direct guest login reached the management SFTP namespace")
	}
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	session.Stdin = strings.NewReader("exit\n")
	var out bytes.Buffer
	session.Stdout = &out
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	call := s.backend.lastExec()
	if !call.pty || call.command != "" || string(call.input) != "exit\n" || call.name != "demo" || call.principal != alice || out.Len() != 0 {
		t.Fatalf("shell did not go directly to guest: %+v output=%q", call, out.String())
	}
}
