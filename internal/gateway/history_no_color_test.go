package gateway

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
)

func TestNoColorDoesNotDisableControlLClear(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var output bytes.Buffer
	var commands []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := newSessionInput(ctx, strings.NewReader("whoam\x0ci\rexit\r"))
	status := repl(ctx, core.Principal{Name: "alice", Project: "ifd-dev"}, handlerFunc(func(_ context.Context, _ core.Principal, command string, _ core.Streams) int {
		commands = append(commands, command)
		return 0
	}), core.Streams{In: input, Out: &output, Err: &output, PTY: true, Term: "xterm"})
	if status != 0 || !reflect.DeepEqual(commands, []string{"whoami"}) {
		t.Fatalf("status=%d commands=%v", status, commands)
	}
	if !strings.Contains(output.String(), "\x1b[H\x1b[2Jincus> whoam") {
		t.Fatalf("Ctrl-L did not redraw the uncolored draft: %q", output.String())
	}
	if strings.Contains(output.String(), "\x1b[36m") || strings.Contains(output.String(), "\x1b[1m") {
		t.Fatalf("NO_COLOR emitted a color style: %q", output.String())
	}
}
