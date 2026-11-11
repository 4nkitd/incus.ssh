package incus

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
)

func TestOCIImageNormalization(t *testing.T) {
	for input, want := range map[string]string{
		"nginx:alpine":                                  "docker.io/library/nginx:alpine",
		"team/app:v2":                                   "docker.io/team/app:v2",
		"https://docker.io/nginx:alpine":                "docker.io/library/nginx:alpine",
		"ghcr.io/org/app:latest":                        "ghcr.io/org/app:latest",
		"https://registry.example.com:5000/team/app:v1": "registry.example.com:5000/team/app:v1",
	} {
		got, err := normalizeOCIImage(input)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", input, got, err)
		}
	}
}

func TestQuickPicksDoNotRestrictImageSelection(t *testing.T) {
	b, _ := hostingFixture(t)
	b.config.OCIImages = nil
	caps, err := b.HostingCapabilities(context.Background(), alice)
	if err != nil || !caps.Available || len(caps.Images) != 0 {
		t.Fatal("quick picks incorrectly required", err)
	}
	app, err := b.CreateApp(context.Background(), alice, core.AppOptions{Name: "custom", Image: "owner/custom-app:v3", Memory: "512MiB"})
	if err != nil || app.AppImage != "docker.io/owner/custom-app:v3" {
		t.Fatal("free-form Hub image rejected", err)
	}
}

func TestCustomRegistryUsesOnlyConfiguredRemote(t *testing.T) {
	b, dir := hostingFixture(t)
	b.config.OCIRegistries = map[string]string{"ghcr.io": "github"}
	writeJSON(t, filepath.Join(dir, "remotes.json"), map[string]any{
		"docker": map[string]any{"Addrs": []string{"https://docker.io"}, "Protocol": "oci", "Public": true},
		"github": map[string]any{"Addrs": []string{"https://ghcr.io"}, "Protocol": "oci", "Public": true},
	})
	app, err := b.CreateApp(context.Background(), alice, core.AppOptions{Name: "custom", Image: "https://ghcr.io/acme/web:v1", Memory: "512MiB"})
	if err != nil || app.AppImage != "ghcr.io/acme/web:v1" {
		t.Fatal(app, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "argv.json"))
	if !strings.Contains(string(args), "github:acme/web:v1") || strings.Contains(string(args), "https://") {
		t.Fatal("URL escaped the configured remote mapping")
	}
	if _, err := b.CreateApp(context.Background(), alice, core.AppOptions{Name: "escape", Image: "https://unknown.example/team/app:v1"}); err == nil {
		t.Fatal("unapproved host accepted")
	}
}

func TestRegistryConfigurationValidation(t *testing.T) {
	for _, host := range []string{"docker.io", "ghcr.io", "registry.example.com:5000"} {
		if !validOCIRegistry(host) {
			t.Errorf("valid registry rejected: %s", host)
		}
	}
	for _, host := range []string{"localhost", "127.0.0.1", "https://ghcr.io", "user@ghcr.io", "ghcr.io/path", "evil.example:0", "evil.example:65536", "-evil.example", "evil.example\n"} {
		if validOCIRegistry(host) {
			t.Errorf("unsafe registry accepted: %s", host)
		}
	}
}
