package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

type handlerFunc func(context.Context, core.Principal, string, core.Streams) int

func (f handlerFunc) Handle(ctx context.Context, p core.Principal, command string, streams core.Streams) int {
	return f(ctx, p, command, streams)
}

func testKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "gateway test key")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "host_key")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	return signer, path
}

func testConfig(t *testing.T, handler core.Handler) (Config, ssh.Signer, ssh.Signer) {
	t.Helper()
	host, path := testKey(t)
	user, _ := testKey(t)
	return Config{
		HostKeyPath: path,
		Users: []AuthorizedUser{{
			Principal:      core.Principal{Name: "alice", Project: "dev-alice"},
			AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(user.PublicKey()))},
		}},
		Handler: handler,
	}, host, user
}

func startServer(t *testing.T, cfg Config) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- Serve(ctx, listener, cfg)
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return listener.Addr().String(), cancel, done
}

func dial(t *testing.T, addr string, host, key ssh.Signer) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "alice",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.FixedHostKey(host.PublicKey()),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func newSession(t *testing.T, client *ssh.Client) *ssh.Session {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestAuthenticationAndHostKey(t *testing.T) {
	cfg, host, key := testConfig(t, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }))
	addr, _, _ := startServer(t, cfg)
	client := dial(t, addr, host, key)
	if err := newSession(t, client).Run("help"); err != nil {
		t.Fatal(err)
	}
	wrong, _ := testKey(t)
	cases := []struct {
		name, user string
		auth       []ssh.AuthMethod
		host       ssh.Signer
	}{
		{"wrong username", "bob", []ssh.AuthMethod{ssh.PublicKeys(key)}, host},
		{"username case", "Alice", []ssh.AuthMethod{ssh.PublicKeys(key)}, host},
		{"wrong key", "alice", []ssh.AuthMethod{ssh.PublicKeys(wrong)}, host},
		{"none", "alice", nil, host},
		{"password", "alice", []ssh.AuthMethod{ssh.Password("alice")}, host},
		{"keyboard interactive", "alice", []ssh.AuthMethod{ssh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) { return []string{"alice"}, nil })}, host},
		{"wrong host key", "alice", []ssh.AuthMethod{ssh.PublicKeys(key)}, wrong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: tc.user, Auth: tc.auth, HostKeyCallback: ssh.FixedHostKey(tc.host.PublicKey()), Timeout: 3 * time.Second})
			if err == nil {
				client.Close()
				t.Fatal("authentication unexpectedly succeeded")
			}
		})
	}
}

func TestConfigurationValidation(t *testing.T) {
	base, _, _ := testConfig(t, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }))
	other, _ := testKey(t)
	otherKey := string(ssh.MarshalAuthorizedKey(other.PublicKey()))
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"no handler", func(c *Config) { c.Handler = nil }},
		{"no users", func(c *Config) { c.Users = nil }},
		{"no name", func(c *Config) { c.Users[0].Principal.Name = "" }},
		{"no project", func(c *Config) { c.Users[0].Principal.Project = "" }},
		{"unsafe identity", func(c *Config) { c.Users[0].Principal.Project = "../default" }},
		{"no keys", func(c *Config) { c.Users[0].AuthorizedKeys = nil }},
		{"bad key", func(c *Config) { c.Users[0].AuthorizedKeys = []string{"not a public key"} }},
		{"key options", func(c *Config) { c.Users[0].AuthorizedKeys[0] = "no-pty " + c.Users[0].AuthorizedKeys[0] }},
		{"multiple keys per entry", func(c *Config) { c.Users[0].AuthorizedKeys[0] += otherKey }},
		{"junk before key", func(c *Config) { c.Users[0].AuthorizedKeys[0] = "bad key data\n" + c.Users[0].AuthorizedKeys[0] }},
		{"duplicate user", func(c *Config) {
			c.Users = append(c.Users, AuthorizedUser{core.Principal{Name: "alice", Project: "other"}, []string{otherKey}})
		}},
		{"duplicate project", func(c *Config) {
			c.Users = append(c.Users, AuthorizedUser{core.Principal{Name: "bob", Project: "dev-alice"}, []string{otherKey}})
		}},
		{"shared key", func(c *Config) {
			c.Users = append(c.Users, AuthorizedUser{core.Principal{Name: "bob", Project: "dev-bob"}, c.Users[0].AuthorizedKeys})
		}},
		{"duplicate key", func(c *Config) {
			c.Users[0].AuthorizedKeys = append(c.Users[0].AuthorizedKeys, c.Users[0].AuthorizedKeys[0])
		}},
		{"no host key", func(c *Config) { c.HostKeyPath = "" }},
		{"missing host key", func(c *Config) { c.HostKeyPath = filepath.Join(t.TempDir(), "missing") }},
		{"invalid limit", func(c *Config) { c.MaxSessions = -1 }},
		{"excessive limit", func(c *Config) { c.MaxSessions = 4097 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Users = append([]AuthorizedUser(nil), base.Users...)
			cfg.Users[0].AuthorizedKeys = append([]string(nil), base.Users[0].AuthorizedKeys...)
			tc.change(&cfg)
			if _, _, _, err := serverConfig(cfg); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	t.Run("malformed host key", func(t *testing.T) {
		cfg := base
		cfg.HostKeyPath = filepath.Join(t.TempDir(), "bad-key")
		if err := os.WriteFile(cfg.HostKeyPath, []byte("not private key data"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := serverConfig(cfg); err == nil {
			t.Fatal("malformed host key accepted")
		}
	})
}

func TestHostKeyFileSecurity(t *testing.T) {
	base, _, _ := testConfig(t, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }))
	data, err := os.ReadFile(base.HostKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0640, 0604, 0620, 0602, 0666} {
		t.Run(mode.String(), func(t *testing.T) {
			cfg := base
			cfg.HostKeyPath = filepath.Join(t.TempDir(), "host_key")
			if err := os.WriteFile(cfg.HostKeyPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(cfg.HostKeyPath, mode); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := serverConfig(cfg); err == nil {
				t.Fatal("accepted exposed host key")
			}
		})
	}
	t.Run("directory", func(t *testing.T) {
		cfg := base
		cfg.HostKeyPath = t.TempDir()
		if _, _, _, err := serverConfig(cfg); err == nil {
			t.Fatal("accepted directory as host key")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		cfg := base
		cfg.HostKeyPath = filepath.Join(t.TempDir(), "host_key")
		if err := os.Symlink(base.HostKeyPath, cfg.HostKeyPath); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := serverConfig(cfg); err == nil {
			t.Fatal("accepted symlink as host key")
		}
	})
	t.Run("oversize", func(t *testing.T) {
		cfg := base
		cfg.HostKeyPath = filepath.Join(t.TempDir(), "host_key")
		if err := os.WriteFile(cfg.HostKeyPath, append(data, bytes.Repeat([]byte{'\n'}, maxHostKeySize)...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := serverConfig(cfg); err == nil {
			t.Fatal("accepted oversized host key")
		}
	})
}

func TestExecStreamsAndExitStatus(t *testing.T) {
	const command = `ssh demo -- printf '%s' 'hello world'`
	observed := make(chan string, 1)
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, p core.Principal, raw string, s core.Streams) int {
		if p != (core.Principal{Name: "alice", Project: "dev-alice"}) {
			t.Errorf("principal = %#v", p)
		}
		observed <- raw
		io.Copy(s.Out, s.In)
		io.WriteString(s.Err, "stderr message")
		return 37
	}))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	var stdout, stderr bytes.Buffer
	session.Stdin = strings.NewReader("stdin message")
	session.Stdout, session.Stderr = &stdout, &stderr
	err := session.Run(command)
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 37 {
		t.Fatalf("exit = %v", err)
	}
	if got := <-observed; got != command {
		t.Fatalf("command = %q", got)
	}
	if stdout.String() != "stdin message" || stderr.String() != "stderr message" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestBlockedRequestsAndChannels(t *testing.T) {
	cfg, host, key := testConfig(t, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }))
	addr, _, _ := startServer(t, cfg)
	client := dial(t, addr, host, key)
	for _, kind := range []string{"direct-tcpip", "forwarded-tcpip", "auth-agent@openssh.com", "x11", "unknown"} {
		channel, _, err := client.OpenChannel(kind, nil)
		if err == nil {
			channel.Close()
			t.Errorf("accepted channel %s", kind)
		}
	}
	session := newSession(t, client)
	if err := session.Setenv("TERM", "xterm"); err == nil {
		t.Error("accepted env")
	}
	if err := session.RequestSubsystem("sftp"); err == nil {
		t.Error("accepted subsystem")
	}
	for _, kind := range []string{"auth-agent-req@openssh.com", "x11-req", "unknown"} {
		if ok, err := session.SendRequest(kind, true, nil); err != nil || ok {
			t.Errorf("request %s: accepted=%v err=%v", kind, ok, err)
		}
	}
	for _, kind := range []string{"tcpip-forward", "cancel-tcpip-forward", "unknown"} {
		if ok, _, err := client.SendRequest(kind, true, nil); err != nil || ok {
			t.Errorf("global %s: accepted=%v err=%v", kind, ok, err)
		}
	}
	if ok, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil || !ok {
		t.Errorf("keepalive: accepted=%v err=%v", ok, err)
	}
	if err := session.Run("help"); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionCancellation(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "client disconnect", true: "parent cancellation"}[parent], func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, _ string, s core.Streams) int {
				close(started)
				<-ctx.Done()
				_, _ = io.ReadAll(s.In)
				close(stopped)
				return 1
			}))
			addr, cancel, _ := startServer(t, cfg)
			client := dial(t, addr, host, key)
			session := newSession(t, client)
			if _, err := session.StdinPipe(); err != nil {
				t.Fatal(err)
			}
			if err := session.Start("ssh demo"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not start")
			}
			if parent {
				cancel()
			} else {
				client.Close()
			}
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("handler was not canceled")
			}
		})
	}
}

func TestSessionLimit(t *testing.T) {
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, _ string, _ core.Streams) int { <-ctx.Done(); return 0 }))
	cfg.MaxSessions = 1
	addr, _, _ := startServer(t, cfg)
	client := dial(t, addr, host, key)
	first := newSession(t, client)
	if err := first.Start("ssh demo"); err != nil {
		t.Fatal(err)
	}
	if second, err := client.NewSession(); err == nil {
		second.Close()
		t.Fatal("session limit not enforced")
	}
}

func TestParentCancellationClosesUnauthenticatedConnection(t *testing.T) {
	cfg, _, _ := testConfig(t, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }))
	addr, cancel, _ := startServer(t, cfg)
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 256)
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("unauthenticated connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("connection was not closed before deadline")
	}
}

func TestConnectionLimitIncludesHandshakes(t *testing.T) {
	cfg, host, key := testConfig(t, handlerFunc(func(context.Context, core.Principal, string, core.Streams) int { return 0 }))
	cfg.MaxSessions = 1
	addr, _, _ := startServer(t, cfg)
	for i := 0; i < 2; i++ {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Read(make([]byte, 256)); err != nil {
			t.Fatal(err)
		}
	}
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: "alice", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: time.Second,
	})
	if err == nil {
		client.Close()
		t.Fatal("accepted connection beyond connection limit")
	}
}

func TestAuthenticatedSessionOutlivesHandshakeDeadline(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, _ string, streams core.Streams) int {
		close(ready)
		select {
		case <-ctx.Done():
			return 1
		case <-release:
			io.WriteString(streams.Out, "still connected")
			return 0
		}
	}))
	addr, _, _ := startServer(t, cfg)
	client := dial(t, addr, host, key)
	session := newSession(t, client)
	var output bytes.Buffer
	session.Stdout = &output
	if err := session.Start("ssh demo"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	<-time.After(handshakeTimeout + 100*time.Millisecond)
	if ok, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil || !ok {
		t.Fatalf("authenticated connection expired: accepted=%v err=%v", ok, err)
	}
	close(release)
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "still connected" {
		t.Fatalf("stdout = %q", output.String())
	}
}

func TestSessionCloseCancelsHandler(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	cfg, host, key := testConfig(t, handlerFunc(func(ctx context.Context, _ core.Principal, _ string, _ core.Streams) int {
		close(started)
		<-ctx.Done()
		close(stopped)
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	client := dial(t, addr, host, key)
	session := newSession(t, client)
	if err := session.Start("ssh demo"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	session.Close()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("session handler was not canceled")
	}
	if ok, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil || !ok {
		t.Fatalf("connection closed with session: accepted=%v err=%v", ok, err)
	}
}
