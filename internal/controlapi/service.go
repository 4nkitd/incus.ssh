package controlapi

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"incus-for-devs/internal/command"
	"incus-for-devs/internal/core"
)

type CreateInput struct {
	Name      string `json:"name,omitempty"`
	Image     string `json:"image,omitempty"`
	CPU       int    `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Processes int    `json:"processes,omitempty"`
	Disk      string `json:"disk,omitempty"`
}

type NameInput struct {
	Name string `json:"name"`
}

type ExecInput struct {
	Name    string `json:"name"`
	Command string `json:"command" jsonschema:"Noninteractive shell command to run inside the container, never on the host"`
	Stdin   string `json:"stdin,omitempty"`
}

type service struct {
	backend            core.Backend
	sharing            core.Sharing
	applicationsDomain string
	version            string
	activity           *activityStore
}

type inputError struct{ error }

func badInput(message string) error { return inputError{errors.New(message)} }

func operationError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		var userError core.UserError
		if errors.As(err, &userError) {
			return userError
		}
		return errors.New("operation failed; check machine ownership, state, resources and operator configuration")
	}
	return nil
}

func (s service) list(ctx context.Context, p core.Principal) (any, error) {
	items, err := s.backend.List(ctx, p)
	if items == nil {
		items = []core.Instance{}
	}
	return map[string]any{"vms": items}, operationError(ctx, err)
}

func (s service) images(ctx context.Context, p core.Principal) (any, error) {
	items, err := s.backend.Images(ctx, p)
	if items == nil {
		items = []core.ImageInfo{}
	}
	return map[string]any{"images": items}, operationError(ctx, err)
}

func (s service) create(ctx context.Context, p core.Principal, in CreateInput) (any, error) {
	opts := core.CreateOptions{Name: in.Name, Image: in.Image, CPU: in.CPU, Memory: in.Memory, Processes: in.Processes, Disk: in.Disk}
	if err := command.ValidateCreate(opts); err != nil {
		return nil, inputError{err}
	}
	vm, err := s.backend.Create(ctx, p, opts)
	s.activity.record("create_container", vm.Name, err == nil)
	return map[string]any{"vm": vm}, operationError(ctx, err)
}

func (s service) lifecycle(ctx context.Context, p core.Principal, action, name string) (any, error) {
	if err := command.ValidateName(name); err != nil {
		return nil, inputError{err}
	}
	var apply func(context.Context, core.Principal, string) error
	switch action {
	case "start":
		apply = s.backend.Start
	case "stop":
		apply = s.backend.Stop
	case "restart":
		apply = s.backend.Restart
	case "delete":
		apply = s.backend.Remove
	default:
		return nil, badInput("unknown lifecycle action")
	}
	change := func() error { return apply(ctx, p, name) }
	var err error
	if s.sharing != nil {
		err = s.sharing.ChangeMachine(ctx, p, name, change)
	} else {
		err = change()
	}
	s.activity.record(action, name, err == nil)
	return map[string]any{"name": name, "action": action}, operationError(ctx, err)
}

func (s service) exec(ctx context.Context, p core.Principal, in ExecInput) (any, error) {
	if err := command.ValidateName(in.Name); err != nil {
		return nil, inputError{err}
	}
	if strings.TrimSpace(in.Command) == "" || len(in.Command) > 16<<10 || strings.ContainsRune(in.Command, 0) || !utf8.ValidString(in.Command) || len(in.Stdin) > 32<<10 {
		return nil, badInput("command must be nonempty UTF-8 without NUL, at most 16 KiB; stdin is limited to 32 KiB")
	}
	stdout, stderr := &limitedOutput{}, &limitedOutput{}
	code, err := s.backend.Exec(ctx, p, in.Name, in.Command, core.Streams{In: strings.NewReader(in.Stdin), Out: stdout, Err: stderr})
	s.activity.record("run_command", in.Name, err == nil && code == 0)
	return map[string]any{"exit_code": code, "stdout": stdout.String(), "stderr": stderr.String(), "truncated": stdout.truncated || stderr.truncated}, operationError(ctx, err)
}

type limitedOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := (1 << 20) - b.buffer.Len()
	if n > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func (b *limitedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
