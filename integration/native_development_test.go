package integration_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/incus"
)

func TestNativeDevelopmentBoxes(t *testing.T) {
	project := os.Getenv("IFD_NATIVE_HOSTING_PROJECT")
	if project == "" || os.Getenv("IFD_NATIVE_DEVELOPMENT_BOXES") != "1" {
		t.Skip("requires explicit disposable-host development-box verification")
	}
	p := core.Principal{Name: "test", Project: project}
	b, err := incus.New(incus.Config{Binary: "/usr/bin/incus", Profile: "ifd", Image: "local:unused", OCIRemote: "docker", Limits: core.Limits{MaxInstances: 2, CPU: 1, Memory: "512MiB", Processes: 128}, OperationTimeout: 180 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	commands := map[string]string{"kali": "grep '^ID=' /etc/os-release", "alpine": "cat /etc/alpine-release", "go-dev": "go version", "php-dev": "php --version", "python-dev": "python --version && pip --version"}
	for _, preset := range core.DevelopmentPresets() {
		t.Run(preset.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			app, err := b.CreateApp(ctx, p, core.AppOptions{Name: "qa-" + preset.ID, Image: preset.Image, Memory: "512MiB", DevelopmentBox: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := b.Remove(cleanup, p, app.Name); err != nil {
					t.Error(err)
				}
			}()
			if app.Status != "Running" || !app.DevelopmentBox {
				t.Fatalf("development box is not running: %+v", app)
			}
			var output, stderr bytes.Buffer
			code, err := b.Exec(ctx, p, app.Name, commands[preset.ID], core.Streams{Out: &output, Err: &stderr})
			if err != nil || code != 0 {
				t.Fatalf("toolchain probe failed: exit=%d error=%v stderr=%s", code, err, stderr.String())
			}
			if strings.TrimSpace(output.String()) == "" {
				t.Fatal("toolchain produced no version output")
			}
			t.Log(strings.TrimSpace(output.String()))
		})
	}
}
