package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseRejectsUnpatchedToolchainBeforeBuilding(t *testing.T) {
	dir := t.TempDir()
	goScript := "#!/bin/sh\ncase \"$1 $2\" in\n'env GOHOSTOS') echo linux;;\n'env GOHOSTARCH') echo amd64;;\n'env GOVERSION') echo go1.27.3;;\n*) exit 99;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(goScript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sha256sum"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join("..", "..", "scripts", "release.sh"), "v0.5.0")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Go 1.27.4 or newer") {
		t.Fatalf("unpatched release accepted: %v %s", err, output)
	}
}
