package controlapi

import (
	"context"
	"testing"

	"incus-for-devs/internal/core"
)

type lifecycleSharing struct {
	core.Sharing
	called    bool
	principal core.Principal
	name      string
}

func (s *lifecycleSharing) ChangeMachine(ctx context.Context, p core.Principal, name string, apply func() error) error {
	s.called = true
	s.principal = p
	s.name = name
	return apply()
}

func TestLifecycleRetainsSharingRevocation(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart", "delete"} {
		backend := &fakeBackend{}
		sharing := &lifecycleSharing{}
		s := service{backend: backend, sharing: sharing}
		if _, err := s.lifecycle(context.Background(), testPrincipal, action, "demo"); err != nil {
			t.Fatal(err)
		}
		if !sharing.called || sharing.principal != testPrincipal || sharing.name != "demo" || len(backend.calls) != 1 {
			t.Fatal("lifecycle bypassed sharing boundary")
		}
	}
}
