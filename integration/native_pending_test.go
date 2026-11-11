package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/incus"
)

func TestNativeCancelledStartKeepsCapacity(t *testing.T) {
	project := os.Getenv("IFD_NATIVE_PENDING_PROJECT")
	if project == "" {
		t.Skip("requires an isolated test project allowing a trusted host pre-start hook")
	}
	p := core.Principal{Name: "test", Project: project}
	b, err := incus.New(incus.Config{AdmissionDirectory: filepath.Join(t.TempDir(), "admission"), Binary: "/usr/bin/incus", Profile: "ifd", Image: "local:unused", OCIRemote: "docker", Limits: core.Limits{RunningMemory: "512MiB", CPU: 1, Memory: "512MiB", Processes: 128}, OperationTimeout: 180 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	create := func(name string) error {
		_, err := b.CreateApp(ctx, p, core.AppOptions{Name: name, Image: "alpine:latest", Memory: "512MiB", DevelopmentBox: true})
		return err
	}
	cleanup := func(name string) {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := b.Remove(ctx, p, name); err != nil {
				t.Error(err)
			}
		})
	}
	if err := create("pending-a"); err != nil {
		t.Fatal(err)
	}
	cleanup("pending-a")
	if err := b.Stop(ctx, p, "pending-a"); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/var/tmp", "ifd-pending-hook-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, "delay-start")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n/bin/sleep 8\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/incus", "--force-local", "--project", project, "config", "set", "pending-a", "raw.lxc=lxc.hook.pre-start = "+hook)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configure isolated hook: %v %s", err, output)
	}
	startCtx, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	done := make(chan error, 1)
	go func() { done <- b.Start(startCtx, p, "pending-a") }()
	pending := func() bool {
		output, err := exec.CommandContext(ctx, "/usr/bin/incus", "--force-local", "--project", project, "operation", "list", "--format=json").Output()
		if err != nil {
			t.Fatal(err)
		}
		var operations []struct {
			Class      string `json:"class"`
			StatusCode int    `json:"status_code"`
		}
		if err := json.Unmarshal(output, &operations); err != nil {
			t.Fatal(err)
		}
		for _, operation := range operations {
			if operation.Class == "task" && operation.StatusCode < 200 {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(20 * time.Second)
	for !pending() {
		if time.Now().After(deadline) {
			t.Fatal("start operation was not observed")
		}
		select {
		case err := <-done:
			t.Fatalf("start ended before cancellation: %v", err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancelStart()
	if err := create("pending-b"); err == nil {
		cleanup("pending-b")
		t.Fatal("overlapping start escaped the running budget")
	} else if !strings.Contains(err.Error(), "running memory budget") {
		t.Fatal("expected budget refusal after the retained start completed", err)
	}
	if err := <-done; err == nil {
		t.Fatal("cancelled request unexpectedly succeeded")
	}
	for pending() {
		if time.Now().After(deadline) {
			t.Fatal("daemon start did not settle")
		}
		time.Sleep(100 * time.Millisecond)
	}
	items, err := b.List(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.Name == "pending-a" && item.Status == "Running" {
			found = true
		}
	}
	if !found {
		t.Fatal("daemon did not finish the cancelled client's start")
	}
	if err := b.Stop(ctx, p, "pending-a"); err != nil {
		t.Fatal(err)
	}
	if err := create("pending-b"); err != nil {
		t.Fatal("capacity did not recover after operation settled", err)
	}
	cleanup("pending-b")
}
