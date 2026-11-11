package incus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"incus-for-devs/internal/core"
)

func TestUnacknowledgedStartSurvivesBackendRestart(t *testing.T) {
	item := ownedInstance("pending", "alice")
	item.Status = "Stopped"
	f := runtimeFixture(t, []instance{item}, "2GiB")
	writeJSON(t, filepath.Join(f.dir, "scenario.json"), scenario{Block: "query"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := f.backend.Start(ctx, alice, "pending"); err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatal(err)
	}
	pending, err := f.backend.readAdmission(alice.Project)
	if err != nil || pending == nil || pending.Operation != "" {
		t.Fatalf("missing uncertain journal: %+v %v", pending, err)
	}
	writeJSON(t, filepath.Join(f.dir, "scenario.json"), scenario{})
	f.backend, err = New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "another", Memory: "256MiB"}); err == nil || !strings.Contains(err.Error(), "not acknowledged") {
		t.Fatal("restart lost unacknowledged mutation", err)
	}
	if err := f.backend.Stop(context.Background(), alice, "pending"); err != nil {
		t.Fatal("recovery stop blocked", err)
	}
}

func TestClientCancellationBeforeAcknowledgementRetainsOwnership(t *testing.T) {
	item := ownedInstance("pending", "alice")
	item.Status = "Stopped"
	f := runtimeFixture(t, []instance{item}, "2GiB")
	writeJSON(t, filepath.Join(f.dir, "scenario.json"), scenario{WaitForStartRelease: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.backend.Start(ctx, alice, "pending") }()
	waitFile(t, filepath.Join(f.dir, "start.ready"))
	cancel()
	select {
	case err := <-done:
		t.Fatal("released mutation before acknowledgement", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(f.dir, "start.release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if pending, err := f.backend.readAdmission(alice.Project); err != nil || pending != nil {
		t.Fatal("completed journal not cleared", pending, err)
	}
	if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "another", Memory: "256MiB"}); err == nil || !strings.Contains(err.Error(), "running memory budget") {
		t.Fatal("cancelled start lost its reservation", err)
	}
}

func TestAcknowledgedJournalReconcilesOnlyAfterCompletion(t *testing.T) {
	f := runtimeFixture(t, nil, "1GiB")
	id := "11111111-1111-1111-1111-111111111111"
	if err := f.backend.writeAdmission(alice.Project, pendingAdmission{Name: "pending", Action: "start", Operation: id, CreatedAt: time.Now()}, true); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(f.dir, "operations.json"), []map[string]any{{"id": id, "class": "task", "status_code": 103}})
	if err := f.backend.checkPendingTasks(context.Background(), alice); err == nil {
		t.Fatal("pending acknowledged operation cleared")
	}
	writeJSON(t, filepath.Join(f.dir, "operations.json"), []map[string]any{{"id": id, "class": "task", "status_code": 400}})
	if err := f.backend.checkPendingTasks(context.Background(), alice); err != nil {
		t.Fatal(err)
	}
	if pending, err := f.backend.readAdmission(alice.Project); err != nil || pending != nil {
		t.Fatal(pending, err)
	}
}

func TestInvalidAdmissionJournalFailsClosed(t *testing.T) {
	f := runtimeFixture(t, nil, "1GiB")
	path := f.backend.admissionPath(alice.Project)
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.backend.checkPendingTasks(context.Background(), alice); err == nil {
		t.Fatal("corrupt journal ignored")
	}
}
