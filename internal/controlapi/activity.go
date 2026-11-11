package controlapi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"incus-for-devs/internal/command"
	"incus-for-devs/internal/core"
)

type activityEvent struct {
	At      time.Time `json:"at"`
	Action  string    `json:"action"`
	Name    string    `json:"name,omitempty"`
	Success bool      `json:"success"`
}
type activityState struct {
	Principal core.Principal  `json:"principal"`
	Events    []activityEvent `json:"events"`
}
type activityStore struct {
	mu         sync.Mutex
	path       string
	state      activityState
	saveFailed bool
}

func openActivity(directory string, p core.Principal) (*activityStore, error) {
	a := &activityStore{path: filepath.Join(directory, "activity.json"), state: activityState{Principal: p, Events: []activityEvent{}}}
	info, err := os.Lstat(a.path)
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return nil, errors.New("API activity must be a private regular file smaller than 1 MiB")
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(data, &a.state) != nil || a.state.Principal != p || len(a.state.Events) > 200 {
		return nil, errors.New("invalid API activity state or principal changed")
	}
	return a, nil
}

func (a *activityStore) record(action, name string, success bool) {
	if a == nil {
		return
	}
	if command.ValidateName(name) != nil {
		name = ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Events = append([]activityEvent{{At: time.Now().UTC(), Action: action, Name: name, Success: success}}, a.state.Events...)
	if len(a.state.Events) > 200 {
		a.state.Events = a.state.Events[:200]
	}
	data, err := json.Marshal(a.state)
	if err == nil {
		err = writePrivateFile(a.path, data)
	}
	a.saveFailed = err != nil
}

func (a *activityStore) snapshot() any {
	if a == nil {
		return map[string]any{"events": []activityEvent{}, "persistent": false}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{"events": append([]activityEvent{}, a.state.Events...), "persistent": !a.saveFailed}
}

func writePrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".activity-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
