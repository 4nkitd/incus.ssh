package command

import (
	"context"
	"testing"

	"incus-for-devs/internal/core"
)

type fakeSharing struct {
	action string
	name   string
	port   int
	user   core.Principal
	revoke []string
}

func (f *fakeSharing) Enable(_ context.Context, p core.Principal, name string, port int) (core.Share, error) {
	f.action, f.name, f.port, f.user = "enable", name, port, p
	return core.Share{Name: name, Port: port, URL: "https://route.test", Status: "connected"}, nil
}
func (f *fakeSharing) Disable(_ context.Context, p core.Principal, name string, port int) error {
	f.action, f.name, f.port, f.user = "disable", name, port, p
	return nil
}
func (f *fakeSharing) List(_ context.Context, p core.Principal, name string) ([]core.Share, error) {
	f.action, f.name, f.user = "list", name, p
	return nil, nil
}
func (f *fakeSharing) ChangeMachine(_ context.Context, _ core.Principal, name string, apply func() error) error {
	f.revoke = append(f.revoke, name)
	return apply()
}

func TestShareRequiresOneExplicitMachineAndPort(t *testing.T) {
	for _, line := range []string{"share", "share enable", "share enable demo", "share enable demo 0", "share enable demo 65536", "share enable '*' 80", "share enable demo '*'", "share enable demo 80,443", "share enable demo 80-90", "share enable demo 80 extra", "share enable demo 80 --target=127.0.0.1", "share list '*'", "share enable demo 80 --all"} {
		f := &fakeSharing{}
		status, _, _ := run(Handler{Sharing: f}, line)
		if status == 0 || f.action != "" {
			t.Fatalf("invalid publishing command reached backend: %s", line)
		}
	}
	for _, line := range []string{"share enable demo 8080 --json", "share --json enable demo 8080"} {
		f := &fakeSharing{}
		status, out, stderr := run(Handler{Sharing: f}, line)
		if status != 0 || stderr != "" || f.user != principal || f.name != "demo" || f.port != 8080 || f.action != "enable" {
			t.Fatalf("status=%d out=%s err=%s fake=%+v", status, out, stderr, f)
		}
		decodeObject(t, out)
	}
	f := &fakeSharing{}
	if status, out, _ := run(Handler{Sharing: f}, "share list --json"); status != 0 || out != "{\"shares\":[]}\n" {
		t.Fatal("empty list did not preserve privacy", status, out)
	}
	if status, _, _ := run(Handler{Sharing: f}, "share disable demo 8080"); status != 0 || f.action != "disable" || f.port != 8080 {
		t.Fatal("disable did not address the individual port")
	}
	if status, _, _ := run(Handler{}, "share enable demo 8080 --json"); status == 0 {
		t.Fatal("unconfigured gateway published a port")
	}
}

func TestLifecycleRevokesMachineSharing(t *testing.T) {
	for _, line := range []string{"stop demo", "restart demo", "start demo", "resume demo", "rm demo"} {
		f := &fakeSharing{}
		status, _, _ := run(Handler{Backend: &fakeBackend{}, Sharing: f}, line)
		if status != 0 || len(f.revoke) != 1 || f.revoke[0] != "demo" {
			t.Fatalf("%s did not revoke sharing", line)
		}
	}
}
