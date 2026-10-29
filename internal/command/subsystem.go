package command

import (
	"context"
	"errors"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/filetransfer"
)

func (h Handler) SupportsSubsystem(name string) bool { return name == "sftp" && h.Files != nil }

func (h Handler) Subsystem(ctx context.Context, p core.Principal, name string, streams core.Streams) int {
	if !h.SupportsSubsystem(name) || streams.PTY {
		return failure(streams, false, errors.New("unsupported subsystem"))
	}
	if err := filetransfer.Serve(ctx, p, h.Files, streams); err != nil {
		return failure(streams, false, errors.New("SFTP transfer ended unexpectedly"))
	}
	return 0
}
