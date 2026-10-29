package integration_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh/knownhosts"

	"incus-for-devs/internal/core"
)

type fileKey struct {
	user core.Principal
	name string
}

func (b *memoryBackend) OpenSFTP(ctx context.Context, p core.Principal, name string) (io.ReadWriteCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item, ok := b.instances[p][name]
	if !ok || item.Status != "Running" {
		return nil, errors.New("instance not owned or not running")
	}
	if b.files == nil {
		b.files = make(map[fileKey]sftp.Handlers)
	}
	key := fileKey{p, name}
	handlers, ok := b.files[key]
	if !ok {
		handlers = sftp.InMemHandler()
		b.files[key] = handlers
	}
	left, right := net.Pipe()
	go func() {
		server := sftp.NewRequestServer(right, handlers)
		defer server.Close()
		stop := context.AfterFunc(ctx, func() { server.Close() })
		defer stop()
		server.Serve()
	}()
	return left, nil
}

func TestSFTPFileTransferAndOwnershipIsolation(t *testing.T) {
	s := startGateway(t)
	client := s.connect(t, alice)
	other := s.connect(t, bob)
	run(t, client, "new --name=demo", nil, 0)
	run(t, other, "new --name=foreign", nil, 0)
	files, err := sftp.NewClient(client)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	entries, err := files.ReadDir("/")
	if err != nil || len(entries) != 1 || entries[0].Name() != "demo" {
		t.Fatal("virtual root exposed another user's machine or host files", entries, err)
	}
	if err := files.MkdirAll("/demo/root/project"); err != nil {
		t.Fatal(err)
	}
	name := "/demo/root/project/space ' quote ; $file.bin"
	file, err := files.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0, 255, 10, 13, 1, 128}, 12000)
	if _, err := file.WriteAt(payload[1000:], 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(payload[:1000], 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	read, err := files.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(read)
	read.Close()
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatal("SFTP changed file bytes or offsets", err)
	}
	if err := files.Mkdir("/demo/etc"); err != nil {
		t.Fatal(err)
	}
	sentinel, err := files.Create("/demo/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sentinel.Write([]byte("guest-only sentinel\n")); err != nil {
		t.Fatal(err)
	}
	sentinel.Close()
	guest, err := files.Open("/demo/../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	guestData, err := io.ReadAll(guest)
	guest.Close()
	if err != nil || string(guestData) != "guest-only sentinel\n" {
		t.Fatal("dot-dot resolution did not remain inside the selected guest", err)
	}
	for _, file := range []string{"/foreign/root/data", "/etc/passwd", "/proc/self/environ"} {
		if remote, err := files.Open(file); err == nil {
			remote.Close()
			t.Fatalf("read outside owned container: %s", file)
		}
	}
	if err := files.Rename(name, "/foreign/root/stolen"); err == nil {
		t.Fatal("cross-container rename accepted")
	}
	if err := files.PosixRename(name, "/foreign/root/stolen"); err == nil {
		t.Fatal("cross-container POSIX rename accepted")
	}
	if err := files.RemoveDirectory("/demo"); err == nil {
		t.Fatal("SFTP removed a container selector")
	}
	if resolved, err := files.RealPath("demo/root/project"); err != nil || resolved != "/demo/root/project" {
		t.Fatal("realpath escaped the container selector", resolved, err)
	}
}

func TestStockSCPUploadDownloadAndRecursiveCopy(t *testing.T) {
	scp, err := exec.LookPath("scp")
	if err != nil {
		t.Skip("stock scp not installed")
	}
	s := startGateway(t)
	client := s.connect(t, alice)
	run(t, client, "new --name=demo", nil, 0)
	files, err := sftp.NewClient(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Mkdir("/demo/root"); err != nil {
		t.Fatal(err)
	}
	files.Close()
	dir := t.TempDir()
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte(knownhosts.Line([]string{knownhosts.Normalize(s.address)}, s.host)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(s.address)
	if err != nil {
		t.Fatal(err)
	}
	remote := "alice@" + host + ":demo/root/"
	copyFile := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		flags := []string{"-F", "/dev/null", "-P", port, "-i", s.keyFiles[alice.Name], "-o", "BatchMode=yes", "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UserKnownHostsFile=" + known}
		output, err := exec.CommandContext(ctx, scp, append(flags, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("stock scp %v failed: %v\n%s", args, err, output)
		}
	}
	payload := bytes.Repeat([]byte{0, 1, 255, 128, 10, 13}, 10000)
	name := "binary ' spaced.bin"
	source := filepath.Join(dir, name)
	if err := os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	copyFile(source, remote+name)
	destination := filepath.Join(dir, "download.bin")
	copyFile(remote+name, destination)
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("stock SCP round trip changed bytes", err)
	}
	tree := filepath.Join(dir, "project")
	if err := os.MkdirAll(filepath.Join(tree, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "nested", "source.txt"), []byte("recursive transfer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	copyFile("-r", tree, remote)
	output := filepath.Join(dir, "retrieved")
	copyFile("-r", remote+"project", output)
	got, err = os.ReadFile(filepath.Join(output, "nested", "source.txt"))
	if err != nil || string(got) != "recursive transfer\n" {
		t.Fatal("stock SCP recursive transfer failed", err)
	}
}
