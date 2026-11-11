package controlapi

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"incus-for-devs/internal/core"
)

var testPrincipal = core.Principal{Name: "dev", Project: "ifd-dev"}

const testOrigin = "https://api.example.com"

func TestTokenLifecycleAndPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "api")
	tokens, err := OpenTokens(dir, testOrigin, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	defer tokens.Close()
	status, err := tokens.Manage(testPrincipal, "status")
	if err != nil || status.Enabled || status.Token != "" {
		t.Fatal("must start disabled")
	}
	if _, err := tokens.Manage(core.Principal{Name: "other", Project: "other"}, "enable"); err == nil {
		t.Fatal("accepted wrong principal")
	}
	status, err = tokens.Manage(testPrincipal, "enable")
	if err != nil || !status.Enabled || len(status.Token) != 47 {
		t.Fatal("enable failed", err)
	}
	first := status.Token
	_, epoch, ok := tokens.authenticate(first)
	if !ok {
		t.Fatal("new token rejected")
	}
	if _, err := tokens.Manage(testPrincipal, "enable"); err == nil {
		t.Fatal("enable silently rotated token")
	}
	status, _ = tokens.Manage(testPrincipal, "status")
	if status.Token != "" {
		t.Fatal("status disclosed token")
	}
	data, err := os.ReadFile(filepath.Join(dir, "token.json"))
	if err != nil || bytes.Contains(data, []byte(first)) {
		t.Fatal("plaintext token persisted", err)
	}
	info, _ := os.Stat(filepath.Join(dir, "token.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("wrong state permissions")
	}
	reopened, err := OpenTokens(dir, testOrigin, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reopened.authenticate(first); !ok {
		t.Fatal("token did not survive restart")
	}
	reopened.Close()
	status, err = tokens.Manage(testPrincipal, "rotate")
	if err != nil || status.Token == first {
		t.Fatal("rotation failed", err)
	}
	if epoch.Err() == nil {
		t.Fatal("old requests not canceled")
	}
	if _, _, ok := tokens.authenticate(first); ok {
		t.Fatal("old token still accepted")
	}
	_, epoch, ok = tokens.authenticate(status.Token)
	if !ok {
		t.Fatal("rotated token rejected")
	}
	if _, err := tokens.Manage(testPrincipal, "disable"); err != nil {
		t.Fatal(err)
	}
	if epoch.Err() == nil {
		t.Fatal("disable did not cancel requests")
	}
	if _, _, ok := tokens.authenticate(status.Token); ok {
		t.Fatal("disabled token accepted")
	}
	reopened, err = OpenTokens(dir, testOrigin, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status, _ = reopened.Manage(testPrincipal, "status")
	if status.Enabled {
		t.Fatal("disable did not survive restart")
	}
}

func TestTokenStateFailsClosed(t *testing.T) {
	for _, kind := range []string{"directory-permissions", "file-permissions", "corrupt", "principal", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "api")
			tokens, err := OpenTokens(dir, testOrigin, testPrincipal)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tokens.Manage(testPrincipal, "enable"); err != nil {
				t.Fatal(err)
			}
			tokens.Close()
			p := testPrincipal
			path := filepath.Join(dir, "token.json")
			switch kind {
			case "directory-permissions":
				err = os.Chmod(dir, 0755)
			case "file-permissions":
				err = os.Chmod(path, 0644)
			case "corrupt":
				err = os.WriteFile(path, []byte("bad json"), 0600)
			case "principal":
				p.Project = "other"
			case "symlink":
				err = os.Rename(path, path+".real")
				if err == nil {
					err = os.Symlink(path+".real", path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenTokens(dir, testOrigin, p); err == nil {
				t.Fatal("unsafe state accepted")
			}
		})
	}
}
