package controlapi

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestSubprocessOutputCannotBypassLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stdout, stderr := &limitedOutput{}, &limitedOutput{}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "/usr/bin/head -c 2097152 /dev/zero; /usr/bin/head -c 2097152 /dev/zero >&2")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []*limitedOutput{stdout, stderr} {
		if len(stream.String()) != 1<<20 || !stream.truncated {
			t.Fatal("subprocess pipe bypassed the output limit")
		}
	}
}
