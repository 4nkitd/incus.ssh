package setup

import (
	"strings"
	"testing"
)

func TestInstallerModeRejectsPreparationFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--install-incus", "--project", "ifd-test"},
		{"--install-incus", "--config", "/missing"},
		{"--install-incus", "--templates", "/missing"},
		{"--install-incus", "--incus", "/not-an-executable", "--apply"},
	} {
		if err := Run(args); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Errorf("%v: expected flag rejection before any installer action, got %v", args, err)
		}
	}
}
