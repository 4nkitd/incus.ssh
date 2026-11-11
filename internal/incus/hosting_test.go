package incus

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"incus-for-devs/internal/core"
)

func TestOCIReferences(t *testing.T) {
	for _, ref := range []string{"nginx:alpine", "docker.io/library/nginx:1.27", "owner/app:v2", "nginx@sha256:" + strings.Repeat("a", 64), "nginx:alpine@sha256:" + strings.Repeat("b", 64), "team/api--worker:v1", "team/api__worker:v1", "ghcr.io/team/app:v1", "https://ghcr.io/team/app:v1", "registry.example.com:5000/team/app:tag"} {
		if _, err := normalizeOCIImage(ref); err != nil {
			t.Errorf("valid ref %q: %v", ref, err)
		}
	}
	for _, ref := range []string{"", "--help", "http://docker.io/nginx", "localhost/app", "https://user:password@ghcr.io/org/app", "https://ghcr.io/org/app?token=bad", "127.0.0.1/app", "registry:5000/app", "../app", "foo//app", "nginx;id", "name\nvalue", "nginx:tag@sha256:bad"} {
		if _, err := normalizeOCIImage(ref); err == nil {
			t.Errorf("accepted unsafe ref %q", ref)
		}
	}
}

func hostingFixture(t *testing.T) (*Backend, string) {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "incus")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexport GORACE=atexit_sleep_ms=0\nexec "+quote(binary)+" -test.run=^TestHostingCLIHelper$ -- "+quote(dir)+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "server.json"), map[string]any{"api_extensions": []string{"instance_oci", "instance_oci_entrypoint"}, "environment": map[string]string{"server_version": "7.0.2"}})
	writeJSON(t, filepath.Join(dir, "remotes.json"), map[string]any{"docker": map[string]any{"Addrs": []string{"https://docker.io"}, "Protocol": "oci", "Public": true}})
	writeJSON(t, filepath.Join(dir, "instances.json"), []instance{})
	b, err := New(Config{Binary: path, Profile: "fixed", Image: "local:unused", OCIRemote: "docker", OCIImages: []string{"nginx:alpine"}, Limits: core.Limits{MaxInstances: 2, CPU: 2, Memory: "2GiB", Processes: 256}, OperationTimeout: 5 * time.Second, AvailableMemory: func() (uint64, error) { return 16 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return b, dir
}

func TestHostingCLIHelper(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	dir, args := os.Args[separator+1], os.Args[separator+2:]
	if len(args) < 3 {
		os.Exit(1)
	}
	if args[0] != "--force-local" {
		os.Exit(2)
	}
	action := args[2]
	if args[1] == "--project" {
		if args[2] != alice.Project {
			os.Exit(3)
		}
		action = args[4]
		args = args[4:]
	} else {
		args = args[2:]
	}
	switch action {
	case "query":
		data, _ := os.ReadFile(filepath.Join(dir, "server.json"))
		_, _ = os.Stdout.Write(data)
	case "remote":
		data, _ := os.ReadFile(filepath.Join(dir, "remotes.json"))
		_, _ = os.Stdout.Write(data)
	case "list":
		data, _ := os.ReadFile(filepath.Join(dir, "instances.json"))
		_, _ = os.Stdout.Write(data)
	case "launch":
		data, _ := io.ReadAll(os.Stdin)
		_ = os.WriteFile(filepath.Join(dir, "stdin.json"), data, 0600)
		encoded, _ := json.Marshal(args)
		_ = os.WriteFile(filepath.Join(dir, "argv.json"), encoded, 0600)
		var body struct {
			Config map[string]string `json:"config"`
		}
		if json.Unmarshal(data, &body) != nil {
			os.Exit(4)
		}
		item := instance{Name: args[2], Type: "container", Status: "Running", Project: alice.Project, Config: body.Config}
		encoded, _ = json.Marshal([]instance{item})
		_ = os.WriteFile(filepath.Join(dir, "instances.json"), encoded, 0600)
	default:
		os.Exit(5)
	}
	os.Exit(0)
}

func TestCreateAppKeepsEnvironmentOutOfArguments(t *testing.T) {
	b, dir := hostingFixture(t)
	app, err := b.CreateApp(context.Background(), alice, core.AppOptions{Name: "website", Image: "nginx:alpine", Memory: "512MiB", Environment: map[string]string{"APP_SECRET": "fixture-secret;not-a-command"}})
	if err != nil {
		t.Fatal(err)
	}
	if app.Kind != "application" || app.AppImage != "docker.io/library/nginx:alpine" {
		t.Fatalf("wrong app projection: %+v", app)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "argv.json"))
	if strings.Contains(string(args), "fixture-secret") || !strings.Contains(string(args), "docker:library/nginx:alpine") {
		t.Fatal("unsafe launch arguments")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "stdin.json"))
	var body struct{ Config map[string]string }
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{ownerKey: alice.Name, "security.privileged": "false", "security.nesting": "false", "security.guestapi": "false", "boot.autostart": "false", "limits.memory": "512MiB", "environment.APP_SECRET": "fixture-secret;not-a-command"} {
		if body.Config[key] != want {
			t.Errorf("wrong %s", key)
		}
	}
	encoded, _ := json.Marshal(app)
	if strings.Contains(string(encoded), "fixture-secret") {
		t.Fatal("environment returned to client")
	}
}

func TestHostingCapabilitiesFailClosed(t *testing.T) {
	b, dir := hostingFixture(t)
	writeJSON(t, filepath.Join(dir, "server.json"), map[string]any{"api_extensions": []string{}, "environment": map[string]string{"server_version": "6.0.7"}})
	caps, err := b.HostingCapabilities(context.Background(), alice)
	if err != nil || caps.Available || caps.ServerVersion != "6.0.7" || caps.Reason == "" {
		t.Fatal(caps, err)
	}
	if _, err := b.CreateApp(context.Background(), alice, core.AppOptions{Name: "demo", Image: "nginx:alpine"}); err == nil {
		t.Fatal("unsupported server launched image")
	}
	writeJSON(t, filepath.Join(dir, "server.json"), map[string]any{"api_extensions": []string{"instance_oci"}})
	writeJSON(t, filepath.Join(dir, "remotes.json"), map[string]any{"docker": map[string]any{"Addrs": []string{"http://127.0.0.1"}, "Protocol": "oci", "Public": true}})
	caps, err = b.HostingCapabilities(context.Background(), alice)
	if err != nil || caps.Available {
		t.Fatal("unsafe remote accepted", err)
	}
}

func TestAppValidationAndCapacity(t *testing.T) {
	b, dir := hostingFixture(t)
	for _, in := range []core.AppOptions{
		{Name: "--help", Image: "nginx"}, {Name: "demo", Image: "ghcr.io/private/app"}, {Name: "demo", Image: "nginx", CPU: 100},
		{Name: "demo", Image: "nginx", Environment: map[string]string{"bad.key": "value"}},
		{Name: "demo", Image: "nginx", Environment: map[string]string{"OK": "value\x00"}},
	} {
		if _, err := b.CreateApp(context.Background(), alice, in); err == nil {
			t.Fatal("invalid deployment accepted")
		}
	}
	writeJSON(t, filepath.Join(dir, "instances.json"), []instance{ownedInstance("one", alice.Name), ownedInstance("two", alice.Name)})
	if _, err := b.CreateApp(context.Background(), alice, core.AppOptions{Name: "demo", Image: "nginx"}); err == nil {
		t.Fatal("instance cap bypassed")
	}
	if _, err := os.Stat(filepath.Join(dir, "argv.json")); !os.IsNotExist(err) {
		t.Fatal("invalid request reached launch")
	}
}

func TestAppPublicationIsBoundToBoot(t *testing.T) {
	b, _ := hostingFixture(t)
	item := ownedInstance("web", alice.Name)
	item.Config[appImageKey] = "library/nginx:alpine"
	item.Config["volatile.uuid"] = "12345678-1234-1234-1234-123456789abc"
	item.LastUsedAt = time.Now().UTC()
	item.Config[appPortKey] = "80"
	item.Config[appBootKey] = instanceBootID(item)
	if b.summary(item).HTTPPort != 80 {
		t.Fatal("valid publication not displayed")
	}
	item.LastUsedAt = item.LastUsedAt.Add(time.Second)
	if b.summary(item).HTTPPort != 0 {
		t.Fatal("new boot inherited publication")
	}
}

func TestTailBufferIsBounded(t *testing.T) {
	b := &tailBuffer{limit: 10}
	_, _ = b.Write([]byte("0123456789"))
	_, _ = b.Write([]byte("abc"))
	if string(b.data) != "3456789abc" {
		t.Fatal(string(b.data))
	}
	_, _ = b.Write([]byte("0123456789XYZ"))
	if string(b.data) != "3456789XYZ" {
		t.Fatal(string(b.data))
	}
}
