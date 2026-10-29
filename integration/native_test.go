package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"incus-for-devs/internal/command"
	"incus-for-devs/internal/core"
	"incus-for-devs/internal/gateway"
	"incus-for-devs/internal/incus"
	"incus-for-devs/internal/sharing"
)

func TestNativeIncusSCPAndPrivateRelay(t *testing.T) {
	if os.Getenv("INCUS_FOR_DEVS_NATIVE_QA") != "1" {
		t.Skip("requires an explicitly prepared disposable native-Incus QA host")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("native QA requires root on the disposable Linux host")
	}
	if _, err := os.Stat("/etc/incus-for-devs-disposable-qa"); err != nil {
		t.Fatal("disposable-host marker missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	p := core.Principal{Name: "qa", Project: "ifd-qa"}
	backend, err := incus.New(incus.Config{Binary: "/usr/bin/incus", Image: "local:ifd-qa-ubuntu", Profile: "ifd", Limits: core.Limits{MaxInstances: 1, CPU: 1, Memory: "1GiB", Processes: 256}, OperationTimeout: 120 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	items, err := backend.List(ctx, p)
	if err != nil || len(items) != 0 {
		t.Fatal("refusing to use a QA project with pre-existing owned guests", err)
	}
	const name = "ifd-qa-guest"
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		items, err := backend.List(cleanup, p)
		if err != nil {
			t.Error("could not inspect partial QA guest creation", err)
			return
		}
		for _, item := range items {
			if item.Name == name {
				if err := backend.Remove(cleanup, p, name); err != nil {
					t.Error("could not remove the disposable QA guest", err)
				}
			}
		}
	})
	guest, err := backend.Create(ctx, p, core.CreateOptions{Name: name, CPU: 1, Memory: "512MiB", Processes: 256})
	if err != nil {
		t.Fatal(err)
	}
	if guest.Name != name {
		t.Fatal(guest)
	}
	execGuest := func(line string) {
		t.Helper()
		var output bytes.Buffer
		code, err := backend.Exec(ctx, p, name, line, core.Streams{Out: &output, Err: &output})
		if err != nil || code != 0 {
			t.Fatalf("guest command failed: %v, exit %d\n%s", err, code, output.String())
		}
	}
	execGuest("export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq openssh-sftp-server busybox-static")
	relay := newNativeRelay(t)
	manager := sharing.New(ctx, backend, sharing.Options{API: relay.server.URL, Directory: filepath.Join(t.TempDir(), "identity")})
	defer manager.Close()
	host, key := generatedKey(t, "native-host"), generatedKey(t, "native-user")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- gateway.Serve(serveCtx, listener, gateway.Config{HostKeyPath: host.path, Users: []gateway.AuthorizedUser{{Principal: p, AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(key.signer.PublicKey()))}}}, Handler: &command.Handler{Backend: backend, Files: backend, Sharing: manager}, MaxSessions: 8})
	}()
	defer func() {
		stop()
		listener.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{User: p.Name, Auth: []ssh.AuthMethod{ssh.PublicKeys(key.signer)}, HostKeyCallback: ssh.FixedHostKey(host.signer.PublicKey()), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	files, err := sftp.NewClient(client)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	dir := t.TempDir()
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte(knownhosts.Line([]string{knownhosts.Normalize(listener.Addr().String())}, host.signer.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	address, port, _ := net.SplitHostPort(listener.Addr().String())
	remote := p.Name + "@" + address + ":" + name + "/root/"
	copyFile := func(args ...string) {
		t.Helper()
		copyCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		flags := []string{"-F", "/dev/null", "-P", port, "-i", key.path, "-o", "BatchMode=yes", "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UserKnownHostsFile=" + known}
		output, err := exec.CommandContext(copyCtx, "scp", append(flags, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("native stock SCP failed: %v\n%s", err, output)
		}
	}
	for _, mode := range []os.FileMode{0600, 0700} {
		file := filepath.Join(dir, "binary ' spaced.bin")
		payload := bytes.Repeat([]byte{0, 255, 1, 128, 10, 13}, 20000)
		if err := os.WriteFile(file, payload, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
		copyFile("-p", file, remote+filepath.Base(file))
		info, err := files.Stat("/" + name + "/root/" + filepath.Base(file))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("native SCP permissions: got %v, want %v, err=%v", info, mode, err)
		}
		download := filepath.Join(dir, "download.bin")
		copyFile(remote+filepath.Base(file), download)
		got, err := os.ReadFile(download)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatal("native SCP binary round trip failed", err)
		}
	}
	tree := filepath.Join(dir, "project")
	os.MkdirAll(filepath.Join(tree, "nested"), 0700)
	os.WriteFile(filepath.Join(tree, "nested", "source.txt"), []byte("native recursive copy\n"), 0600)
	copyFile("-r", tree, remote)
	copyFile("-r", remote+"project", filepath.Join(dir, "retrieved"))
	if data, err := os.ReadFile(filepath.Join(dir, "retrieved", "nested", "source.txt")); err != nil || string(data) != "native recursive copy\n" {
		t.Fatal("native recursive copy failed", err)
	}
	execGuest("mkdir -p /tmp/qa-target; ln -s /tmp/qa-target /root/qa-link; printf guest-symlink > /tmp/qa-sentinel; printf wrong-lexical-path > /root/qa-sentinel")
	symlinkFile, err := files.Open("/" + name + "/root/qa-link/../qa-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	symlinkData, err := io.ReadAll(symlinkFile)
	symlinkFile.Close()
	if err != nil || string(symlinkData) != "guest-symlink" {
		t.Fatal("native symlink path was resolved on the host", err)
	}
	if file, err := files.Open("/etc/passwd"); err == nil {
		file.Close()
		t.Fatal("SFTP reached an unselected host path")
	}
	if file, err := backend.OpenSFTP(ctx, core.Principal{Name: "other", Project: p.Project}, name); err == nil {
		file.Close()
		t.Fatal("native file ownership check failed")
	}
	startHTTP := func() {
		execGuest("mkdir -p /root/site; printf native-https-test > /root/site/index.html; nohup /bin/busybox httpd -f -p 3000 -h /root/site >/root/qa-http.log 2>&1 </dev/null &")
		for attempt := 0; attempt < 40; attempt++ {
			id, err := backend.ServiceIdentity(ctx, p, name)
			if err == nil {
				connection, err := backend.DialService(ctx, p, name, id, 3000)
				if err == nil {
					connection.Close()
					return
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatal("native guest HTTP service did not become reachable")
	}
	startHTTP()
	if response := run(t, client, "share list --json", nil, 0); response.out != "{\"shares\":[]}\n" {
		t.Fatal("native gateway was not private by default", response)
	}
	var share core.Share
	decode(t, run(t, client, "share enable "+name+" 3000 --json", nil, 0).out, &share)
	if body, err := relay.visit(share.URL); err != nil || !strings.Contains(body, "native-https-test") {
		t.Fatal("native guest HTTPS relay round trip failed", body, err)
	}
	if body, err := relay.visit(share.URL); err != nil || !strings.Contains(body, "native-https-test") || relay.sessions.Load() != 1 {
		t.Fatal("visitor cleanup disrupted the shared bridge", body, err)
	}
	run(t, client, "share disable "+name+" 3000", nil, 0)
	if _, err := relay.visit(share.URL); err == nil {
		t.Fatal("disabled native route still served traffic")
	}
	decode(t, run(t, client, "share enable "+name+" 3000 --json", nil, 0).out, &share)
	before, err := backend.ServiceIdentity(ctx, p, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Restart(ctx, p, name); err != nil {
		t.Fatal(err)
	}
	files.Close()
	files, err = sftp.NewClient(client)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	after, err := backend.ServiceIdentity(ctx, p, name)
	if err != nil || before == after {
		t.Fatal("native reboot did not change trusted boot identity", err)
	}
	startHTTP()
	if _, err := relay.visit(share.URL); err == nil {
		t.Fatal("native reboot inherited exposure consent")
	}
	if persisted, err := files.Open("/" + name + "/root/project/nested/source.txt"); err != nil {
		t.Fatal("native copied data did not survive reboot", err)
	} else {
		persisted.Close()
	}
	if shares, err := manager.List(ctx, p, ""); err != nil || len(shares) != 0 {
		t.Fatal("stale boot route was not revoked", shares, err)
	}
	_, err = backend.DialService(ctx, p, name, before, 3000)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatal("old native boot approval was accepted", err)
	}
	var metadata struct {
		VMs []core.Instance `json:"vms"`
	}
	if err := json.Unmarshal([]byte(run(t, client, "ls --json", nil, 0).out), &metadata); err != nil || len(metadata.VMs) != 1 {
		t.Fatal("native inventory changed unexpectedly", err)
	}
}
