package integration_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"incus-for-devs/internal/controlapi"
	"incus-for-devs/internal/core"
	"incus-for-devs/internal/incus"
)

func TestNativeOCIHosting(t *testing.T) {
	project := os.Getenv("IFD_NATIVE_HOSTING_PROJECT")
	if project == "" {
		t.Skip("requires an explicitly prepared disposable Incus OCI project")
	}
	p := core.Principal{Name: "test", Project: project}
	config := incus.Config{Binary: "/usr/bin/incus", Profile: "ifd", Image: "local:unused", OCIRemote: "docker", OCIImages: []string{"nginx:alpine"}, Limits: core.Limits{MaxInstances: 2, CPU: 1, Memory: "512MiB", Processes: 128}, OperationTimeout: 180 * time.Second}
	if os.Getenv("IFD_NATIVE_HOSTING_CUSTOM_IMAGE") != "" {
		config.OCIRegistries = map[string]string{"ghcr.io": "github"}
	}
	backend, err := incus.New(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	caps, err := backend.HostingCapabilities(ctx, p)
	if err != nil || !caps.Available {
		t.Fatalf("capabilities: %+v %v", caps, err)
	}
	app, err := backend.CreateApp(ctx, p, core.AppOptions{Name: "qa-web", Image: "nginx:alpine", Memory: "256MiB", Environment: map[string]string{"IFD_MARKER": "native-oci"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := backend.Remove(cleanup, p, app.Name); err != nil {
			t.Error(err)
		}
	})
	if app.Kind != "application" || app.Status != "Running" {
		t.Fatalf("unexpected app %+v", app)
	}
	var out bytes.Buffer
	code, err := backend.Exec(ctx, p, app.Name, `printf '%s' "$IFD_MARKER"; printf 'persisted' > /tmp/ifd-ui-marker`, core.Streams{Out: &out, Err: io.Discard})
	if err != nil || code != 0 || out.String() != "native-oci" {
		t.Fatalf("environment/exec failed: %v %d", err, code)
	}
	if err := backend.PublishApp(ctx, p, app.Name, 80); err != nil {
		t.Fatal(err)
	}
	tokens, err := controlapi.OpenTokens(filepath.Join(t.TempDir(), "state"), "https://panel.example.test", p)
	if err != nil {
		t.Fatal(err)
	}
	defer tokens.Close()
	handler := controlapi.NewHandler(tokens, backend, nil, 30*time.Second, 8, "test", "apps.example.test")
	server := httptest.NewServer(handler)
	defer server.Close()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/", nil)
	req.Host = app.Name + ".apps.example.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "nginx") {
		t.Fatalf("app proxy returned %d", resp.StatusCode)
	}
	if _, err := backend.AppLogs(ctx, p, app.Name); err != nil {
		t.Fatal(err)
	}
	details, err := backend.InspectApp(ctx, p, app.Name)
	if err != nil || details.Metrics.MemoryBytes == nil {
		t.Fatalf("live metrics: %v", err)
	}
	value := "updated-marker"
	if err := backend.UpdateEnvironment(ctx, p, app.Name, core.EnvironmentUpdate{Values: map[string]*string{"IFD_MARKER": &value}}); err != nil {
		t.Fatal(err)
	}
	details, err = backend.InspectApp(ctx, p, app.Name)
	if err != nil || !details.EnvironmentPendingRestart {
		t.Fatalf("missing pending restart state: %v", err)
	}
	if err := backend.Stop(ctx, p, app.Name); err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(ctx, p, app.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.AppTarget(ctx, p, app.Name); err == nil {
		t.Fatal("publication survived a new boot")
	}
	out.Reset()
	code, err = backend.Exec(ctx, p, app.Name, "cat /tmp/ifd-ui-marker", core.Streams{Out: &out, Err: io.Discard})
	if err != nil || code != 0 || out.String() != "persisted" {
		t.Fatalf("data did not survive stop/start: %v %d", err, code)
	}
	out.Reset()
	code, err = backend.Exec(ctx, p, app.Name, `printf '%s' "$IFD_MARKER"`, core.Streams{Out: &out, Err: io.Discard})
	if err != nil || code != 0 || out.String() != value {
		t.Fatal("environment update was not applied", err)
	}
	details, err = backend.InspectApp(ctx, p, app.Name)
	if err != nil || details.EnvironmentPendingRestart {
		t.Fatal("restart did not clear pending state", err)
	}
	if image := os.Getenv("IFD_NATIVE_HOSTING_CUSTOM_IMAGE"); image != "" {
		custom, err := backend.CreateApp(ctx, p, core.AppOptions{Name: "qa-custom", Image: image, Memory: "256MiB"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := backend.Remove(cleanup, p, custom.Name); err != nil {
				t.Error(err)
			}
		})
		if custom.Kind != "application" || custom.Status != "Running" || !strings.HasPrefix(custom.AppImage, "ghcr.io/") {
			t.Fatalf("custom registry deployment: %+v", custom)
		}
	}
}
