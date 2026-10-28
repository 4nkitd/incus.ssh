package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/names"
)

var alice = core.Principal{Name: "alice", Project: "developers"}

type scenario struct {
	Block              string
	Child              bool
	Exit               int
	Failure            string
	Stdout             []byte
	Stderr             []byte
	Echo               bool
	LargeOutput        int
	LaunchDelay        time.Duration
	WaitForExecRelease bool
}

type fixture struct {
	dir     string
	config  Config
	backend *Backend
}

func newFixture(t *testing.T, items []instance, behavior scenario) *fixture {
	t.Helper()
	dir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "incus")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nexport GORACE=atexit_sleep_ms=0\nexec " + quote(testBinary) + " -test.run=^TestIncusHelper$ -- " + quote(dir) + " \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "instances.json"), items)
	writeJSON(t, filepath.Join(dir, "scenario.json"), behavior)
	config := Config{Binary: binary, Image: "images:debian/12", Profile: "development", Limits: core.Limits{MaxInstances: 3, CPU: 2, Memory: "2GiB", Processes: 512}, OperationTimeout: 5 * time.Second, AvailableMemory: func() (uint64, error) { return 16 << 30, nil }}
	backend, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: dir, config: config, backend: backend}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) calls(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "calls.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var result [][]string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var args []string
		if err := json.Unmarshal(line, &args); err != nil {
			t.Fatal(err)
		}
		result = append(result, args)
	}
	return result
}

func ownedInstance(name, owner string) instance {
	return instance{Name: name, Project: alice.Project, Type: "container", Status: "Running", Config: map[string]string{ownerKey: owner, "limits.cpu": "1", "limits.memory": "2GiB", "limits.processes": "256"}}
}

func TestIncusHelper(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	args := os.Args[separator+1:]
	if len(args) == 1 && args[0] == "child" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if len(args) < 6 {
		os.Exit(91)
	}
	dir, args := args[0], args[1:]
	if !reflect.DeepEqual(args[:4], []string{"--force-local", "--project", alice.Project, "--quiet"}) {
		os.Exit(92)
	}
	log, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(93)
	}
	_ = json.NewEncoder(log).Encode(args)
	_ = log.Close()
	var behavior scenario
	data, _ := os.ReadFile(filepath.Join(dir, "scenario.json"))
	if json.Unmarshal(data, &behavior) != nil {
		os.Exit(94)
	}
	data, _ = json.Marshal(os.Environ())
	_ = os.WriteFile(filepath.Join(dir, "env.json"), data, 0600)
	action := args[4]
	if behavior.Block == action {
		signals := make(chan os.Signal, 8)
		signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGWINCH)
		if behavior.Child {
			executable, _ := os.Executable()
			child := exec.Command(executable, "-test.run=^TestIncusHelper$", "--", "child")
			if child.Start() != nil {
				os.Exit(95)
			}
			_ = os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600)
		}
		if size, err := pty.GetsizeFull(os.Stdin); err == nil {
			data, _ := json.Marshal(size)
			_ = os.WriteFile(filepath.Join(dir, "size.json"), data, 0600)
		}
		_ = os.WriteFile(filepath.Join(dir, "ready.pid"), []byte(strconv.Itoa(os.Getpid())), 0600)
		for received := range signals {
			if received == syscall.SIGINT {
				os.Exit(130)
			}
			if received == syscall.SIGWINCH {
				if size, err := pty.GetsizeFull(os.Stdin); err == nil {
					data, _ := json.Marshal(size)
					_ = os.WriteFile(filepath.Join(dir, "resized.json"), data, 0600)
				}
			}
		}
	}
	if behavior.Failure == action {
		_, _ = os.Stderr.Write(behavior.Stderr)
		os.Exit(behavior.Exit)
	}
	if behavior.LargeOutput > 0 && action == "list" {
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", behavior.LargeOutput))
		os.Exit(0)
	}
	var items []instance
	data, _ = os.ReadFile(filepath.Join(dir, "instances.json"))
	_ = json.Unmarshal(data, &items)
	switch action {
	case "list":
		_ = json.NewEncoder(os.Stdout).Encode(items)
	case "launch":
		time.Sleep(behavior.LaunchDelay)
		item := ownedInstance(args[6], alice.Name)
		for i := 7; i+1 < len(args); i++ {
			if args[i] == "--config" {
				parts := strings.SplitN(args[i+1], "=", 2)
				item.Config[parts[0]] = parts[1]
			}
		}
		items = append(items, item)
		data, _ = json.Marshal(items)
		_ = os.WriteFile(filepath.Join(dir, "instances.json"), data, 0600)
	case "delete", "start", "stop", "restart":
	case "exec":
		if behavior.WaitForExecRelease {
			for {
				if _, err := os.Stat(filepath.Join(dir, "exec.release")); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		_, _ = os.Stdout.Write(behavior.Stdout)
		_, _ = os.Stderr.Write(behavior.Stderr)
		if behavior.Echo {
			_, _ = io.Copy(os.Stdout, os.Stdin)
		}
		os.Exit(behavior.Exit)
	default:
		os.Exit(96)
	}
	os.Exit(0)
}

func TestNewRejectsInvalidConfigurationWithoutRunningCLI(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"relative binary", func(c *Config) { c.Binary = "incus" }},
		{"binary control", func(c *Config) { c.Binary = "/bin/incus\n" }},
		{"image option", func(c *Config) { c.Image = "--empty" }},
		{"image shell", func(c *Config) { c.Image = "images:debian/12;id" }},
		{"image traversal", func(c *Config) { c.Image = "../secret" }},
		{"profile remote", func(c *Config) { c.Profile = "remote:profile" }},
		{"zero CPU", func(c *Config) { c.Limits.CPU = 0 }},
		{"negative maximum", func(c *Config) { c.Limits.MaxInstances = -1 }},
		{"negative processes", func(c *Config) { c.Limits.Processes = -1 }},
		{"memory overflow", func(c *Config) { c.Limits.Memory = "18446744073709551615GiB" }},
		{"memory zero", func(c *Config) { c.Limits.Memory = "0GiB" }},
		{"disk negative", func(c *Config) { c.Limits.Disk = "-1GiB" }},
		{"negative timeout", func(c *Config) { c.OperationTimeout = -time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := f.config
			test.change(&config)
			if _, err := New(config); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	config := f.config
	config.Binary = "/does/not/exist/incus"
	config.OperationTimeout = 0
	config.Limits.Processes = 0
	backend, err := New(config)
	if err != nil {
		t.Fatalf("constructor attempted execution or rejected timeout default: %v", err)
	}
	if backend.config.Limits.Processes != defaultProcesses {
		t.Fatalf("process maximum default = %d", backend.config.Limits.Processes)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatalf("constructor ran CLI: %v", calls)
	}
}

func TestInvalidNamesAndPrincipalsNeverReachCLI(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	for _, name := range []string{"-x", "--help", "remote:name", "a/b", "a b", "a\n", "a\x00", "dévelop", "$(id)", "a;id", "Aname", strings.Repeat("a", 64)} {
		if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: name}); err == nil {
			t.Errorf("Create accepted %q", name)
		}
		if err := f.backend.Remove(context.Background(), alice, name); err == nil {
			t.Errorf("Remove accepted %q", name)
		}
		if err := f.backend.Resume(context.Background(), alice, name); err == nil {
			t.Errorf("Resume accepted %q", name)
		}
		if err := f.backend.Start(context.Background(), alice, name); err == nil {
			t.Errorf("Start accepted %q", name)
		}
		if err := f.backend.Stop(context.Background(), alice, name); err == nil {
			t.Errorf("Stop accepted %q", name)
		}
		if err := f.backend.Restart(context.Background(), alice, name); err == nil {
			t.Errorf("Restart accepted %q", name)
		}
		if _, err := f.backend.Exec(context.Background(), alice, name, "true", core.Streams{}); err == nil {
			t.Errorf("Exec accepted %q", name)
		}
	}
	for _, principal := range []core.Principal{{Name: "alice", Project: "remote:project"}, {Name: "alice", Project: "--all-projects"}, {Name: "a\n", Project: "developers"}, {Name: "alice"}} {
		if _, err := f.backend.List(context.Background(), principal); err == nil {
			t.Errorf("List accepted %#v", principal)
		}
		if _, err := f.backend.Create(context.Background(), principal, core.CreateOptions{}); err == nil {
			t.Errorf("Create accepted %#v", principal)
		}
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatalf("invalid values reached CLI: %v", calls)
	}
}

func TestOwnershipAndProjectScoping(t *testing.T) {
	mine := ownedInstance("mine", "alice")
	foreign := ownedInstance("foreign", "bob")
	unlabelled := ownedInstance("unlabelled", "")
	unlabelled.ExpandedConfig = map[string]string{ownerKey: "alice", "limits.memory": "2GiB"}
	wrongProject := ownedInstance("wrong-project", "alice")
	wrongProject.Project = "other"
	vm := ownedInstance("machine", "alice")
	vm.Type = "virtual-machine"
	f := newFixture(t, []instance{mine, foreign, unlabelled, wrongProject, vm}, scenario{})
	items, err := f.backend.List(context.Background(), alice)
	if err != nil || len(items) != 1 || items[0].Name != "mine" {
		t.Fatalf("list = %#v, %v", items, err)
	}
	for _, name := range []string{"foreign", "unlabelled", "wrong-project", "machine", "missing"} {
		if err := f.backend.Remove(context.Background(), alice, name); err == nil {
			t.Errorf("Remove allowed %s", name)
		}
		if err := f.backend.Resume(context.Background(), alice, name); err == nil {
			t.Errorf("Resume allowed %s", name)
		}
		if err := f.backend.Start(context.Background(), alice, name); err == nil {
			t.Errorf("Start allowed %s", name)
		}
		if err := f.backend.Stop(context.Background(), alice, name); err == nil {
			t.Errorf("Stop allowed %s", name)
		}
		if err := f.backend.Restart(context.Background(), alice, name); err == nil {
			t.Errorf("Restart allowed %s", name)
		}
		if _, err := f.backend.Exec(context.Background(), alice, name, "true", core.Streams{}); err == nil {
			t.Errorf("Exec allowed %s", name)
		}
	}
	for _, call := range f.calls(t) {
		if call[4] != "list" {
			t.Fatalf("unauthorized mutation: %v", call)
		}
	}
	if err := f.backend.Resume(context.Background(), alice, "mine"); err != nil {
		t.Fatal(err)
	}
	if err := f.backend.Start(context.Background(), alice, "mine"); err != nil {
		t.Fatal(err)
	}
	if err := f.backend.Stop(context.Background(), alice, "mine"); err != nil {
		t.Fatal(err)
	}
	if err := f.backend.Restart(context.Background(), alice, "mine"); err != nil {
		t.Fatal(err)
	}
	if err := f.backend.Remove(context.Background(), alice, "mine"); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	want := [][]string{
		{"start", "mine"},
		{"start", "mine"},
		{"stop", "mine", "--force=false", "--timeout=5"},
		{"restart", "mine", "--force=false", "--timeout=5"},
		{"delete", "mine", "--force"},
	}
	offset := len(calls) - 2*len(want)
	for i, wantArgs := range want {
		if got := calls[offset+2*i][4:]; !reflect.DeepEqual(got, []string{"list", "--format=json"}) {
			t.Fatalf("ownership check before %q = %q", wantArgs[0], got)
		}
		if got := calls[offset+2*i+1][4:]; !reflect.DeepEqual(got, wantArgs) {
			t.Fatalf("lifecycle args = %q, want %q", got, wantArgs)
		}
	}
}

func TestLifecycleArguments(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("mine", alice.Name)}, scenario{})
	actions := []struct {
		name string
		call func() error
		want []string
	}{
		{"start", func() error { return f.backend.Start(context.Background(), alice, "mine") }, []string{"start", "mine"}},
		{"stop", func() error { return f.backend.Stop(context.Background(), alice, "mine") }, []string{"stop", "mine", "--force=false", "--timeout=5"}},
		{"restart", func() error { return f.backend.Restart(context.Background(), alice, "mine") }, []string{"restart", "mine", "--force=false", "--timeout=5"}},
	}
	for _, action := range actions {
		if err := action.call(); err != nil {
			t.Fatalf("%s: %v", action.name, err)
		}
		calls := f.calls(t)
		last := calls[len(calls)-1]
		if !reflect.DeepEqual(last[:4], []string{"--force-local", "--project", alice.Project, "--quiet"}) {
			t.Fatalf("%s lost invocation prefix: %q", action.name, last)
		}
		if got := last[4:]; !reflect.DeepEqual(got, action.want) {
			t.Fatalf("%s args = %q, want %q", action.name, got, action.want)
		}
	}
}

func TestResourceCeilingsAndLaunchArguments(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	config := f.config
	config.Limits.Disk = "10GiB"
	backend, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []core.CreateOptions{{CPU: -1}, {CPU: 3}, {Processes: -1}, {Processes: 513}, {Memory: "3GiB"}, {Memory: "2147483649B"}, {Memory: "-1MB"}, {Memory: "0MiB"}, {Memory: "18446744073709551615GiB"}, {Memory: "2GiB --config=x"}, {Disk: "11GiB"}, {Disk: "0GB"}} {
		if _, err := backend.Create(context.Background(), alice, options); err == nil {
			t.Errorf("accepted resources %#v", options)
		}
	}
	if len(f.calls(t)) != 0 {
		t.Fatal("invalid resources reached CLI")
	}
	item, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "small", CPU: 1, Memory: "2000MB", Processes: 64, Disk: "9GB"})
	if err != nil {
		t.Fatal(err)
	}
	if item.CPU != "1" || item.Memory != "2000MB" || item.Processes != "64" || item.Disk != "9GB" {
		t.Fatalf("wrong resources: %#v", item)
	}
	calls := f.calls(t)
	got := calls[len(calls)-1]
	want := []string{"--force-local", "--project", "developers", "--quiet", "launch", "images:debian/12", "small", "--profile", "development", "--config", ownerKey + "=alice", "--config", "limits.cpu=1", "--config", "limits.memory=2000MB", "--config", "limits.processes=64", "--config", "boot.autostart=false", "--config", "security.privileged=false", "--config", "security.nesting=false", "--device", "root,size=9GB"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("launch arguments\ngot:  %q\nwant: %q", got, want)
	}
	item, err = backend.Create(context.Background(), alice, core.CreateOptions{})
	if err != nil || !names.Valid(item.Name) || !validName(item.Name) || item.CPU != "1" || item.Memory != "2GiB" || item.Processes != "256" || item.Disk != "10GiB" {
		t.Fatalf("default create = %#v, %v", item, err)
	}
	launch := f.calls(t)
	last := launch[len(launch)-1]
	for _, arg := range []string{"limits.cpu=1", "limits.memory=2GiB", "limits.processes=256"} {
		found := false
		for _, value := range last {
			found = found || value == arg
		}
		if !found {
			t.Fatalf("default launch missing %q: %q", arg, last)
		}
	}
}

func TestDefaultsAndExplicitRequestsWithinMaxima(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	config := f.config
	config.Limits = core.Limits{MaxInstances: 2, CPU: 4, Memory: "4GiB", Processes: 512}
	config.AvailableMemory = func() (uint64, error) { return 8 << 30, nil }
	backend, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	item, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "defaults"})
	if err != nil || item.CPU != "1" || item.Memory != "2GiB" || item.Processes != "256" {
		t.Fatalf("default create = %#v, %v", item, err)
	}
	item, err = backend.Create(context.Background(), alice, core.CreateOptions{Name: "explicit", CPU: 1, Memory: "512MiB", Processes: 64})
	if err != nil || item.CPU != "1" || item.Memory != "512MiB" || item.Processes != "64" {
		t.Fatalf("explicit create = %#v, %v", item, err)
	}
	calls := f.calls(t)
	defaultLaunch := calls[1]
	for _, arg := range []string{"limits.cpu=1", "limits.memory=2GiB", "limits.processes=256"} {
		found := false
		for _, value := range defaultLaunch {
			found = found || value == arg
		}
		if !found {
			t.Fatalf("default launch missing %q: %q", arg, defaultLaunch)
		}
	}
	last := calls[len(calls)-1]
	for _, arg := range []string{"limits.cpu=1", "limits.memory=512MiB", "limits.processes=64"} {
		found := false
		for _, value := range last {
			found = found || value == arg
		}
		if !found {
			t.Fatalf("explicit launch missing %q: %q", arg, last)
		}
	}
}

func TestMemoryAdmission(t *testing.T) {
	newAdmissionBackend := func(t *testing.T, available uint64) (*fixture, *Backend) {
		t.Helper()
		f := newFixture(t, nil, scenario{})
		config := f.config
		config.Limits.Memory = "4GiB"
		config.AvailableMemory = func() (uint64, error) { return available, nil }
		backend, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		return f, backend
	}

	t.Run("accepts boundary", func(t *testing.T) {
		f, backend := newAdmissionBackend(t, 3<<30)
		if _, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "boundary", Memory: "2GiB"}); err != nil {
			t.Fatal(err)
		}
		if len(f.calls(t)) != 2 {
			t.Fatalf("boundary request did not launch: %v", f.calls(t))
		}
	})

	t.Run("rejects above boundary", func(t *testing.T) {
		f, backend := newAdmissionBackend(t, 3<<30)
		if _, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "over", Memory: "3221225473B"}); err == nil || !strings.Contains(err.Error(), "cannot admit") {
			t.Fatalf("above boundary error = %v", err)
		}
		for _, call := range f.calls(t) {
			if call[4] == "launch" {
				t.Fatalf("rejected request launched: %v", call)
			}
		}
	})

	t.Run("rejects when reserve would be consumed", func(t *testing.T) {
		f, backend := newAdmissionBackend(t, 2<<30)
		if _, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "tight", Memory: "2GiB"}); err == nil || !strings.Contains(err.Error(), "cannot admit") {
			t.Fatalf("insufficient host memory error = %v", err)
		}
		for _, call := range f.calls(t) {
			if call[4] == "launch" {
				t.Fatalf("rejected request launched: %v", call)
			}
		}
	})

	t.Run("fails closed when availability is unknown", func(t *testing.T) {
		f := newFixture(t, nil, scenario{})
		f.backend.config.AvailableMemory = func() (uint64, error) { return 0, errors.New("no meminfo") }
		if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "unknown"}); err == nil || !strings.Contains(err.Error(), "cannot verify host memory availability") {
			t.Fatalf("unknown availability error = %v", err)
		}
		for _, call := range f.calls(t) {
			if call[4] == "launch" {
				t.Fatalf("unknown availability launched: %v", call)
			}
		}
	})
}

func TestSequentialCreatesShareCommittedMemoryBudget(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	config := f.config
	config.Limits = core.Limits{MaxInstances: 10, CPU: 2, Memory: "4GiB", Processes: 512}
	config.AvailableMemory = func() (uint64, error) { return 4 << 30, nil }
	backend, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "first", Memory: "2GiB"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "second", Memory: "2GiB"}); err == nil || !strings.Contains(err.Error(), "cannot admit") {
		t.Fatalf("overcommitted aggregate error = %v", err)
	}
	if _, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "small", Memory: "512MiB"}); err != nil {
		t.Fatalf("create within remaining budget: %v", err)
	}
	if _, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "exact", Memory: "512MiB"}); err != nil {
		t.Fatalf("create at exact remaining budget: %v", err)
	}
	if _, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "overflow", Memory: "1B"}); err == nil || !strings.Contains(err.Error(), "cannot admit") {
		t.Fatalf("create past exact budget error = %v", err)
	}
	for _, call := range f.calls(t) {
		if call[4] == "launch" && strings.Contains(strings.Join(call, " "), "overflow") {
			t.Fatalf("rejected create launched: %v", call)
		}
	}
}

func TestUnenforceableExistingMemoryFailsClosed(t *testing.T) {
	for _, raw := range []string{"", "unbounded", "0GiB"} {
		t.Run(raw, func(t *testing.T) {
			item := ownedInstance("existing", alice.Name)
			item.Status = "Stopped"
			if raw == "" {
				delete(item.Config, "limits.memory")
			} else {
				item.Config["limits.memory"] = raw
			}
			f := newFixture(t, []instance{item}, scenario{})
			if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "new"}); err == nil || !strings.Contains(err.Error(), "cannot verify project memory") {
				t.Fatalf("create error = %v", err)
			}
			if err := f.backend.Start(context.Background(), alice, "existing"); err == nil || !strings.Contains(err.Error(), "cannot verify instance memory") {
				t.Fatalf("start error = %v", err)
			}
			if err := f.backend.Restart(context.Background(), alice, "existing"); err == nil || !strings.Contains(err.Error(), "cannot verify instance memory") {
				t.Fatalf("restart error = %v", err)
			}
			for _, call := range f.calls(t) {
				if call[4] != "list" {
					t.Fatalf("fail-closed check mutated project: %v", call)
				}
			}
		})
	}
}

func TestLifecycleAdmissionExcludesTargetFromCommittedMemory(t *testing.T) {
	// Budget is 5GiB - 1GiB reserve = 4GiB. Other already commits 2GiB, so the
	// target's 2GiB fits only when the target is excluded from the total.
	started := ownedInstance("target", alice.Name)
	started.Status = "Stopped"
	f := newFixture(t, []instance{started, ownedInstance("other", alice.Name)}, scenario{})
	f.backend.config.AvailableMemory = func() (uint64, error) { return 5 << 30, nil }
	if err := f.backend.Start(context.Background(), alice, "target"); err != nil {
		t.Fatalf("start at aggregate boundary: %v", err)
	}
	calls := f.calls(t)
	if got := calls[len(calls)-1][4:]; !reflect.DeepEqual(got, []string{"start", "target"}) {
		t.Fatalf("start args = %q", got)
	}

	restarted := ownedInstance("target", alice.Name)
	f = newFixture(t, []instance{restarted, ownedInstance("other", alice.Name)}, scenario{})
	f.backend.config.AvailableMemory = func() (uint64, error) { return 5 << 30, nil }
	if err := f.backend.Restart(context.Background(), alice, "target"); err != nil {
		t.Fatalf("restart at aggregate boundary: %v", err)
	}
	calls = f.calls(t)
	if got := calls[len(calls)-1][4:]; !reflect.DeepEqual(got, []string{"restart", "target", "--force=false", "--timeout=5"}) {
		t.Fatalf("restart args = %q", got)
	}

	tooSmall := ownedInstance("target", alice.Name)
	tooSmall.Status = "Stopped"
	f = newFixture(t, []instance{tooSmall, ownedInstance("other", alice.Name)}, scenario{})
	f.backend.config.AvailableMemory = func() (uint64, error) { return 3 << 30, nil }
	if err := f.backend.Start(context.Background(), alice, "target"); err == nil || !strings.Contains(err.Error(), "cannot admit") {
		t.Fatalf("start over budget error = %v", err)
	}
	if err := f.backend.Restart(context.Background(), alice, "target"); err == nil || !strings.Contains(err.Error(), "cannot admit") {
		t.Fatalf("restart over budget error = %v", err)
	}
	for _, call := range f.calls(t) {
		if call[4] == "start" || call[4] == "restart" {
			t.Fatalf("over-budget lifecycle reached CLI: %v", call)
		}
	}
}

func TestStartCannotExceedAdministratorMemoryCap(t *testing.T) {
	target := ownedInstance("target", alice.Name)
	target.Status = "Stopped"
	target.Config["limits.memory"] = "3GiB"
	f := newFixture(t, []instance{target}, scenario{})
	if err := f.backend.Start(context.Background(), alice, "target"); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("start above administrator cap error = %v", err)
	}
	for _, call := range f.calls(t) {
		if call[4] != "list" {
			t.Fatalf("over-cap start reached CLI: %v", call)
		}
	}
}

func TestRestartRunningGuestDoesNotDoubleChargeResidentMemory(t *testing.T) {
	target := ownedInstance("target", alice.Name)
	target.Config["limits.memory"] = "4GiB"
	f := newFixture(t, []instance{target}, scenario{})
	f.backend.memory = 4 << 30
	f.backend.config.Limits.Memory = "4GiB"
	f.backend.config.AvailableMemory = func() (uint64, error) {
		t.Fatal("running guest memory was charged a second time")
		return 0, nil
	}
	if err := f.backend.Restart(context.Background(), alice, target.Name); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	if calls[len(calls)-1][4] != "restart" {
		t.Fatalf("restart did not reach Incus: %v", calls)
	}
	// Lowered operator ceilings remain authoritative even for a running guest.
	f.backend.memory = 2 << 30
	if err := f.backend.Restart(context.Background(), alice, target.Name); err == nil {
		t.Fatal("restart ignored the lower operator ceiling")
	}
}

func TestStartCannotExceedAdministratorCPUOrProcessCaps(t *testing.T) {
	target := ownedInstance("target", alice.Name)
	target.Status = "Stopped"
	target.Config["limits.cpu"] = "2"
	f := newFixture(t, []instance{target}, scenario{})
	config := f.config
	config.Limits.CPU = 1
	backend, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(context.Background(), alice, "target"); err == nil || !strings.Contains(err.Error(), "CPU limit") {
		t.Fatalf("start above CPU cap error = %v", err)
	}

	target = ownedInstance("target", alice.Name)
	target.Status = "Stopped"
	target.Config["limits.cpu"] = "1"
	target.Config["limits.processes"] = "256"
	f = newFixture(t, []instance{target}, scenario{})
	config = f.config
	config.Limits.Processes = 128
	backend, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(context.Background(), alice, "target"); err == nil || !strings.Contains(err.Error(), "process limit") {
		t.Fatalf("start above process cap error = %v", err)
	}
	for _, call := range f.calls(t) {
		if call[4] != "list" {
			t.Fatalf("over-cap start reached CLI: %v", call)
		}
	}
}

func TestStartingAnActiveInstanceNeedsNoAdmission(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("running", alice.Name)}, scenario{})
	f.backend.config.AvailableMemory = func() (uint64, error) { return 1 << 30, nil }
	if err := f.backend.Start(context.Background(), alice, "running"); err != nil {
		t.Fatalf("already-running start was blocked by admission: %v", err)
	}
	calls := f.calls(t)
	if got := calls[len(calls)-1][4:]; !reflect.DeepEqual(got, []string{"start", "running"}) {
		t.Fatalf("start args = %q", got)
	}
}

func TestExplicitRequestsUpToMaxima(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	config := f.config
	maxCPU := 4
	if cores := runtime.NumCPU(); cores > 0 && cores < maxCPU {
		maxCPU = cores
	}
	config.Limits = core.Limits{MaxInstances: 4, CPU: maxCPU, Memory: "4GiB", Processes: 512}
	config.AvailableMemory = func() (uint64, error) { return 8 << 30, nil }
	backend, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	item, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: "max", CPU: maxCPU, Memory: "4GiB", Processes: 512})
	if err != nil || item.CPU != strconv.Itoa(maxCPU) || item.Memory != "4GiB" || item.Processes != "512" {
		t.Fatalf("maximum create = %#v, %v", item, err)
	}
	for _, options := range []core.CreateOptions{
		{Name: "cpu", CPU: maxCPU + 1},
		{Name: "processes", Processes: 513},
		{Name: "memory", Memory: "4097MiB"},
	} {
		if _, err := backend.Create(context.Background(), alice, options); err == nil {
			t.Fatalf("accepted above maximum %#v", options)
		}
	}
}

func TestProjectMemoryOverflowFailsClosed(t *testing.T) {
	items := []instance{
		{Name: "a", Config: map[string]string{"limits.memory": "9223372036854775807B"}},
		{Name: "b", Config: map[string]string{"limits.memory": "9223372036854775807B"}},
		{Name: "c", Config: map[string]string{"limits.memory": "9223372036854775807B"}},
	}
	if _, err := projectMemory(items, ""); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("overflow error = %v", err)
	}
	if committed, err := projectMemory(items, "a"); err != nil || committed != (^uint64(0)-1) {
		t.Fatalf("excluding a = %d, %v", committed, err)
	}
}

func TestGracefulFlagsNeverForce(t *testing.T) {
	for _, tc := range []struct {
		timeout time.Duration
		want    []string
	}{
		{150 * time.Millisecond, []string{"--force=false", "--timeout=1"}},
		{time.Second, []string{"--force=false", "--timeout=1"}},
		{30 * time.Second, []string{"--force=false", "--timeout=30"}},
	} {
		if got := gracefulFlags(tc.timeout); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("gracefulFlags(%v) = %q, want %q", tc.timeout, got, tc.want)
		}
	}
}

func TestCPUCeilingMatchesRuntime(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	config := f.config
	config.Limits.CPU = 1
	backend, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if got := backend.cpuCeiling(); got != 1 {
		t.Fatalf("single CPU ceiling = %d", got)
	}
	config.Limits.CPU = runtime.NumCPU() + 1000
	backend, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := backend.cpuCeiling(), runtime.NumCPU(); got != want {
		t.Fatalf("CPU ceiling = %d, want runtime cores %d", got, want)
	}
}

func TestProcessLimitsAreReportedFromAppliedConfig(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	created, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "processes", Processes: 128})
	if err != nil || created.Processes != "128" {
		t.Fatalf("create process metadata = %#v, %v", created, err)
	}
	calls := f.calls(t)
	launch := calls[len(calls)-1]
	found := false
	for _, arg := range launch {
		found = found || arg == "limits.processes=128"
	}
	if !found {
		t.Fatalf("launch missing applied process limit: %q", launch)
	}

	item := ownedInstance("processes", alice.Name)
	item.Config["limits.processes"] = "999"
	item.ExpandedConfig = map[string]string{ownerKey: alice.Name, "limits.cpu": "1", "limits.memory": "2GiB", "limits.processes": "128"}
	writeJSON(t, filepath.Join(f.dir, "instances.json"), []instance{item})
	listed, err := f.backend.List(context.Background(), alice)
	if err != nil || len(listed) != 1 || listed[0].Processes != "128" {
		t.Fatalf("listed process metadata = %#v, %v", listed, err)
	}
}

func TestParseMemAvailable(t *testing.T) {
	value, err := parseMemAvailable([]byte("MemTotal:       16384000 kB\nMemAvailable:    6291456 kB\n"))
	if err != nil || value != 6<<30 {
		t.Fatalf("parseMemAvailable = %d, %v", value, err)
	}
	for _, data := range []string{"", "MemFree: 1 kB\n", "MemAvailable: 1234\n", "MemAvailable: 0 kB\n", "MemAvailable: 12 MB\n", "MemAvailable: nope kB\n"} {
		if _, err := parseMemAvailable([]byte(data)); err == nil {
			t.Errorf("accepted %q", data)
		}
	}
}

func TestNoDiskQuotaIsNeverPromised(t *testing.T) {
	item := ownedInstance("existing", "alice")
	item.ExpandedDevices = map[string]map[string]string{"root": {"type": "disk", "path": "/", "size": "10GiB"}}
	f := newFixture(t, []instance{item}, scenario{})
	if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Disk: "1GiB"}); err == nil {
		t.Fatal("accepted disk request without administrator quota")
	}
	items, err := f.backend.List(context.Background(), alice)
	if err != nil || len(items) != 1 || items[0].Disk != "" {
		t.Fatalf("list claimed disk quota: %#v, %v", items, err)
	}
	created, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "new"})
	if err != nil || created.Disk != "" {
		t.Fatalf("create claimed disk quota: %#v, %v", created, err)
	}
	for _, call := range f.calls(t) {
		for _, arg := range call {
			if arg == "--device" || strings.Contains(arg, "root,size=") {
				t.Fatalf("unexpected disk override: %v", call)
			}
		}
	}
}

func TestResourceUnitsAreCanonicalInArgumentsAndSummaries(t *testing.T) {
	for _, defaults := range []bool{false, true} {
		t.Run(strconv.FormatBool(defaults), func(t *testing.T) {
			f := newFixture(t, nil, scenario{})
			config := f.config
			config.Limits.Disk = "10GiB"
			options := core.CreateOptions{Name: "canonical", Memory: "2000000KB", Disk: "9000000KB"}
			if defaults {
				config.Limits.Memory = options.Memory
				config.Limits.Disk = options.Disk
				options.Memory, options.Disk = "", ""
			}
			backend, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			created, err := backend.Create(context.Background(), alice, options)
			if err != nil || created.Memory != "2000000kB" || created.Disk != "9000000kB" {
				t.Fatalf("canonical create = %#v, %v", created, err)
			}
			calls := f.calls(t)
			launch := calls[len(calls)-1]
			memory, disk := false, false
			for _, arg := range launch {
				memory = memory || arg == "limits.memory=2000000kB"
				disk = disk || arg == "root,size=9000000kB"
				if strings.Contains(arg, "KB") || strings.Contains(arg, "limits.memory.swap") {
					t.Fatalf("unsupported Incus 6.0 option: %q", launch)
				}
			}
			if !memory || !disk {
				t.Fatalf("canonical memory/disk missing from argv: %q", launch)
			}
			item := ownedInstance("canonical", alice.Name)
			item.Config["limits.memory"] = "2000000kB"
			item.Devices = map[string]map[string]string{"root": {"type": "disk", "path": "/", "size": "9000000kB"}}
			writeJSON(t, filepath.Join(f.dir, "instances.json"), []instance{item})
			listed, err := backend.List(context.Background(), alice)
			if err != nil || len(listed) != 1 || listed[0].Memory != created.Memory || listed[0].Disk != created.Disk {
				t.Fatalf("listed resources differ from applied resources: %#v, %v", listed, err)
			}
		})
	}
}

func TestAdmitMemoryAccountsForCommitted(t *testing.T) {
	f := newFixture(t, nil, scenario{})
	f.backend.config.AvailableMemory = func() (uint64, error) { return 4 << 30, nil }
	if err := f.backend.admitMemory("2GiB", 2<<30, 0); err != nil {
		t.Fatalf("first request rejected: %v", err)
	}
	if err := f.backend.admitMemory("2GiB", 2<<30, 2<<30); err == nil {
		t.Fatal("accepted overcommitted aggregate")
	}
	if err := f.backend.admitMemory("512MiB", 512<<20, 2<<30); err != nil {
		t.Fatalf("request within remaining budget rejected: %v", err)
	}
	f.backend.config.AvailableMemory = func() (uint64, error) { return 1 << 30, nil }
	if err := f.backend.admitMemory("1B", 1, 0); err == nil {
		t.Fatal("accepted when available equals the reserve")
	}
}

func TestCapCountsOtherOwnersAndUnmanagedInstances(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("foreign", "bob"), ownedInstance("unmanaged", ""), ownedInstance("third", "")}, scenario{})
	if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "new"}); err == nil {
		t.Fatal("ignored unmanaged instances in project capacity")
	}
	calls := f.calls(t)
	if len(calls) != 1 || calls[0][4] != "list" {
		t.Fatalf("capacity failure mutated project: %v", calls)
	}
}

func TestCreateCannotTakeOverExistingName(t *testing.T) {
	f := newFixture(t, []instance{ownedInstance("taken", "bob")}, scenario{})
	if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "taken"}); err == nil {
		t.Fatal("accepted existing instance name")
	}
	calls := f.calls(t)
	if len(calls) != 1 || calls[0][4] != "list" {
		t.Fatalf("name collision mutated project: %v", calls)
	}
}

func TestConcurrentCreatesCannotOverbook(t *testing.T) {
	f := newFixture(t, nil, scenario{LaunchDelay: 100 * time.Millisecond})
	config := f.config
	config.Limits.MaxInstances = 1
	backend, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		wait.Add(1)
		go func(name string) {
			defer wait.Done()
			_, err := backend.Create(context.Background(), alice, core.CreateOptions{Name: name})
			results <- err
		}(name)
	}
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful creates = %d, want 1", successes)
	}
}

func TestMetadataTimeoutAndBoundedErrors(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		f := newFixture(t, nil, scenario{Block: "list"})
		f.backend.config.OperationTimeout = 150 * time.Millisecond
		start := time.Now()
		_, err := f.backend.List(context.Background(), alice)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
			t.Fatalf("timeout = %v after %v", err, time.Since(start))
		}
	})
	t.Run("secret stderr", func(t *testing.T) {
		f := newFixture(t, nil, scenario{Failure: "list", Exit: 1, Stderr: []byte(strings.Repeat("secret\x1b[31m", 10000))})
		_, err := f.backend.List(context.Background(), alice)
		if err == nil || strings.Contains(err.Error(), "secret") || len(err.Error()) > 100 {
			t.Fatalf("unsafe error = %v", err)
		}
	})
	t.Run("large metadata", func(t *testing.T) {
		f := newFixture(t, nil, scenario{LargeOutput: 5 << 20})
		if _, err := f.backend.List(context.Background(), alice); err == nil || !strings.Contains(err.Error(), "output limit") {
			t.Fatalf("oversized output = %v", err)
		}
	})
	t.Run("cancel waiting for create lock", func(t *testing.T) {
		f := newFixture(t, nil, scenario{})
		f.backend.gate <- struct{}{}
		defer func() { <-f.backend.gate }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.backend.Create(ctx, alice, core.CreateOptions{Name: "new"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("lock cancellation = %v", err)
		}
	})
	t.Run("lifecycle timeout", func(t *testing.T) {
		for _, action := range []string{"start", "stop", "restart"} {
			t.Run(action, func(t *testing.T) {
				f := newFixture(t, []instance{ownedInstance("mine", alice.Name)}, scenario{Block: action})
				f.backend.config.OperationTimeout = 150 * time.Millisecond
				var err error
				switch action {
				case "start":
					err = f.backend.Start(context.Background(), alice, "mine")
				case "stop":
					err = f.backend.Stop(context.Background(), alice, "mine")
				case "restart":
					err = f.backend.Restart(context.Background(), alice, "mine")
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("%s timeout = %v", action, err)
				}
			})
		}
	})
	t.Run("cancel waiting for lifecycle lock", func(t *testing.T) {
		f := newFixture(t, []instance{ownedInstance("mine", alice.Name)}, scenario{})
		f.backend.gate <- struct{}{}
		defer func() { <-f.backend.gate }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for name, call := range map[string]func() error{
			"start":   func() error { return f.backend.Start(ctx, alice, "mine") },
			"stop":    func() error { return f.backend.Stop(ctx, alice, "mine") },
			"restart": func() error { return f.backend.Restart(ctx, alice, "mine") },
		} {
			if err := call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s lock cancellation = %v", name, err)
			}
		}
		if calls := f.calls(t); len(calls) != 0 {
			t.Fatalf("lock cancellation reached CLI: %v", calls)
		}
	})
}

func TestSizeParsing(t *testing.T) {
	for input, want := range map[string]uint64{"1B": 1, "2GB": 2000000000, "2GiB": 2 << 30, "512MiB": 512 << 20, "512MB": 512000000, "1TB": 1000000000000, "1TiB": 1 << 40} {
		got, err := parseSize(input)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-1GiB", "0GB", "2", "2GiB\n", "1.5GB", "9223372036854775808B", "9999999999999999999999999999GB"} {
		if _, err := parseSize(input); err == nil {
			t.Errorf("accepted size %q", input)
		}
	}
}

func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return nil
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		// An orphan can remain a zombie briefly until the sandbox's init reaps it.
		if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			if closeName := bytes.LastIndexByte(stat, ')'); closeName >= 0 && strings.HasPrefix(string(stat[closeName+1:]), " Z ") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("process %d is still running", pid)
}
