package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

func validConfig() Config {
	return Config{HostKey: "/etc/ifd/host.key", IncusBinary: "/usr/bin/incus", Image: "images:ubuntu/24.04", Profile: "ifd", Limits: core.Limits{MaxInstances: 1, CPU: 1, Memory: "2GiB"}, Users: []User{{Name: "alice", Project: "ifd-dev", AuthorizedKeys: []string{"key validated by gateway"}}}}
}

func TestPrivateListeners(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:2222", "[::1]:2222", "100.64.0.10:2222", "[fd7a:115c:a1e0::1]:2222"} {
		if err := validateListen(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:2222", "[::]:2222", "192.168.1.2:2222", "8.8.8.8:2222", "100.64.0.10:22", "127.0.0.1:65536", ":2222", "example.com:2222"} {
		if err := validateListen(addr); err == nil {
			t.Errorf("accepted unsafe listener %s", addr)
		}
	}
}

func TestConfigDefaultsAndSingleUser(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:2222" || cfg.MaxSessions != 8 || cfg.OperationTimeoutSeconds != 120 || cfg.Limits.Processes != 256 {
		t.Fatal("unexpected defaults")
	}
	configured := validConfig()
	configured.Limits.Processes = 512
	if err := configured.Validate(); err != nil || configured.Limits.Processes != 512 {
		t.Fatalf("configured process maximum = %d, %v", configured.Limits.Processes, err)
	}
	cfg.Users = append(cfg.Users, User{Name: "other", Project: "ifd-other", AuthorizedKeys: []string{"key"}})
	if cfg.Validate() == nil {
		t.Fatal("multiple users accepted without global admission limits")
	}
}

func TestConfigRejectsUnsafeValues(t *testing.T) {
	tests := []func(*Config){
		func(c *Config) { c.HostKey = "relative" },
		func(c *Config) { c.IncusBinary = "incus" },
		func(c *Config) { c.Users[0].Project = "default" },
		func(c *Config) { c.Users[0].Name = "--project" },
		func(c *Config) { c.Users[0].Name = "alice-" },
		func(c *Config) { c.Users[0].Project = "ifd-dev-" },
		func(c *Config) { c.Profile = "ifd-" },
		func(c *Config) { c.Users[0].AuthorizedKeys = nil },
		func(c *Config) { c.MaxSessions = 999 },
		func(c *Config) { c.OperationTimeoutSeconds = -1 },
		func(c *Config) { c.Limits.MaxInstances = 0 },
		func(c *Config) { c.Limits.Processes = -1 },
	}
	for i, mutate := range tests {
		cfg := validConfig()
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestLoadConfigStrictJSON(t *testing.T) {
	data, _ := json.Marshal(validConfig())
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{" {}", " null", " garbage"} {
		if err := os.WriteFile(path, append(append([]byte(nil), data...), suffix...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Error("accepted trailing JSON")
		}
	}
	unknown := strings.Replace(string(data), `"listen":`, `"unexpected":true,"listen":`, 1)
	os.WriteFile(path, []byte(unknown), 0600)
	if _, err := LoadConfig(path); err == nil {
		t.Error("accepted unknown fields")
	}
	os.WriteFile(path, data, 0600)
	os.Chmod(path, 0666)
	if _, err := LoadConfig(path); err == nil {
		t.Error("accepted writable configuration")
	}
}

func TestHostKeyNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.key")
	fingerprint, err := generateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatal("fingerprint mismatch")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe key mode")
	}
	if _, err := generateHostKey(path); err == nil {
		t.Fatal("overwrote key")
	}
	after, _ := os.ReadFile(path)
	if string(data) != string(after) {
		t.Fatal("key changed")
	}
}
