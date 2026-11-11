package command

import (
	"context"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
)

type fakeAPI struct {
	action    string
	principal core.Principal
}

func (f *fakeAPI) Manage(p core.Principal, action string) (core.APIStatus, error) {
	f.action = action
	f.principal = p
	result := core.APIStatus{Enabled: action != "disable", REST: "https://api.example.com/api/v1", MCP: "https://api.example.com/mcp"}
	if action == "enable" || action == "rotate" {
		result.Token = "test-token"
	}
	return result, nil
}

func TestAPIManagementCommands(t *testing.T) {
	f := &fakeAPI{}
	for _, action := range []string{"enable", "rotate", "status", "disable"} {
		var out, stderr strings.Builder
		code := (Handler{API: f}).Handle(context.Background(), principal, "api "+action+" --json", core.Streams{Out: &out, Err: &stderr})
		if code != 0 || f.action != action || f.principal != principal {
			t.Fatalf("%s failed: %s", action, stderr.String())
		}
		if (action == "status" || action == "disable") && strings.Contains(out.String(), "test-token") {
			t.Fatal("token disclosed")
		}
	}
	for _, line := range []string{"api", "api enable extra", "api delete", "api enable --token=chosen"} {
		f.action = ""
		if code := (Handler{API: f}).Handle(context.Background(), principal, line, core.Streams{}); code == 0 || f.action != "" {
			t.Fatalf("invalid command accepted: %s", line)
		}
	}
	if code := (Handler{}).Handle(context.Background(), principal, "api enable", core.Streams{}); code == 0 {
		t.Fatal("unconfigured API succeeded")
	}
}
