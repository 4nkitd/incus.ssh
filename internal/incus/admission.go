package incus

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"incus-for-devs/internal/core"
)

const minimumRunningReservation = 256 << 20

func (b *Backend) admissionInstances(ctx context.Context, p core.Principal) ([]instance, error) {
	if err := b.checkPendingTasks(ctx, p); err != nil {
		return nil, err
	}
	items, err := b.list(ctx, p)
	if err != nil {
		return nil, err
	}
	if err := b.checkPendingTasks(ctx, p); err != nil {
		return nil, err
	}
	return items, nil
}

func (b *Backend) checkPendingTasks(ctx context.Context, p core.Principal) error {
	if b.runningMemory == 0 {
		return nil
	}
	data, err := b.run(ctx, p, "operation", "list", "--format=json")
	if err != nil {
		return err
	}
	var operations []struct {
		ID         string `json:"id"`
		Class      string `json:"class"`
		StatusCode int    `json:"status_code"`
	}
	if json.Unmarshal(data, &operations) != nil {
		return errors.New("cannot verify pending Incus operations")
	}
	pending, err := b.readAdmission(p.Project)
	if err != nil {
		return err
	}
	if pending != nil {
		if pending.Operation == "" {
			return core.UserError{Message: "A previous start was not acknowledged. New starts are blocked until the operator reconciles the admission journal."}
		}
		for _, operation := range operations {
			if operation.ID == pending.Operation && operation.StatusCode < 200 {
				return core.UserError{Message: "A previous Incus start is still pending. Wait for it to finish before starting another container."}
			}
		}
		if err := b.clearAdmission(p.Project); err != nil {
			return err
		}
	}
	for _, operation := range operations {
		if operation.StatusCode >= 200 {
			continue
		}
		if operation.Class == "websocket" || operation.Class == "token" {
			continue
		}
		// An in-flight start can still report Stopped. Do not release its capacity on that observation.
		return core.UserError{Message: "An Incus task is still pending in this project. Wait for it to finish before creating or starting another container."}
	}
	return nil
}

func (b *Backend) reservation(memory uint64) uint64 {
	if b.runningMemory != 0 && memory < minimumRunningReservation {
		return minimumRunningReservation
	}
	return memory
}

func (b *Backend) committedMemory(items []instance, exclude string) (uint64, error) {
	if b.runningMemory == 0 {
		return projectMemory(items, exclude)
	}
	var committed uint64
	for _, item := range items {
		// Only a confirmed stopped state releases RAM. Frozen and uncertain states remain charged.
		if item.Name == exclude || item.Status == "Stopped" {
			continue
		}
		memory, err := parseSize(instanceConfig(item)["limits.memory"])
		if err != nil {
			return 0, errors.New("cannot verify running memory: an active instance has no valid enforceable memory limit")
		}
		memory = b.reservation(memory)
		if committed > math.MaxUint64-memory {
			return 0, errors.New("running memory allocation overflows")
		}
		committed += memory
	}
	return committed, nil
}
