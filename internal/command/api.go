package command

import (
	"errors"

	"incus-for-devs/internal/core"
)

func (h Handler) api(p core.Principal, r request, s core.Streams) int {
	if h.API == nil {
		return failure(s, r.json, errors.New("API is not configured; ask the operator to run incus-for-devs setup-api"))
	}
	status, err := h.API.Manage(p, r.args[0])
	if err != nil {
		return failure(s, r.json, err)
	}
	if r.json {
		return writeJSON(s, status)
	}
	if status.Token != "" {
		return writeText(s, "API enabled. Save this token now; it will not be shown again.\nREST: %s\nMCP: %s\nToken: %s\n", status.REST, status.MCP, status.Token)
	}
	return writeText(s, "API enabled: %t\nREST: %s\nMCP: %s\n", status.Enabled, status.REST, status.MCP)
}

func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("invalid instance name")
	}
	return nil
}

func ValidateCreate(opts core.CreateOptions) error {
	if opts.Name != "" {
		if err := ValidateName(opts.Name); err != nil {
			return err
		}
	}
	if opts.CPU < 0 || opts.Processes < 0 || opts.CPU > 1<<31-1 || opts.Processes > 1<<31-1 {
		return errors.New("CPU and processes must be nonnegative 31-bit integers; zero selects defaults")
	}
	for _, size := range []string{opts.Memory, opts.Disk} {
		if size != "" {
			if err := validSize(size); err != nil {
				return err
			}
		}
	}
	if opts.Image != "" {
		return validImageRef(opts.Image)
	}
	return nil
}
