package controlapi

import (
	"context"
	"incus-for-devs/internal/command"
	"incus-for-devs/internal/core"
)

func (s service) inspectApp(ctx context.Context, p core.Principal, name string) (any, error) {
	if err := command.ValidateName(name); err != nil {
		return nil, inputError{err}
	}
	backend, ok := s.backend.(core.AppInspector)
	if !ok {
		return nil, badInput("Runtime inspection is not available on this backend.")
	}
	result, err := backend.InspectApp(ctx, p, name)
	return result, operationError(ctx, err)
}

func (s service) updateEnvironment(ctx context.Context, p core.Principal, name string, in core.EnvironmentUpdate) (any, error) {
	if err := command.ValidateName(name); err != nil {
		return nil, inputError{err}
	}
	backend, ok := s.backend.(core.AppInspector)
	if !ok {
		return nil, badInput("Environment editing is not available on this backend.")
	}
	err := backend.UpdateEnvironment(ctx, p, name, in)
	s.activity.record("update_environment", name, err == nil)
	return map[string]any{"name": name, "restart_required": true}, operationError(ctx, err)
}
