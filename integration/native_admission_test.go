package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/incus"
)

func TestNativeRunningOnlyAdmission(t *testing.T) {
	project := os.Getenv("IFD_NATIVE_RUNTIME_PROJECT")
	if project == "" {
		t.Skip("requires a disposable project without native aggregate inventory quotas")
	}
	p := core.Principal{Name: "test", Project: project}
	b, err := incus.New(incus.Config{AdmissionDirectory: filepath.Join(t.TempDir(), "admission"), Binary: "/usr/bin/incus", Profile: "ifd", Image: "local:unused", OCIRemote: "docker", Limits: core.Limits{MaxInstances: 0, RunningMemory: "1GiB", CPU: 1, Memory: "512MiB", Processes: 128}, OperationTimeout: 180 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	create := func(name string) {
		t.Helper()
		_, err := b.CreateApp(ctx, p, core.AppOptions{Name: name, Image: "alpine:latest", Memory: "512MiB", DevelopmentBox: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := b.Remove(cleanup, p, name); err != nil {
				t.Error(err)
			}
		})
	}
	create("runtime-a")
	create("runtime-b")
	if _, err := b.CreateApp(ctx, p, core.AppOptions{Name: "runtime-c", Image: "alpine:latest", Memory: "512MiB", DevelopmentBox: true}); err == nil {
		t.Fatal("third active container exceeded runtime budget")
	}
	if err := b.Stop(ctx, p, "runtime-a"); err != nil {
		t.Fatal(err)
	}
	create("runtime-c")
	if err := b.Start(ctx, p, "runtime-a"); err == nil {
		t.Fatal("stopped start bypassed running budget")
	}
	if err := b.Stop(ctx, p, "runtime-b"); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(ctx, p, "runtime-a"); err != nil {
		t.Fatal("released running capacity was not reusable", err)
	}
	if err := b.Restart(ctx, p, "runtime-a"); err != nil {
		t.Fatal("restart was double-counted", err)
	}
	items, err := b.List(ctx, p)
	if err != nil || len(items) != 3 {
		t.Fatal("stopped inventory was removed or capped", err)
	}
	running := 0
	for _, item := range items {
		if item.Status == "Running" {
			running++
		}
	}
	if running != 2 {
		t.Fatalf("expected 2 running among 3 stored containers, got %d", running)
	}
}
