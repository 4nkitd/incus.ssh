package incus

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"incus-for-devs/internal/core"
)

func TestDevelopmentBoxUsesFixedGuestStartup(t *testing.T) {
	b, dir := hostingFixture(t)
	app, err := b.CreateApp(context.Background(), alice, core.AppOptions{Name: "dev", Image: "alpine:latest", Memory: "512MiB", DevelopmentBox: true})
	if err != nil || !app.DevelopmentBox {
		t.Fatal(app, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "stdin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Config map[string]string }
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body.Config["oci.entrypoint"] != `/bin/sh -c "exec sleep infinity"` || body.Config[developmentBoxKey] != "true" {
		t.Fatal("incorrect development startup")
	}
	caps, err := b.HostingCapabilities(context.Background(), alice)
	if err != nil || !caps.DevelopmentBoxes || len(caps.Presets) != 5 {
		t.Fatal("missing development presets", err)
	}
}

func TestDevelopmentBoxRequiresEntrypointExtension(t *testing.T) {
	b, dir := hostingFixture(t)
	writeJSON(t, filepath.Join(dir, "server.json"), map[string]any{"api_extensions": []string{"instance_oci"}})
	if _, err := b.CreateApp(context.Background(), alice, core.AppOptions{Name: "dev", Image: "alpine:latest", DevelopmentBox: true}); err == nil {
		t.Fatal("unsupported override accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "argv.json")); !os.IsNotExist(err) {
		t.Fatal("unsupported request reached launch")
	}
}
