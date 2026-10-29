package command

import (
	"context"
	"errors"
	"strconv"

	"incus-for-devs/internal/core"
)

const shareUsage = "usage: share enable NAME PORT | share disable NAME PORT | share list [NAME] [--json]"

func validateShareArgs(args []string) error {
	if len(args) == 0 {
		return errors.New(shareUsage)
	}
	switch args[0] {
	case "enable", "disable":
		if len(args) != 3 || !namePattern.MatchString(args[1]) {
			return errors.New(shareUsage)
		}
		port, err := positiveInteger(args[2])
		if err != nil || port > 65535 {
			return errors.New("share requires exactly one numeric port from 1 to 65535")
		}
	case "list":
		if len(args) > 2 || (len(args) == 2 && !namePattern.MatchString(args[1])) {
			return errors.New(shareUsage)
		}
	default:
		return errors.New(shareUsage)
	}
	return nil
}

func (h Handler) share(ctx context.Context, p core.Principal, r request, s core.Streams) int {
	if h.Sharing == nil {
		return failure(s, r.json, errors.New("HTTPS sharing is not configured by the operator; all machine ports remain private"))
	}
	args := r.args
	if args[0] == "list" {
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		items, err := h.Sharing.List(ctx, p, name)
		if err != nil {
			return failure(s, r.json, err)
		}
		if items == nil {
			items = []core.Share{}
		}
		if r.json {
			return writeJSON(s, struct {
				Shares []core.Share `json:"shares"`
			}{items})
		}
		for _, item := range items {
			if status := writeText(s, "%s\t%d\t%s\t%s\n", item.Name, item.Port, item.Status, item.URL); status != 0 {
				return status
			}
		}
		return 0
	}
	port, _ := strconv.Atoi(args[2])
	if args[0] == "disable" {
		if err := h.Sharing.Disable(ctx, p, args[1], port); err != nil {
			return failure(s, r.json, err)
		}
		if r.json {
			return writeJSON(s, struct {
				Name string `json:"vm_name"`
				Port int    `json:"port"`
			}{args[1], port})
		}
		return writeText(s, "private %s:%d\n", args[1], port)
	}
	item, err := h.Sharing.Enable(ctx, p, args[1], port)
	if err != nil {
		return failure(s, r.json, err)
	}
	if r.json {
		return writeJSON(s, item)
	}
	return writeText(s, "%s\nPUBLIC: anyone with this URL can reach %s:%d. Require application authentication.\n", item.URL, item.Name, item.Port)
}
