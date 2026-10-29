package install

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapScriptSyntaxAndHelp(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	path := filepath.Join("..", "..", "install.sh")
	if output, err := exec.Command(bash, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bootstrap shell syntax: %v\n%s", err, output)
	}
	output, err := exec.Command(bash, path, "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "--ssh-key-file") || !strings.Contains(string(output), "fresh Ubuntu 24.04") {
		t.Fatalf("bootstrap help: %v\n%s", err, output)
	}
}

func TestBootstrapRejectsUnknownOrIncompleteOptionsBeforeHostWork(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	for _, args := range [][]string{{"--unknown"}, {"--ssh-key-file"}, {"--listen"}} {
		output, err := exec.Command(bash, append([]string{filepath.Join("..", "..", "install.sh")}, args...)...).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "incus.ssh:") {
			t.Fatalf("unsafe option acceptance %v: %v\n%s", args, err, output)
		}
	}
}
