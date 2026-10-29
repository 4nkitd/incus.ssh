package operator

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseRequiresSeparatorForFlagLikeNativeArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--project", "other"},
		{"--debug", "list"},
	} {
		if _, err := parse(args); err == nil || !strings.Contains(err.Error(), "-- separator") {
			t.Fatalf("%v: expected separator error, got %v", args, err)
		}
	}
}

func TestParseAcceptsConfigFormsAndNativeArgs(t *testing.T) {
	opts, err := parse([]string{"--config=/tmp/a.json", "--", "--config", "native"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.explicit || opts.config != "/tmp/a.json" {
		t.Fatalf("unexpected options %+v", opts)
	}
	if want := []string{"--config", "native"}; !reflect.DeepEqual(opts.native, want) {
		t.Fatalf("native = %#v, want %#v", opts.native, want)
	}
}

func TestParseRejectsInvalidConfigOptions(t *testing.T) {
	tests := [][]string{
		{"--config"},
		{"--config="},
		{"--config", "relative.json"},
		{"--config", "/a.json", "--config", "/b.json"},
		{"--config=/a.json", "--config=/b.json"},
	}
	for _, args := range tests {
		if _, err := parse(args); err == nil {
			t.Fatalf("%v: expected parse error", args)
		}
	}
}

func TestParseNoArgsDefaultsToHelpPath(t *testing.T) {
	if opts, err := parse(nil); err != nil || opts.explicit {
		t.Fatalf("unexpected options %+v, err %v", opts, err)
	}
}

func TestLoadConfigOptionalDefaultMissing(t *testing.T) {
	cfg, err := loadOperatorConfigWith(testConfigPath, false, absentConfigFS(testConfigPath), func(string) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Binary != defaultIncusBinary || cfg.Project != "" {
		t.Fatalf("unexpected fallback config %+v", cfg)
	}
}

func TestLoadConfigExplicitMissingFails(t *testing.T) {
	if _, err := loadOperatorConfigWith(testConfigPath, true, absentConfigFS(testConfigPath), func(string) error {
		return nil
	}); err == nil {
		t.Fatal("expected explicit missing config error")
	}
}

func TestLoadConfigReadsOnlyBinaryAndFirstProject(t *testing.T) {
	files := existingConfigFS(testConfigPath, rootConfigInfo(validConfig), rootConfigInfo(validConfig), validConfig)
	cfg, err := loadOperatorConfigWith(testConfigPath, true, files, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Binary != "/usr/bin/incus" || cfg.Project != "ifd-dev" {
		t.Fatalf("unexpected config %+v", cfg)
	}
}

func TestConfigPathIsCleanedBeforeLookup(t *testing.T) {
	files := existingConfigFS(testConfigPath, rootConfigInfo(validConfig), rootConfigInfo(validConfig), validConfig)
	cfg, err := loadOperatorConfigWith("/etc/incus-for-devs/unused/../config.json", true, files, func(path string) error {
		if path != "/etc/incus-for-devs" {
			t.Fatalf("unclean parent path: %q", path)
		}
		return nil
	})
	if err != nil || cfg.Project != "ifd-dev" {
		t.Fatalf("cleaned configuration not loaded: %+v, %v", cfg, err)
	}
}

func TestLoadConfigRejectsNonRootOwner(t *testing.T) {
	info := configInfo(validConfig, 1000, 0o600, 42)
	files := existingConfigFS(testConfigPath, info, info, validConfig)
	_, err := loadOperatorConfigWith(testConfigPath, true, files, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Fatalf("expected root-owner error, got %v", err)
	}
}

func TestLoadConfigRejectsGroupWritableFile(t *testing.T) {
	info := configInfo(validConfig, 0, 0o620, 42)
	files := existingConfigFS(testConfigPath, info, info, validConfig)
	_, err := loadOperatorConfigWith(testConfigPath, true, files, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("expected writable error, got %v", err)
	}
}

func TestLoadConfigRejectsSymlinkFile(t *testing.T) {
	info := configInfo(validConfig, 0, os.ModeSymlink|0o777, 42)
	files := existingConfigFS(testConfigPath, info, info, validConfig)
	_, err := loadOperatorConfigWith(testConfigPath, true, files, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
}

func TestLoadConfigRejectsDirectory(t *testing.T) {
	info := configInfo(validConfig, 0, os.ModeDir|0o755, 42)
	files := existingConfigFS(testConfigPath, info, info, validConfig)
	_, err := loadOperatorConfigWith(testConfigPath, true, files, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected regular-file error, got %v", err)
	}
}

func TestLoadConfigRejectsOversizedFile(t *testing.T) {
	content := strings.Repeat("x", maxConfigBytes+1)
	info := configInfo(content, 0, 0o600, 42)
	files := existingConfigFS(testConfigPath, info, info, content)
	_, err := loadOperatorConfigWith(testConfigPath, true, files, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size error, got %v", err)
	}
}

func TestLoadConfigRejectsUntrustedParent(t *testing.T) {
	files := existingConfigFS(testConfigPath, rootConfigInfo(validConfig), rootConfigInfo(validConfig), validConfig)
	trust := func(string) error {
		return errors.New("directory /etc/incus-for-devs must not be writable by group or others")
	}
	_, err := loadOperatorConfigWith(testConfigPath, true, files, trust)
	if err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("expected untrusted-parent error, got %v", err)
	}
}

func TestLoadConfigRejectsUnsafeSymlinkParent(t *testing.T) {
	files := existingConfigFS(testConfigPath, rootConfigInfo(validConfig), rootConfigInfo(validConfig), validConfig)
	trust := func(string) error {
		return errors.New("directory /etc/incus-for-devs must be owned by root")
	}
	_, err := loadOperatorConfigWith(testConfigPath, true, files, trust)
	if err == nil || !strings.Contains(err.Error(), "untrusted parent") {
		t.Fatalf("expected unsafe-parent error, got %v", err)
	}
}

func TestLoadConfigRejectsOpenedIdentitySwap(t *testing.T) {
	lstatInfo := configInfo(validConfig, 0, 0o600, 42)
	openInfo := configInfo(validConfig, 0, 0o600, 43)
	files := existingConfigFS(testConfigPath, lstatInfo, openInfo, validConfig)
	_, err := loadOperatorConfigWith(testConfigPath, true, files, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("expected identity-change error, got %v", err)
	}
}

func TestLoadConfigRejectsPostOpenNonRootOwner(t *testing.T) {
	lstatInfo := configInfo(validConfig, 0, 0o600, 42)
	openInfo := configInfo(validConfig, 1000, 0o600, 42)
	files := existingConfigFS(testConfigPath, lstatInfo, openInfo, validConfig)
	_, err := loadOperatorConfigWith(testConfigPath, true, files, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Fatalf("expected post-open owner error, got %v", err)
	}
}

func TestLoadConfigRejectsPostOpenModeChange(t *testing.T) {
	lstatInfo := configInfo(validConfig, 0, 0o600, 42)
	openInfo := configInfo(validConfig, 0, 0o620, 42)
	files := existingConfigFS(testConfigPath, lstatInfo, openInfo, validConfig)
	_, err := loadOperatorConfigWith(testConfigPath, true, files, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("expected post-open mode error, got %v", err)
	}
}

func TestLoadConfigDefaultAbsentDanglingParentFailsClosed(t *testing.T) {
	parent := filepath.Dir(testConfigPath)
	files := configFS{
		lstat: func(path string) (os.FileInfo, error) {
			switch path {
			case testConfigPath:
				return nil, fs.ErrNotExist
			case parent:
				return configInfo("", 0, os.ModeSymlink|0o777, 1), nil
			default:
				return nil, fs.ErrNotExist
			}
		},
		open: func(string) (configFile, error) { return nil, fs.ErrNotExist },
	}
	trust := func(path string) error {
		if path == parent {
			return errors.New("dangling symlink")
		}
		return nil
	}
	_, err := loadOperatorConfigWith(testConfigPath, false, files, trust)
	if err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("expected fail-closed absent config error, got %v", err)
	}
}

func TestLoadConfigDefaultTopLevelSymlinkFailsClosed(t *testing.T) {
	info := configInfo(validConfig, 0, os.ModeSymlink|0o777, 42)
	files := existingConfigFS(testConfigPath, info, info, validConfig)
	_, err := loadOperatorConfigWith(testConfigPath, false, files, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink fail-closed error, got %v", err)
	}
}

func TestConfigAcceptsUnknownFields(t *testing.T) {
	content := `{
	  "listen": "127.0.0.1:2222",
	  "unknown_object": {"nested": [1, 2, 3]},
	  "incus_binary": "/opt/incus/bin/incus",
	  "users": [{"name": "you", "project": "ifd-dev", "extra": true}]
	}`
	cfg, err := parseOperatorConfig([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Binary != "/opt/incus/bin/incus" || cfg.Project != "ifd-dev" {
		t.Fatalf("unexpected config %+v", cfg)
	}
}

func TestConfigDefaultsBinary(t *testing.T) {
	for _, content := range []string{
		`{"users":[{"project":"ifd-dev"}]}`,
		`{"incus_binary":"","users":[{"project":"ifd-dev"}]}`,
	} {
		cfg, err := parseOperatorConfig([]byte(content))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Binary != defaultIncusBinary {
			t.Fatalf("binary = %q", cfg.Binary)
		}
	}
}

func TestConfigRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"empty", "", "empty"},
		{"array", `[]`, "JSON object"},
		{"null", `null`, "JSON object"},
		{"duplicate top-level key", `{"incus_binary":"/a","incus_binary":"/b","users":[{"project":"ifd-dev"}]}`, "duplicate"},
		{"duplicate nested key", `{"users":[{"project":"a","project":"b"}]}`, "duplicate"},
		{"trailing document", validConfig + "\n{}", "exactly one"},
		{"missing users", `{"incus_binary":"/usr/bin/incus"}`, "no users"},
		{"empty users", `{"users":[]}`, "no users"},
		{"users not array", `{"users":{}}`, "array"},
		{"binary not string", `{"incus_binary":1,"users":[{"project":"ifd-dev"}]}`, "string"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseOperatorConfig([]byte(test.content))
			if err == nil {
				t.Fatalf("expected parse error, got nil")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestConfigRejectsInvalidDefaultProject(t *testing.T) {
	for _, project := range []string{"", "default", "Upper", "1bad", "ifd_dev", "-bad", "bad-"} {
		content := `{"users":[{"project":"` + project + `"}]}`
		if _, err := parseOperatorConfig([]byte(content)); err == nil {
			t.Fatalf("project %q must be rejected", project)
		}
	}
}

func TestConfigUsesFirstUserProject(t *testing.T) {
	content := `{"users":[{"project":"first-project"},{"project":"second-project"}]}`
	cfg, err := parseOperatorConfig([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project != "first-project" {
		t.Fatalf("project = %q", cfg.Project)
	}
}
