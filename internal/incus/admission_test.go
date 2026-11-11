package incus

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"incus-for-devs/internal/core"
)

func runtimeFixture(t *testing.T, items []instance, budget string) *fixture {
	t.Helper()
	f := newFixture(t, items, scenario{})
	f.config.Limits.MaxInstances = 0
	f.config.Limits.RunningMemory = budget
	f.config.AdmissionDirectory = filepath.Join(f.dir, "admission")
	var err error
	f.backend, err = New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRunningBudgetIgnoresStoppedInventory(t *testing.T) {
	items := []instance{}
	for _, name := range []string{"one", "two", "three", "four"} {
		item := ownedInstance(name, "alice")
		item.Status = "Stopped"
		items = append(items, item)
	}
	f := runtimeFixture(t, items, "1GiB")
	if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "new", Memory: "512MiB"}); err != nil {
		t.Fatal("stopped inventory consumed capacity", err)
	}
	if err := f.backend.Start(context.Background(), alice, "one"); err == nil || !strings.Contains(err.Error(), "running memory budget") {
		t.Fatal("oversized stopped start bypassed running budget", err)
	}
}

func TestRunningBudgetChargesFrozenAndUnknownStates(t *testing.T) {
	for _, status := range []string{"Running", "Starting", "Stopping", "Frozen", "Unknown", "Error"} {
		t.Run(status, func(t *testing.T) {
			item := ownedInstance("active", "alice")
			item.Status = status
			f := runtimeFixture(t, []instance{item}, "2GiB")
			if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "new", Memory: "256MiB"}); err == nil {
				t.Fatal("active/uncertain state was not charged")
			}
		})
	}
}

func TestRunningBudgetMinimumReservationAndRestart(t *testing.T) {
	item := ownedInstance("small", "alice")
	item.Config["limits.memory"] = "1MiB"
	f := runtimeFixture(t, []instance{item}, "256MiB")
	if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "new", Memory: "1MiB"}); err == nil {
		t.Fatal("tiny requests bypassed minimum reservation")
	}
	if err := f.backend.Restart(context.Background(), alice, "small"); err != nil {
		t.Fatal("active restart was double charged", err)
	}
	item.Status = "Stopped"
	delete(item.Config, "limits.memory")
	if committed, err := f.backend.committedMemory([]instance{item}, ""); err != nil || committed != 0 {
		t.Fatal("stopped malformed allocation should not consume RAM", err)
	}
	item.Status = "Running"
	if _, err := f.backend.committedMemory([]instance{item}, ""); err == nil {
		t.Fatal("active malformed allocation was accepted")
	}
}

func TestConcurrentRunningAdmission(t *testing.T) {
	f := runtimeFixture(t, nil, "1GiB")
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for _, name := range []string{"one", "two", "three", "four"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: name, Memory: "512MiB"})
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 2 {
		t.Fatalf("expected two admitted starts, got %d", success)
	}
}

func TestRunningBudgetConfiguration(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	cfg := f.config
	for _, budget := range []string{"0", "1MiB", "invalid"} {
		cfg.Limits.MaxInstances = 0
		cfg.Limits.RunningMemory = budget
		if _, err := New(cfg); err == nil {
			t.Fatal("invalid budget accepted", budget)
		}
	}
	cfg.Limits.MaxInstances = 1
	cfg.Limits.RunningMemory = "8GiB"
	if _, err := New(cfg); err == nil {
		t.Fatal("ambiguous inventory and runtime limits accepted")
	}
}

func TestPendingDaemonTaskBlocksAdmission(t *testing.T) {
	item := ownedInstance("pending", "alice")
	item.Status = "Stopped"
	f := runtimeFixture(t, []instance{item}, "2GiB")
	writeJSON(t, filepath.Join(f.dir, "operations.json"), []map[string]any{{"class": "task", "status_code": 103}})
	if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "another", Memory: "256MiB"}); err == nil || !strings.Contains(err.Error(), "still pending") {
		t.Fatal("pending daemon start was ignored", err)
	}
	if err := f.backend.Start(context.Background(), alice, "pending"); err == nil || !strings.Contains(err.Error(), "still pending") {
		t.Fatal("start bypassed pending task gate", err)
	}
	if err := f.backend.Stop(context.Background(), alice, "pending"); err != nil {
		t.Fatal("pending gate prevented recovery stop", err)
	}
	writeJSON(t, filepath.Join(f.dir, "operations.json"), []map[string]any{{"class": "websocket", "status_code": 103}, {"class": "task", "status_code": 200}})
	if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "another", Memory: "256MiB"}); err != nil {
		t.Fatal("completed task or shell blocked admission", err)
	}
}
