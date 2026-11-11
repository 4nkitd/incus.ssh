package incus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"incus-for-devs/internal/core"
)

type pendingAdmission struct {
	Name      string    `json:"name"`
	Action    string    `json:"action"`
	Operation string    `json:"operation,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func prepareAdmissionDirectory(directory string) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" {
		return errors.New("running-only admission requires a private absolute admission directory")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("admission directory must be private and not a symlink")
	}
	return nil
}

func (b *Backend) admissionPath(project string) string {
	return filepath.Join(b.config.AdmissionDirectory, project+".json")
}

func (b *Backend) readAdmission(project string) (*pendingAdmission, error) {
	path := b.admissionPath(project)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return nil, errors.New("invalid private admission journal")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pending pendingAdmission
	if json.Unmarshal(data, &pending) != nil || !validName(pending.Name) || (pending.Action != "start" && pending.Action != "restart" && pending.Action != "unfreeze") || pending.CreatedAt.IsZero() || (pending.Operation != "" && !uuidPattern.MatchString(pending.Operation)) {
		return nil, errors.New("admission journal is corrupt; operator reconciliation is required")
	}
	return &pending, nil
}

func (b *Backend) syncAdmissionDirectory() error {
	f, err := os.Open(b.config.AdmissionDirectory)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (b *Backend) writeAdmission(project string, pending pendingAdmission, initial bool) error {
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	path := b.admissionPath(project)
	var f *os.File
	if initial {
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	} else {
		f, err = os.CreateTemp(b.config.AdmissionDirectory, ".admission-*")
	}
	if err != nil {
		return err
	}
	if !initial {
		defer os.Remove(f.Name())
	}
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
	if !initial {
		if err := os.Rename(f.Name(), path); err != nil {
			return err
		}
	}
	return b.syncAdmissionDirectory()
}

func (b *Backend) clearAdmission(project string) error {
	if err := os.Remove(b.admissionPath(project)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return b.syncAdmissionDirectory()
}

func (b *Backend) startCreated(ctx context.Context, p core.Principal, name string) error {
	items, err := b.admissionInstances(ctx, p)
	if err != nil {
		return err
	}
	item, err := findOwned(items, p, name)
	if err != nil {
		return err
	}
	if item.Status != "Stopped" {
		return errors.New("new container did not settle in the stopped state")
	}
	if err := b.admitStart(item, items); err != nil {
		return err
	}
	return b.trackedStart(ctx, p, name, "start")
}

func (b *Backend) trackedStart(ctx context.Context, p core.Principal, name, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pending := pendingAdmission{Name: name, Action: action, CreatedAt: time.Now().UTC()}
	if err := b.writeAdmission(p.Project, pending, true); err != nil {
		return err
	}
	// Preserve mutation ownership after a client disconnect. The durable journal covers timeouts or crashes before acknowledgement.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(b.config.OperationTimeout)
	}
	work, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"action": action, "timeout": max(1, int(b.config.OperationTimeout/time.Second)), "force": false})
	data, err := b.run(work, p, "query", "/1.0/instances/"+name+"/state?project="+p.Project, "--request", "PUT", "--data", string(body), "--raw")
	if err != nil {
		return core.UserError{Message: "Incus did not acknowledge the start. Capacity is held in the admission journal; ask the operator to reconcile it before retrying."}
	}
	var response struct {
		Operation string `json:"operation"`
	}
	if json.Unmarshal(data, &response) != nil || !strings.HasPrefix(response.Operation, "/1.0/operations/") {
		return errors.New("invalid start acknowledgement; admission remains blocked")
	}
	id := strings.TrimPrefix(response.Operation, "/1.0/operations/")
	if !uuidPattern.MatchString(id) {
		return errors.New("invalid start operation identity; admission remains blocked")
	}
	pending.Operation = id
	if err := b.writeAdmission(p.Project, pending, false); err != nil {
		return err
	}
	data, err = b.run(work, p, "query", "/1.0/operations/"+id+"/wait?project="+p.Project+"&timeout=-1")
	if err != nil {
		return err
	}
	var completed struct {
		StatusCode int `json:"status_code"`
	}
	if json.Unmarshal(data, &completed) != nil || completed.StatusCode < 200 {
		return errors.New("cannot confirm start completion; admission remains blocked")
	}
	if err := b.clearAdmission(p.Project); err != nil {
		return err
	}
	if completed.StatusCode != 200 {
		return core.UserError{Message: "Incus could not start the container. Inspect its logs before retrying."}
	}
	return ctx.Err()
}
