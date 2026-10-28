package gateway

import (
	"context"
	"testing"
	"time"

	"incus-for-devs/internal/core"
)

func TestPTYUnspecifiedTermAndSize(t *testing.T) {
	seen := make(chan core.Streams, 1)
	cfg, host, key := testConfig(t, handlerFunc(func(_ context.Context, _ core.Principal, _ string, s core.Streams) int {
		seen <- s
		return 0
	}))
	addr, _, _ := startServer(t, cfg)
	session := newSession(t, dial(t, addr, host, key))
	if err := session.RequestPty("", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Run("ssh demo"); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-seen:
		if !s.PTY || s.Term != "dumb" || s.Window != (core.Window{Width: 80, Height: 24}) {
			t.Fatalf("wrong PTY defaults: %+v", s)
		}
	case <-time.After(time.Second):
		t.Fatal("missing handler invocation")
	}
}

func TestUnspecifiedResizeKeepsKnownDimensions(t *testing.T) {
	h := &sessionHandler{}
	h.resize(core.Window{Width: 120, Height: 40})
	h.resize(core.Window{Height: 50})
	s, finish := h.begin(core.Streams{})
	defer finish()
	if s.Window != (core.Window{Width: 120, Height: 50}) {
		t.Fatalf("lost known dimensions: %+v", s.Window)
	}
}
