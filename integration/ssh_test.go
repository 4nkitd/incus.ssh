package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/command"
	"incus-for-devs/internal/core"
	"incus-for-devs/internal/gateway"
)

const binaryCommand = `printf 'out\000\377'; cat; printf 'err\000\376' >&2; exit 37`

var alice = core.Principal{Name: "alice", Project: "dev-alice"}
var bob = core.Principal{Name: "bob", Project: "dev-bob"}

func TestSSHManagementAndRemoteExecution(t *testing.T) {
	s := startGateway(t)
	for _, attempt := range []struct {
		name string
		user string
		key  ssh.Signer
	}{
		{"unknown username", "mallory", s.keys[alice.Name]},
		{"root username", "root", s.keys[alice.Name]},
		{"another user's key", alice.Name, s.keys[bob.Name]},
		{"unknown key", alice.Name, generatedKey(t, "untrusted").signer},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			client, err := s.dial(attempt.user, attempt.key, s.host)
			if client != nil {
				client.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
				t.Fatalf("authentication should be rejected, got %v", err)
			}
		})
	}
	if calls := s.backend.callCount(); calls != 0 {
		t.Fatalf("failed authentication reached backend: %d calls", calls)
	}
	client := s.connect(t, alice)
	other := s.connect(t, bob)

	help := run(t, client, "help", nil, 0)
	if !strings.Contains(help.out, "new [--name=NAME]") || !strings.Contains(help.out, "pause") || help.err != "" {
		t.Fatalf("unexpected help response: %+v", help)
	}
	for _, user := range []struct {
		client *ssh.Client
		want   core.Principal
	}{{client, alice}, {other, bob}} {
		response := run(t, user.client, "whoami --json", nil, 0)
		var got core.Principal
		decode(t, response.out, &got)
		if got != user.want || response.err != "" {
			t.Fatalf("whoami = %+v, stderr %q; want %+v", got, response.err, user.want)
		}
	}
	created := run(t, client, "new --name=demo --json", nil, 0)
	assertInstance(t, created.out, "demo")
	if created.err != "" {
		t.Fatalf("create stderr: %q", created.err)
	}
	assertList(t, run(t, client, "ls --json", nil, 0), "demo")
	assertList(t, run(t, other, "ls --json", nil, 0))

	for _, line := range []string{"rm demo", "resume demo", "start demo", "stop demo", "restart demo", "ssh demo printf stolen"} {
		response := run(t, other, line, nil, 1)
		if response.out != "" || !strings.Contains(response.err, "not owned") {
			t.Fatalf("foreign access %q: %+v", line, response)
		}
	}
	assertList(t, run(t, client, "ls --json", nil, 0), "demo")
	before := s.backend.callCount()
	for _, line := range []string{
		"pause demo",
		"new --name=escape --project=dev-bob",
		"new --name=escape --user=bob",
		"ls --project=dev-bob",
		"rm demo --user=bob",
	} {
		response := run(t, client, line, nil, 1)
		if response.out != "" || response.err == "" {
			t.Fatalf("invalid management command %q: %+v", line, response)
		}
	}
	if got := s.backend.callCount(); got != before {
		t.Fatalf("rejected commands reached backend: calls %d -> %d", before, got)
	}

	input := bytes.Repeat([]byte{0, 1, 10, 13, 127, 128, 254, 255}, 8193)
	response := run(t, client, "ssh demo "+binaryCommand, input, 37)
	wantOut := append([]byte{'o', 'u', 't', 0, 255}, input...)
	if !bytes.Equal([]byte(response.out), wantOut) || response.err != "err\x00\xfe" {
		t.Fatalf("binary streams changed: stdout bytes=%d want=%d, stderr=%q", len(response.out), len(wantOut), response.err)
	}
	call := s.backend.lastExec()
	if call.principal != alice || call.name != "demo" || call.command != binaryCommand || call.pty || !bytes.Equal(call.input, input) {
		t.Fatalf("remote request changed: principal=%+v name=%q command=%q pty=%v input bytes=%d", call.principal, call.name, call.command, call.pty, len(call.input))
	}
	response = run(t, client, `ssh demo printf '%s' 'still connected'`, nil, 0)
	if response.out != "still connected" || response.err != "" {
		t.Fatalf("session after nonzero exit: %+v", response)
	}

	assertInstance(t, run(t, other, "new --name=demo --json", nil, 0).out, "demo")
	removed := run(t, client, "rm demo --json", nil, 0)
	var result struct {
		Removed []string `json:"removed"`
	}
	decode(t, removed.out, &result)
	if !reflect.DeepEqual(result.Removed, []string{"demo"}) || removed.err != "" {
		t.Fatalf("remove response: %+v, stderr %q", result, removed.err)
	}
	assertList(t, run(t, client, "ls --json", nil, 0))
	assertList(t, run(t, other, "ls --json", nil, 0), "demo")
	s.stop(t)
	if session, err := client.NewSession(); err == nil {
		session.Close()
		t.Fatal("connection accepted a session after server shutdown")
	}
}

func TestSSHShellManagementSequence(t *testing.T) {
	s := startGateway(t)
	client := s.connect(t, alice)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	session.Stdin = strings.NewReader("whoami\nnew --name=demo --json\nls --json\npause demo\nrm demo --json\nls --json\nexit\n")
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	output := stdout.String()
	for _, fragment := range []string{"alice\tdev-alice", `"vm_name":"demo"`, `"vms":[{"vm_name":"demo"`, `"removed":["demo"]`, `"vms":[]`} {
		at := strings.Index(output, fragment)
		if at < 0 {
			t.Fatalf("missing ordered shell output %q in %q", fragment, output)
		}
		output = output[at+len(fragment):]
	}
	if !strings.Contains(stderr.String(), "pause is unsupported") {
		t.Fatalf("missing shell error: %q", stderr.String())
	}
	assertList(t, run(t, client, "ls --json", nil, 0))
}

func TestSSHLifecycleStateAndOwnership(t *testing.T) {
	s := startGateway(t)
	aliceClient := s.connect(t, alice)
	bobClient := s.connect(t, bob)
	assertInstance(t, run(t, aliceClient, "new --name=demo --json", nil, 0).out, "demo")
	assertStatus(t, aliceClient, "demo", "Running")

	steps := []struct {
		command string
		status  string
	}{
		{"stop", "Stopped"},
		{"start", "Running"},
		{"stop", "Stopped"},
		{"resume", "Running"},
		{"restart", "Running"},
	}
	for _, step := range steps {
		assertLifecycle(t, run(t, aliceClient, step.command+" demo --json", nil, 0), "demo")
		assertStatus(t, aliceClient, "demo", step.status)
	}
	var methods []string
	for _, call := range s.backend.lifecycleCalls() {
		if call.principal != alice || call.name != "demo" {
			t.Fatalf("lifecycle call recorded wrong identity: %+v", call)
		}
		methods = append(methods, call.method)
	}
	if want := []string{"stop", "start", "stop", "resume", "restart"}; !reflect.DeepEqual(methods, want) {
		t.Fatalf("lifecycle calls = %v, want %v", methods, want)
	}

	recorded := len(s.backend.lifecycleCalls())
	for _, command := range []string{"stop", "start", "restart", "resume"} {
		response := run(t, bobClient, command+" demo", nil, 1)
		if response.out != "" || !strings.Contains(response.err, "not owned") {
			t.Fatalf("foreign %s: %+v", command, response)
		}
	}
	if got := s.backend.lifecycleCalls(); len(got) != recorded {
		t.Fatalf("foreign lifecycle reached the backend: %+v", got[recorded:])
	}
	assertStatus(t, aliceClient, "demo", "Running")

	assertInstance(t, run(t, bobClient, "new --name=demo --json", nil, 0).out, "demo")
	assertLifecycle(t, run(t, aliceClient, "stop demo --json", nil, 0), "demo")
	assertStatus(t, aliceClient, "demo", "Stopped")
	assertStatus(t, bobClient, "demo", "Running")
	run(t, aliceClient, "rm demo --json", nil, 0)
	run(t, bobClient, "rm demo --json", nil, 0)
}

func TestSSHProcessesWireMeaning(t *testing.T) {
	s := startGateway(t)
	client := s.connect(t, alice)

	var help struct {
		Commands []struct {
			Usage string `json:"usage"`
		} `json:"commands"`
		Limits core.Limits `json:"limits"`
	}
	decode(t, run(t, client, "help --json", nil, 0).out, &help)
	if help.Limits.Processes != 512 {
		t.Fatalf("help limits processes = %d, want 512", help.Limits.Processes)
	}
	found := false
	for _, command := range help.Commands {
		found = found || strings.Contains(command.Usage, "--processes=N")
	}
	if !found {
		t.Fatalf("help JSON omits the processes flag: %+v", help.Commands)
	}
	if text := run(t, client, "help new", nil, 0).out; !strings.Contains(text, "256 processes") {
		t.Fatalf("new help omits the process default: %q", text)
	}

	created := run(t, client, "new --name=proc --processes=128 --json", nil, 0)
	assertInstance(t, created.out, "proc")
	if got := s.backend.lastCreate(); got.Processes != 128 || got.Name != "proc" {
		t.Fatalf("create options = %+v", got)
	}
	if !strings.Contains(created.out, `"processes":"128"`) {
		t.Fatalf("create response omits the requested process limit: %s", created.out)
	}

	var listed struct {
		VMs []struct {
			Name      string `json:"vm_name"`
			Processes string `json:"processes"`
		} `json:"vms"`
	}
	decode(t, run(t, client, "ls --json", nil, 0).out, &listed)
	if len(listed.VMs) != 1 || listed.VMs[0].Name != "proc" || listed.VMs[0].Processes != "128" {
		t.Fatalf("list process limit = %+v", listed.VMs)
	}
}

func TestSSHPTYHelpCRLFAndGuestBytes(t *testing.T) {
	s := startGateway(t)
	client := s.connect(t, alice)
	run(t, client, "new --name=demo --json", nil, 0)

	assertPTYLineEndings(t, runPTY(t, client, "help", nil, 0).out)
	plain := run(t, client, "help", nil, 0)
	if strings.Contains(plain.out, "\r") || !strings.Contains(plain.out, "\n") {
		t.Fatalf("non-PTY help changed line endings: %q", plain.out)
	}

	input := bytes.Repeat([]byte{0, 1, 10, 13, 127, 128, 254, 255}, 8193)
	response := runPTY(t, client, "ssh demo "+binaryCommand, input, 37)
	wantOut := append([]byte{'o', 'u', 't', 0, 255}, input...)
	if !bytes.Equal([]byte(response.out), wantOut) || response.err != "err\x00\xfe" {
		t.Fatalf("PTY guest streams changed: stdout bytes=%d want=%d, stderr=%q", len(response.out), len(wantOut), response.err)
	}
	call := s.backend.lastExec()
	if call.principal != alice || call.name != "demo" || call.command != binaryCommand || !call.pty || !bytes.Equal(call.input, input) {
		t.Fatalf("PTY request changed: principal=%+v name=%q command=%q pty=%v input bytes=%d", call.principal, call.name, call.command, call.pty, len(call.input))
	}
}

func TestSSHExecutionCancellation(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "client disconnect"
		if shutdown {
			name = "server shutdown"
		}
		t.Run(name, func(t *testing.T) {
			s := startGateway(t)
			client := s.connect(t, alice)
			run(t, client, "new --name=demo --json", nil, 0)
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if err := session.Start("ssh demo wait-for-cancel"); err != nil {
				t.Fatal(err)
			}
			await(t, s.backend.started, "backend exec start")
			if shutdown {
				s.stop(t)
			} else {
				client.Close()
			}
			await(t, s.backend.cancelled, "backend context cancellation")
			if err := session.Wait(); err == nil {
				t.Fatal("cancelled execution reported success")
			}
			if !shutdown {
				fresh := s.connect(t, alice)
				assertList(t, run(t, fresh, "ls --json", nil, 0), "demo")
			}
		})
	}
}

type keyFile struct {
	path   string
	signer ssh.Signer
}

func generatedKey(t *testing.T, name string) keyFile {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, name)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	data := pem.EncodeToMemory(block)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	return keyFile{path, signer}
}

type testGateway struct {
	address string
	host    ssh.PublicKey
	keys    map[string]ssh.Signer
	backend *memoryBackend
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
}

func startGateway(t *testing.T) *testGateway {
	t.Helper()
	host := generatedKey(t, "host")
	backend := &memoryBackend{instances: make(map[core.Principal]map[string]core.Instance), started: make(chan struct{}), cancelled: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &testGateway{address: listener.Addr().String(), host: host.signer.PublicKey(), keys: make(map[string]ssh.Signer), backend: backend, cancel: cancel, done: make(chan struct{})}
	cfg := gateway.Config{
		HostKeyPath: host.path,
		Handler:     &command.Handler{Backend: backend, Limits: core.Limits{MaxInstances: 4, CPU: 2, Memory: "1GiB", Processes: 512, Disk: "5GiB"}},
		MaxSessions: 8,
	}
	for _, p := range []core.Principal{alice, bob} {
		key := generatedKey(t, p.Name)
		s.keys[p.Name] = key.signer
		cfg.Users = append(cfg.Users, gateway.AuthorizedUser{Principal: p, AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(key.signer.PublicKey()))}})
	}
	go func() {
		s.err = gateway.Serve(ctx, listener, cfg)
		listener.Close()
		close(s.done)
	}()
	t.Cleanup(func() { s.stop(t) })
	return s
}

func (s *testGateway) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	await(t, s.done, "gateway shutdown")
	if s.err != nil {
		t.Fatalf("gateway Serve: %v", s.err)
	}
}

func (s *testGateway) dial(user string, key ssh.Signer, host ssh.PublicKey) (*ssh.Client, error) {
	raw, err := net.DialTimeout("tcp", s.address, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if err := raw.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		raw.Close()
		return nil, err
	}
	conn, channels, requests, err := ssh.NewClientConn(raw, s.address, &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host),
	})
	if err != nil {
		raw.Close()
		return nil, err
	}
	return ssh.NewClient(conn, channels, requests), nil
}

func (s *testGateway) connect(t *testing.T, principal core.Principal) *ssh.Client {
	t.Helper()
	client, err := s.dial(principal.Name, s.keys[principal.Name], s.host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

type response struct{ out, err string }

func run(t *testing.T, client *ssh.Client, line string, input []byte, wantStatus int) response {
	t.Helper()
	return runSession(t, client, line, input, wantStatus, false)
}

func runPTY(t *testing.T, client *ssh.Client, line string, input []byte, wantStatus int) response {
	t.Helper()
	return runSession(t, client, line, input, wantStatus, true)
}

func runSession(t *testing.T, client *ssh.Client, line string, input []byte, wantStatus int, pty bool) response {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdin, session.Stdout, session.Stderr = bytes.NewReader(input), &stdout, &stderr
	if pty {
		if err := session.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
			t.Fatalf("%q request PTY: %v", line, err)
		}
	}
	err = session.Run(line)
	status := 0
	if err != nil {
		var exit *ssh.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%q transport error: %v", line, err)
		}
		status = exit.ExitStatus()
	}
	if status != wantStatus {
		t.Fatalf("%q exit=%d want=%d stdout=%q stderr=%q", line, status, wantStatus, stdout.String(), stderr.String())
	}
	return response{stdout.String(), stderr.String()}
}

func decode(t *testing.T, text string, value any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), value); err != nil {
		t.Fatalf("decode response %q: %v", text, err)
	}
}

func assertInstance(t *testing.T, text, name string) {
	t.Helper()
	var item map[string]json.RawMessage
	decode(t, text, &item)
	var got string
	decode(t, string(item["vm_name"]), &got)
	if got != name || item["name"] != nil || item["status"] == nil {
		t.Fatalf("unexpected instance JSON: %s", text)
	}
}

func assertList(t *testing.T, response response, names ...string) {
	t.Helper()
	var object map[string]json.RawMessage
	decode(t, response.out, &object)
	var items []json.RawMessage
	decode(t, string(object["vms"]), &items)
	if items == nil || len(items) != len(names) || response.err != "" {
		t.Fatalf("list = %+v; want names %v and a non-null vms array", response, names)
	}
	for i, name := range names {
		assertInstance(t, string(items[i]), name)
	}
}

func assertStatus(t *testing.T, client *ssh.Client, name, want string) {
	t.Helper()
	var object struct {
		VMs []struct {
			Name   string `json:"vm_name"`
			Status string `json:"status"`
		} `json:"vms"`
	}
	decode(t, run(t, client, "ls --json", nil, 0).out, &object)
	for _, vm := range object.VMs {
		if vm.Name == name {
			if vm.Status != want {
				t.Fatalf("%s status = %q, want %q", name, vm.Status, want)
			}
			return
		}
	}
	t.Fatalf("instance %q missing from list", name)
}

func assertLifecycle(t *testing.T, response response, name string) {
	t.Helper()
	var object map[string]string
	decode(t, response.out, &object)
	if response.err != "" || !reflect.DeepEqual(object, map[string]string{"vm_name": name}) {
		t.Fatalf("lifecycle response = %+v", response)
	}
}

func assertPTYLineEndings(t *testing.T, text string) {
	t.Helper()
	if text == "" || !strings.HasSuffix(text, "\r\n") {
		t.Fatalf("PTY output lacks a final CRLF: %q", text)
	}
	if strings.Contains(text, "\r\r") {
		t.Fatalf("PTY output doubled a carriage return: %q", text)
	}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' && (i == 0 || text[i-1] != '\r') {
			t.Fatalf("bare newline at offset %d: %q", i, text)
		}
		if text[i] == '\r' && (i+1 == len(text) || text[i+1] != '\n') {
			t.Fatalf("bare carriage return at offset %d: %q", i, text)
		}
	}
}

func await(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

type execCall struct {
	principal core.Principal
	name      string
	command   string
	input     []byte
	pty       bool
}

type lifecycleCall struct {
	method    string
	principal core.Principal
	name      string
}

type memoryBackend struct {
	mu        sync.Mutex
	instances map[core.Principal]map[string]core.Instance
	calls     int
	exec      execCall
	create    core.CreateOptions
	lifecycle []lifecycleCall
	started   chan struct{}
	cancelled chan struct{}
}

var _ core.Backend = (*memoryBackend)(nil)

func (b *memoryBackend) List(_ context.Context, p core.Principal) ([]core.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	items := make([]core.Instance, 0, len(b.instances[p]))
	for _, item := range b.instances[p] {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func (b *memoryBackend) Images(_ context.Context, _ core.Principal) ([]core.ImageInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	items := make([]core.ImageInfo, 0, len(imageCatalog))
	for _, info := range imageCatalog {
		info.Aliases = append([]string(nil), info.Aliases...)
		items = append(items, info)
	}
	return items, nil
}

func (b *memoryBackend) Create(_ context.Context, p core.Principal, options core.CreateOptions) (core.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.create = options
	image, hasImage := core.ImageInfo{}, options.Image != ""
	if hasImage {
		image, hasImage = fixtureImage(options.Image)
		if !hasImage {
			return core.Instance{}, fmt.Errorf("image %q is not cached", options.Image)
		}
	}
	if b.instances[p] == nil {
		b.instances[p] = make(map[string]core.Instance)
	}
	if _, exists := b.instances[p][options.Name]; exists {
		return core.Instance{}, errors.New("already exists")
	}
	item := core.Instance{Name: options.Name, Status: "Running", CPU: "2", Memory: "1GiB", Disk: "5GiB"}
	if hasImage {
		item.Image = image.Fingerprint
	}
	if options.Processes > 0 {
		item.Processes = strconv.Itoa(options.Processes)
	}
	b.instances[p][item.Name] = item
	return item, nil
}

func (b *memoryBackend) Remove(_ context.Context, p core.Principal, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if _, exists := b.instances[p][name]; !exists {
		return errors.New("instance not owned by principal")
	}
	delete(b.instances[p], name)
	return nil
}

func (b *memoryBackend) Resume(_ context.Context, p core.Principal, name string) error {
	return b.transition(p, name, "resume", "Running")
}

func (b *memoryBackend) Start(_ context.Context, p core.Principal, name string) error {
	return b.transition(p, name, "start", "Running")
}

func (b *memoryBackend) Stop(_ context.Context, p core.Principal, name string) error {
	return b.transition(p, name, "stop", "Stopped")
}

func (b *memoryBackend) Restart(_ context.Context, p core.Principal, name string) error {
	return b.transition(p, name, "restart", "Running")
}

func (b *memoryBackend) transition(p core.Principal, name, method, status string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	items := b.instances[p]
	item, exists := items[name]
	if !exists {
		return errors.New("instance not owned by principal")
	}
	item.Status = status
	items[name] = item
	b.lifecycle = append(b.lifecycle, lifecycleCall{method, p, name})
	return nil
}

func (b *memoryBackend) Exec(ctx context.Context, p core.Principal, name, command string, streams core.Streams) (int, error) {
	b.mu.Lock()
	b.calls++
	_, owned := b.instances[p][name]
	b.mu.Unlock()
	if !owned {
		return 1, errors.New("instance not owned by principal")
	}
	if command == "wait-for-cancel" {
		close(b.started)
		<-ctx.Done()
		close(b.cancelled)
		return 1, ctx.Err()
	}
	input, err := io.ReadAll(streams.In)
	if err != nil {
		return 1, err
	}
	b.mu.Lock()
	b.exec = execCall{p, name, command, input, streams.PTY}
	b.mu.Unlock()
	switch command {
	case binaryCommand:
		if _, err := streams.Out.Write(append([]byte{'o', 'u', 't', 0, 255}, input...)); err != nil {
			return 1, err
		}
		_, err := streams.Err.Write([]byte{'e', 'r', 'r', 0, 254})
		return 37, err
	case `printf '%s' 'still connected'`:
		_, err := io.WriteString(streams.Out, "still connected")
		return 0, err
	default:
		return 1, fmt.Errorf("unexpected fake remote command %q", command)
	}
}

func (b *memoryBackend) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *memoryBackend) lastExec() execCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exec
}

func (b *memoryBackend) lastCreate() core.CreateOptions {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.create
}

func (b *memoryBackend) lifecycleCalls() []lifecycleCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]lifecycleCall(nil), b.lifecycle...)
}
