package operator

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testConfigPath = "/etc/incus-for-devs/config.json"

type execCall struct {
	path string
	argv []string
	env  []string
}

type runStub struct {
	calls   []execCall
	checked []string
}

func stubRun(t *testing.T, euid int, trustErr error) *runStub {
	t.Helper()
	oldEUID, oldExec, oldTrust, oldDefault := geteuid, execProcess, trustedExecutable, defaultConfigPath
	oldFiles, oldDirectory := configFiles, trustedDirectory
	stub := &runStub{}
	geteuid = func() int { return euid }
	execProcess = func(path string, argv, env []string) error {
		stub.calls = append(stub.calls, execCall{
			path: path,
			argv: append([]string(nil), argv...),
			env:  append([]string(nil), env...),
		})
		return nil
	}
	trustedExecutable = func(path string) error {
		stub.checked = append(stub.checked, path)
		return trustErr
	}
	defaultConfigPath = testConfigPath
	configFiles = absentConfigFS(defaultConfigPath)
	trustedDirectory = func(string) error { return nil }
	t.Cleanup(func() {
		geteuid, execProcess, trustedExecutable, defaultConfigPath = oldEUID, oldExec, oldTrust, oldDefault
		configFiles, trustedDirectory = oldFiles, oldDirectory
	})
	return stub
}

func useConfigAt(t *testing.T, path, content string, info os.FileInfo) {
	t.Helper()
	useConfigFS(t, existingConfigFS(path, info, info, content), func(string) error { return nil })
}

func useConfigFS(t *testing.T, files configFS, trustDir func(string) error) {
	t.Helper()
	oldFiles, oldDirectory := configFiles, trustedDirectory
	configFiles, trustedDirectory = files, trustDir
	t.Cleanup(func() {
		configFiles, trustedDirectory = oldFiles, oldDirectory
	})
}

type testFileInfo struct {
	name string
	mode os.FileMode
	size int64
	stat syscall.Stat_t
}

func (f testFileInfo) Name() string       { return f.name }
func (f testFileInfo) Size() int64        { return f.size }
func (f testFileInfo) Mode() os.FileMode  { return f.mode }
func (f testFileInfo) ModTime() time.Time { return time.Time{} }
func (f testFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f testFileInfo) Sys() any           { s := f.stat; return &s }

func configInfo(content string, uid uint32, mode os.FileMode, inode uint64) os.FileInfo {
	return testFileInfo{
		name: "config.json",
		mode: mode,
		size: int64(len(content)),
		stat: syscall.Stat_t{Uid: uid, Dev: 1, Ino: inode},
	}
}

func rootConfigInfo(content string) os.FileInfo {
	return configInfo(content, 0, 0o600, 42)
}

func dirInfo(name string) os.FileInfo {
	return testFileInfo{name: name, mode: os.ModeDir | 0o755, stat: syscall.Stat_t{Uid: 0, Dev: 1, Ino: 1}}
}

type testConfigFile struct {
	info   os.FileInfo
	data   []byte
	offset int
	closed bool
}

func (f *testConfigFile) Stat() (os.FileInfo, error) { return f.info, nil }

func (f *testConfigFile) Read(p []byte) (int, error) {
	if f.offset >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += n
	return n, nil
}

func (f *testConfigFile) Close() error {
	f.closed = true
	return nil
}

func existingConfigFS(path string, lstatInfo, openInfo os.FileInfo, content string) configFS {
	return configFS{
		lstat: func(candidate string) (os.FileInfo, error) {
			if candidate == path {
				return lstatInfo, nil
			}
			return nil, fs.ErrNotExist
		},
		open: func(candidate string) (configFile, error) {
			if candidate != path {
				return nil, fs.ErrNotExist
			}
			return &testConfigFile{info: openInfo, data: []byte(content)}, nil
		},
		sameFile: fakeSameFile,
	}
}

func fakeSameFile(a, b os.FileInfo) bool {
	first, ok := a.Sys().(*syscall.Stat_t)
	if !ok || first == nil {
		return false
	}
	second, ok := b.Sys().(*syscall.Stat_t)
	if !ok || second == nil {
		return false
	}
	return first.Dev == second.Dev && first.Ino == second.Ino
}

func absentConfigFS(path string) configFS {
	parent := filepath.Dir(path)
	return configFS{
		lstat: func(candidate string) (os.FileInfo, error) {
			if candidate == path || candidate == parent {
				return nil, fs.ErrNotExist
			}
			return dirInfo(filepath.Base(candidate)), nil
		},
		open: func(string) (configFile, error) {
			return nil, fs.ErrNotExist
		},
	}
}

const validConfig = `{
  "listen": "127.0.0.1:2222",
  "host_key": "/var/lib/incus-for-devs/host.key",
  "incus_binary": "/usr/bin/incus",
  "image": "local:ifd-ubuntu-24-04",
  "limits": {"cpu": 2, "memory": "4GiB"},
  "users": [
    {"name": "you", "project": "ifd-dev", "authorized_keys": ["ssh-ed25519 AAAA"]}
  ]
}`

func TestRunRequiresRootBeforeExec(t *testing.T) {
	stub := stubRun(t, 1000, nil)
	err := Run([]string{"--config", testConfigPath, "--", "version"})
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("expected root error, got %v", err)
	}
	if len(stub.calls) != 0 || len(stub.checked) != 0 {
		t.Fatal("must not check trust or execute without root")
	}
}

func TestRunHelpReturnsWithoutRootOrExec(t *testing.T) {
	stub := stubRun(t, 1000, errors.New("must not check trust"))
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"--config", "/nonexistent/config.json", "-h"},
	} {
		if err := Run(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if len(stub.calls) != 0 || len(stub.checked) != 0 {
		t.Fatal("help must not execute Incus")
	}
}

func TestRunExecutesNativeArgumentsUnchanged(t *testing.T) {
	stub := stubRun(t, 0, nil)
	useConfigAt(t, testConfigPath, validConfig, rootConfigInfo(validConfig))
	t.Setenv("HOME", "/home/app")
	t.Setenv("PATH", "/tmp/evil")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("INCUS_REMOTE", "attacker")
	t.Setenv("LXD_DIR", "/tmp/lxd")
	t.Setenv("KEEP_ME", "value with spaces")
	native := []string{
		"exec", "demo", "--env", "FOO=a b", "--",
		"/bin/sh", "-lc", "printf '%s\\n' 'query?project=ifd-dev&x=1'",
	}
	args := append([]string{"--config", testConfigPath, "--"}, native...)
	if err := Run(args); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected one exec, got %d", len(stub.calls))
	}
	call := stub.calls[0]
	if call.path != "/usr/bin/incus" {
		t.Fatalf("unexpected executable %q", call.path)
	}
	wantArgv := append([]string{"/usr/bin/incus", "--force-local", "--project", "ifd-dev"}, native...)
	if !reflect.DeepEqual(call.argv, wantArgv) {
		t.Fatalf("argv mismatch\n got: %#v\nwant: %#v", call.argv, wantArgv)
	}
	env := envMap(call.env)
	for _, name := range []string{"LD_PRELOAD", "INCUS_REMOTE", "LXD_DIR"} {
		if _, ok := env[name]; ok {
			t.Fatalf("%s must be stripped from the root environment", name)
		}
	}
	if env["HOME"] != "/root" {
		t.Fatalf("HOME = %q, want /root", env["HOME"])
	}
	if env["PATH"] != minimalPath {
		t.Fatalf("PATH = %q, want %q", env["PATH"], minimalPath)
	}
	if env["TERM"] != "xterm-256color" {
		t.Fatalf("TERM = %q", env["TERM"])
	}
	if env["KEEP_ME"] != "value with spaces" {
		t.Fatalf("KEEP_ME = %q", env["KEEP_ME"])
	}
}

func TestRunForwardsExplicitlyClearedProject(t *testing.T) {
	for _, native := range [][]string{
		{"--project=", "query", "/1.0"},
		{"--project=", "list", "--all-projects"},
	} {
		t.Run(strings.Join(native, " "), func(t *testing.T) {
			stub := stubRun(t, 0, nil)
			useConfigAt(t, testConfigPath, validConfig, rootConfigInfo(validConfig))
			args := append([]string{"--config", testConfigPath, "--"}, native...)
			if err := Run(args); err != nil {
				t.Fatal(err)
			}
			want := append([]string{"/usr/bin/incus", "--force-local", "--project", "ifd-dev"}, native...)
			if len(stub.calls) != 1 || !reflect.DeepEqual(stub.calls[0].argv, want) {
				t.Fatalf("native override not forwarded: %#v", stub.calls)
			}
		})
	}
}

func TestRunWithoutExplicitConfigUsesMissingDefault(t *testing.T) {
	stub := stubRun(t, 0, nil)
	if err := Run([]string{"--", "list", "--json"}); err != nil {
		t.Fatal(err)
	}
	call := stub.calls[0]
	want := []string{"/usr/bin/incus", "--force-local", "list", "--json"}
	if !reflect.DeepEqual(call.argv, want) {
		t.Fatalf("argv = %#v, want %#v", call.argv, want)
	}
}

func TestRunUsesDefaultConfigWhenPresent(t *testing.T) {
	stub := stubRun(t, 0, nil)
	useConfigAt(t, defaultConfigPath, validConfig, rootConfigInfo(validConfig))
	if err := Run([]string{"--", "list"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/incus", "--force-local", "--project", "ifd-dev", "list"}
	if !reflect.DeepEqual(stub.calls[0].argv, want) {
		t.Fatalf("argv = %#v, want %#v", stub.calls[0].argv, want)
	}
}

func TestRunInvalidDefaultConfigDoesNotFallback(t *testing.T) {
	stub := stubRun(t, 0, nil)
	content := `{"users":[{"project":"default"}]}`
	useConfigAt(t, defaultConfigPath, content, rootConfigInfo(content))
	if err := Run([]string{"--", "list"}); err == nil {
		t.Fatal("expected invalid default config error")
	}
	if len(stub.calls) != 0 {
		t.Fatal("must not fall back when the default config exists but is invalid")
	}
}

func TestRunExplicitConfigMustExist(t *testing.T) {
	stub := stubRun(t, 0, nil)
	if err := Run([]string{"--config", testConfigPath, "--", "list"}); err == nil {
		t.Fatal("expected missing explicit config error")
	}
	if len(stub.calls) != 0 {
		t.Fatal("must not execute with a missing explicit config")
	}
}

func TestRunRejectsUntrustedBinaryBeforeExec(t *testing.T) {
	stub := stubRun(t, 0, errors.New("not root owned"))
	useConfigAt(t, testConfigPath, validConfig, rootConfigInfo(validConfig))
	if err := Run([]string{"--config", testConfigPath, "--", "list"}); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("expected untrusted-binary error, got %v", err)
	}
	if len(stub.calls) != 0 {
		t.Fatal("must not execute an untrusted binary")
	}
	if len(stub.checked) != 1 || stub.checked[0] != "/usr/bin/incus" {
		t.Fatalf("checked binary paths = %#v", stub.checked)
	}
}

func TestRunChecksAndExecutesSameNonDefaultBinary(t *testing.T) {
	stub := stubRun(t, 0, nil)
	content := `{"incus_binary":"/opt/incus/bin/incus","users":[{"project":"ifd-dev"}]}`
	useConfigAt(t, testConfigPath, content, rootConfigInfo(content))
	if err := Run([]string{"--config", testConfigPath, "--", "list"}); err != nil {
		t.Fatal(err)
	}
	if len(stub.checked) != 1 || stub.checked[0] != "/opt/incus/bin/incus" {
		t.Fatalf("checked binary paths = %#v", stub.checked)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected one exec, got %d", len(stub.calls))
	}
	call := stub.calls[0]
	if call.path != "/opt/incus/bin/incus" || call.argv[0] != "/opt/incus/bin/incus" {
		t.Fatalf("executed path/argv0 = %q/%q", call.path, call.argv[0])
	}
	if stub.checked[0] != call.path || stub.checked[0] != call.argv[0] {
		t.Fatalf("checked %q but executed %q with argv0 %q", stub.checked[0], call.path, call.argv[0])
	}
}

func TestRunPrintsNothingBeforeExec(t *testing.T) {
	stubRun(t, 0, nil)
	useConfigAt(t, testConfigPath, validConfig, rootConfigInfo(validConfig))
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	runErr := Run([]string{"--config", testConfigPath, "--", "list", "--json"})
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, _ := io.ReadAll(outR)
	errt, _ := io.ReadAll(errR)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(out) != 0 || len(errt) != 0 {
		t.Fatalf("operator wrote before exec: stdout=%q stderr=%q", out, errt)
	}
}

func TestRunNonFlagNativeArgsDoNotNeedSeparator(t *testing.T) {
	stub := stubRun(t, 0, nil)
	if err := Run([]string{"list", "--json"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/incus", "--force-local", "list", "--json"}
	if !reflect.DeepEqual(stub.calls[0].argv, want) {
		t.Fatalf("argv = %#v, want %#v", stub.calls[0].argv, want)
	}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			out[name] = value
		}
	}
	return out
}
