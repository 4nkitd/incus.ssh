package controlapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestActivityPersistenceAndBound(t *testing.T) {
	dir := t.TempDir()
	store, err := openActivity(dir, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		store.record("run_command", "demo", true)
	}
	data, err := os.ReadFile(filepath.Join(dir, "activity.json"))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, "activity.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("activity file is not private")
	}
	var state activityState
	if json.Unmarshal(data, &state) != nil || len(state.Events) != 200 {
		t.Fatal("activity is not bounded")
	}
	reopened, err := openActivity(dir, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.state.Events) != 200 {
		t.Fatal("activity did not survive restart")
	}
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		t.Fatal("invalid activity data")
	}
	for _, item := range raw["events"].([]any) {
		for key := range item.(map[string]any) {
			if key != "at" && key != "action" && key != "name" && key != "success" {
				t.Fatal("unexpected activity field", key)
			}
		}
	}
}

func TestActivityPersistenceFailureIsVisible(t *testing.T) {
	store, err := openActivity(t.TempDir(), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(t.TempDir(), "missing", "activity.json")
	store.record("deploy_app", "demo", false)
	if !store.saveFailed {
		t.Fatal("save failure hidden")
	}
	if store.snapshot().(map[string]any)["persistent"] != false {
		t.Fatal("unpersisted history claimed durable")
	}
}
