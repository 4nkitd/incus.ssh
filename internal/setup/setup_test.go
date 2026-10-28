package setup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const fakeIncus = `#!/bin/sh
set -u
state=${FAKE_STATE:?}
if [ -n "${INCUS_PROJECT:-}" ]; then
  echo "fake incus: INCUS_PROJECT leaked into the child environment" >&2
  exit 91
fi
log=$state/log
{
  printf 'CALL'
  for a in "$@"; do printf ' %s' "$(printf '%s' "$a" | base64 | tr -d '\n')"; done
  printf '\n'
} >> "$log"
stdin=$(cat)
if [ -n "$stdin" ]; then
  printf 'STDIN %s\n' "$(printf '%s' "$stdin" | base64 | tr -d '\n')" >> "$log"
fi
last=
prev=
method=GET
data=
for a in "$@"; do
  last=$a
  if [ "$prev" = "-X" ] || [ "$prev" = "--request" ]; then method=$a; fi
  if [ "$prev" = "--data" ] || [ "$prev" = "-d" ]; then data=$a; fi
  prev=$a
done
fail() { cat "$state/$1" >&2; exit 1; }

if [ "$method" != "GET" ] && [ "$method" != "POST" ]; then
  echo "fake incus: unsupported method $method" >&2
  exit 92
fi

if [ "$method" = "POST" ]; then
  case " $* " in
    *" --wait "*) ;;
    *) echo "fake incus: POST requires --wait" >&2; exit 93 ;;
  esac
  case "$last" in
    /1.0/projects)
      case "$data" in
        *'"features.storage.volumes"'*) ;;
        *) data=$(printf '%s' "$data" | sed 's/"config":{/"config":{"features.storage.volumes":"true",/') ;;
      esac
      case "$data" in
        *'"features.storage.buckets"'*) ;;
        *) data=$(printf '%s' "$data" | sed 's/"config":{/"config":{"features.storage.buckets":"true",/') ;;
      esac
      if [ -f "$state/fail.project.create.after" ]; then
        printf '%s' "$data" > "$state/project.json"
        cat "$state/fail.project.create.after" >&2
        exit 1
      fi
      [ -f "$state/fail.project.create" ] && fail fail.project.create
      printf '%s' "$data" > "$state/project.json"
      exit 0 ;;
    "/1.0/profiles?project="*)
      if [ -f "$state/fail.profile.create.after" ]; then
        printf '%s' "$data" > "$state/profile.json"
        cat "$state/fail.profile.create.after" >&2
        exit 1
      fi
      [ -f "$state/fail.profile.create" ] && fail fail.profile.create
      printf '%s' "$data" > "$state/profile.json"
      exit 0 ;;
    *)
      echo "fake incus: unsupported POST $last" >&2
      exit 90 ;;
  esac
fi

case "$last" in
  /1.0)
    [ -f "$state/fail.server" ] && fail fail.server
    cat "$state/server.json"; exit 0 ;;
  /1.0/projects/*)
    [ -f "$state/fail.project.get" ] && fail fail.project.get
    if [ -f "$state/project.json" ]; then cat "$state/project.json"; else echo "Project not found" >&2; exit 1; fi ;;
  /1.0/profiles/*)
    [ -f "$state/fail.profile.get" ] && fail fail.profile.get
    if [ -f "$state/profile.json" ]; then cat "$state/profile.json"; else echo "Profile not found" >&2; exit 1; fi ;;
  /1.0/storage-pools/*)
    if [ -f "$state/pool.json" ]; then cat "$state/pool.json"; else echo "Storage pool not found" >&2; exit 1; fi ;;
  /1.0/networks/*)
    if [ -f "$state/network.json" ]; then cat "$state/network.json"; else echo "Network not found" >&2; exit 1; fi ;;
  /1.0/images/aliases/*)
    if [ -f "$state/alias.json" ]; then cat "$state/alias.json"; else echo "Image alias not found" >&2; exit 1; fi ;;
  /1.0/images/*)
    if [ -f "$state/image.json" ]; then cat "$state/image.json"; else echo "Image not found" >&2; exit 1; fi ;;
  *)
    echo "fake incus: unsupported command: $*" >&2
    exit 90 ;;
esac
`

const validProjectTemplate = `description: Test project
config:
  features.images: "false"
  features.profiles: "true"
  features.networks: "false"
  features.storage.volumes: "true"
  features.storage.buckets: "false"
  limits.containers: "1"
  limits.instances: "1"
  limits.virtual-machines: "0"
  limits.cpu: 2
  limits.memory: 4GiB
  limits.processes: "512"
  restricted: "true"
  restricted.containers.privilege: isolated
  restricted.containers.nesting: block
  restricted.containers.lowlevel: block
  restricted.devices.disk: block
  restricted.devices.nic: managed
  restricted.devices.proxy: block
  restricted.networks.access: incusbr0
`

const validProfileTemplate = `description: Fixed unprivileged development container
config:
  boot.autostart: "false"
  limits.cpu: "1"
  limits.memory: 2GiB
  limits.processes: "256"
  security.idmap.isolated: "true"
  security.nesting: "false"
  security.privileged: "false"
devices:
  eth0:
    name: eth0
    network: incusbr0
    security.mac_filtering: "true"
    type: nic
  root:
    path: /
    pool: devpool
    type: disk
`

type fakeCLI struct {
	opts   options
	state  string
	log    string
	script string
	out    *bytes.Buffer
	errOut *bytes.Buffer
}

func newFake(t *testing.T) *fakeCLI {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "incus")
	if err := os.WriteFile(script, []byte(fakeIncus), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_STATE", state)

	tpl := filepath.Join(dir, "tpl")
	if err := os.Mkdir(tpl, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tpl, "project.yaml"), validProjectTemplate)
	writeFile(t, filepath.Join(tpl, "profile.yaml"), validProfileTemplate)

	writeJSON(t, filepath.Join(state, "server.json"), map[string]any{
		"environment": map[string]any{"server": "incus", "server_version": "6.0.4", "driver": "lxc"},
		"auth":        "trusted",
		"api_status":  "stable",
	})
	writeJSON(t, filepath.Join(state, "pool.json"), map[string]any{"name": "devpool", "driver": "dir", "status": "Created"})
	writeJSON(t, filepath.Join(state, "network.json"), map[string]any{"name": "incusbr0", "type": "bridge", "managed": true, "status": "Created"})
	writeJSON(t, filepath.Join(state, "alias.json"), map[string]any{"name": "ifd-image", "target": testFingerprint})
	writeJSON(t, filepath.Join(state, "image.json"), map[string]any{"fingerprint": testFingerprint, "type": "container"})

	return &fakeCLI{
		opts: options{
			project: "ifd-dev", profile: "ifd", pool: "devpool", network: "incusbr0",
			image: "ifd-image", binary: script, templates: tpl,
		},
		state: state, log: filepath.Join(state, "log"), script: script,
		out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeCLI) writeTemplate(t *testing.T, name, content string) {
	t.Helper()
	writeFile(t, filepath.Join(f.opts.templates, name), content)
}

func (f *fakeCLI) run(t *testing.T, apply bool) error {
	t.Helper()
	f.out.Reset()
	f.errOut.Reset()
	opts := f.opts
	opts.apply = apply
	return run(context.Background(), opts, f.out, f.errOut)
}

func (f *fakeCLI) plans(t *testing.T) (projectPlan, profilePlan) {
	t.Helper()
	project, profile, err := desired(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return project, profile
}

func (f *fakeCLI) seedProject(t *testing.T, mutate func(*projectPlan)) projectPlan {
	t.Helper()
	plan, _ := f.plans(t)
	if mutate != nil {
		mutate(&plan)
	}
	writeJSON(t, filepath.Join(f.state, "project.json"), projectCreate{Name: plan.Name, Description: plan.Description, Config: plan.Config})
	return plan
}

func (f *fakeCLI) seedProfile(t *testing.T, mutate func(*profilePlan)) profilePlan {
	t.Helper()
	_, plan := f.plans(t)
	if mutate != nil {
		mutate(&plan)
	}
	writeJSON(t, filepath.Join(f.state, "profile.json"), profileCreate{Name: plan.Name, Description: plan.Description, Config: plan.Config, Devices: plan.Devices})
	return plan
}

func (f *fakeCLI) fail(t *testing.T, name, message string) {
	t.Helper()
	writeFile(t, filepath.Join(f.state, name), message+"\n")
}

func (f *fakeCLI) clearFail(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.state, name)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func (f *fakeCLI) calls(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "CALL" {
			continue
		}
		args := make([]string, 0, len(fields)-1)
		for _, encoded := range fields[1:] {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("decode call argument: %v", err)
			}
			args = append(args, string(decoded))
		}
		calls = append(calls, args)
	}
	return calls
}

func postPaths(calls [][]string) []string {
	var paths []string
	for _, args := range calls {
		post := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-X" && args[i+1] == "POST" {
				post = true
			}
		}
		if post && len(args) > 0 {
			paths = append(paths, args[len(args)-1])
		}
	}
	return paths
}

func getPaths(calls [][]string) []string {
	var paths []string
	for _, args := range calls {
		if len(args) < 3 {
			continue
		}
		post := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-X" && args[i+1] == "POST" {
				post = true
			}
		}
		if !post {
			paths = append(paths, args[len(args)-1])
		}
	}
	return paths
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func stubRoot(t *testing.T) {
	t.Helper()
	previousEUID, previousCheck := geteuid, checkExecutable
	geteuid = func() int { return 0 }
	checkExecutable = func(string) error { return nil }
	t.Cleanup(func() {
		geteuid, checkExecutable = previousEUID, previousCheck
	})
}

func TestMain(m *testing.M) {
	// The fake Incus CLI lives in a temporary directory, which is not a
	// root-owned trusted path. TestMain models a trusted deployment so root test
	// runs can exercise setup, while trust checks are covered directly.
	if os.Geteuid() == 0 {
		checkExecutable = func(string) error { return nil }
	}
	os.Exit(m.Run())
}

func TestHelpAndArgumentValidation(t *testing.T) {
	if err := Run([]string{"--help"}); err != nil {
		t.Fatalf("--help returned %v", err)
	}
	if err := Run([]string{"unexpected"}); err == nil {
		t.Fatal("positional argument accepted")
	}
}

func TestIncusEnvironmentOverridesRemoved(t *testing.T) {
	t.Setenv("INCUS_PROJECT", "other")
	f := newFake(t)
	if err := f.run(t, false); err != nil {
		t.Fatal(err)
	}
}

func TestPlanOnlyChecksWithoutMutation(t *testing.T) {
	f := newFake(t)
	if err := f.run(t, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "plan only") {
		t.Fatalf("missing plan: %q", f.out.String())
	}
	if !strings.Contains(f.out.String(), "project:  ifd-dev (missing)") {
		t.Fatalf("missing project state: %q", f.out.String())
	}
	calls := f.calls(t)
	if posts := postPaths(calls); len(posts) != 0 {
		t.Fatalf("plan mutated: %v", posts)
	}
	get := getPaths(calls)
	if !containsPath(get, "/1.0") {
		t.Fatalf("daemon was not checked: %v", get)
	}
	if !containsPath(get, "/1.0/images/aliases/ifd-image") {
		t.Fatalf("image alias was not checked: %v", get)
	}
}

func TestApplyCreatesProjectAndProfileAtomically(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
	posts := postPaths(f.calls(t))
	if len(posts) != 2 {
		t.Fatalf("expected two atomic creates, got %v", posts)
	}
	if !containsPath(posts, "/1.0/projects") || !containsPath(posts, "/1.0/profiles?project=ifd-dev") {
		t.Fatalf("unexpected create paths: %v", posts)
	}
	for _, calls := range f.calls(t) {
		for _, arg := range calls {
			if arg == "create" || arg == "edit" {
				t.Fatalf("used mutating subcommand instead of query API: %v", calls)
			}
		}
	}

	project := readProject(t, f)
	if project.Config["restricted"] != "true" || project.Config["features.images"] != "false" {
		t.Fatalf("project body not rendered from template: %+v", project.Config)
	}
	profile := readProfile(t, f)
	if profile.Devices["root"]["pool"] != "devpool" || profile.Devices["eth0"]["network"] != "incusbr0" {
		t.Fatalf("profile devices not rendered: %+v", profile.Devices)
	}
	if _, ok := profile.Devices["eth0"]["name"]; ok {
		t.Fatalf("cosmetic device name was posted: %+v", profile.Devices)
	}
	if !strings.Contains(f.out.String(), "incus preparation complete") {
		t.Fatalf("missing completion: %q", f.out.String())
	}
}

func TestFirstApplyAndRerunWithInjectedServerDefault(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
	project := readProject(t, f)
	if project.Config["features.storage.buckets"] != "false" {
		t.Fatalf("buckets were not sent explicitly: %+v", project.Config)
	}
	if project.Config["features.storage.volumes"] != "true" {
		t.Fatalf("volumes were not sent explicitly: %+v", project.Config)
	}
	if countPath(postPaths(f.calls(t)), "/1.0/projects") != 1 {
		t.Fatalf("first apply did not create exactly one project: %v", postPaths(f.calls(t)))
	}
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
	posts := postPaths(f.calls(t))
	if countPath(posts, "/1.0/projects") != 1 || countPath(posts, "/1.0/profiles?project=ifd-dev") != 1 {
		t.Fatalf("rerun mutated resources: %v", posts)
	}
	if !strings.Contains(f.out.String(), "incus preparation complete") {
		t.Fatalf("rerun did not complete: %q", f.out.String())
	}
}

func TestFakeCLIRejectsUnsafeMethodsAndUnwaitedCreates(t *testing.T) {
	f := newFake(t)
	cases := [][]string{
		{"--force-local", "query", "-X", "PUT", "/1.0/projects/ifd-dev"},
		{"--force-local", "query", "-X", "PATCH", "/1.0/projects/ifd-dev"},
		{"--force-local", "query", "-X", "DELETE", "/1.0/projects/ifd-dev"},
		{"--force-local", "query", "-X", "POST", "--data", "{}", "/1.0/projects"},
	}
	for _, args := range cases {
		cmd := exec.Command(f.script, args...)
		if err := cmd.Run(); err == nil {
			t.Fatalf("fake accepted unsafe command %v", args)
		}
	}
}

func readProject(t *testing.T, f *fakeCLI) projectCreate {
	t.Helper()
	var project projectCreate
	readJSON(t, filepath.Join(f.state, "project.json"), &project)
	return project
}

func readProfile(t *testing.T, f *fakeCLI) profileCreate {
	t.Helper()
	var profile profileCreate
	readJSON(t, filepath.Join(f.state, "profile.json"), &profile)
	return profile
}

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func TestMatchingResourcesAreLeftUntouched(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.seedProject(t, nil)
	f.seedProfile(t, nil)
	beforeProject := readProject(t, f)
	beforeProfile := readProfile(t, f)
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
	if posts := postPaths(f.calls(t)); len(posts) != 0 {
		t.Fatalf("matching resources were rewritten: %v", posts)
	}
	if !strings.Contains(f.out.String(), "already matches") || !strings.Contains(f.out.String(), "incus preparation complete") {
		t.Fatalf("unexpected output: %q", f.out.String())
	}
	if !equalJSON(t, beforeProject, readProject(t, f)) || !equalJSON(t, beforeProfile, readProfile(t, f)) {
		t.Fatal("matching resources changed")
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func TestExistingProjectMismatchIsNeverOverwritten(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.seedProject(t, func(plan *projectPlan) { plan.Config["restricted"] = "false" })
	before := readProject(t, f)
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "does not match") || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), `config "restricted"`) {
		t.Fatalf("mismatch is not precise: %v", err)
	}
	if posts := postPaths(f.calls(t)); len(posts) != 0 {
		t.Fatalf("mismatch triggered a write: %v", posts)
	}
	if !equalJSON(t, before, readProject(t, f)) {
		t.Fatal("existing project changed")
	}
}

func TestExistingProfileMismatchIsNeverOverwritten(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.seedProject(t, nil)
	f.seedProfile(t, func(plan *profilePlan) { plan.Config["security.privileged"] = "true" })
	before := readProfile(t, f)
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "does not match") || !strings.Contains(err.Error(), "security.privileged") {
		t.Fatalf("err=%v", err)
	}
	if posts := postPaths(f.calls(t)); len(posts) != 0 {
		t.Fatalf("mismatch triggered a write: %v", posts)
	}
	if !equalJSON(t, before, readProfile(t, f)) {
		t.Fatal("existing profile changed")
	}
}

func TestRetryAfterPartialProfileFailureReconciles(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.seedProject(t, nil)
	f.fail(t, "fail.profile.create", "quota exceeded")
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "partial state") || strings.Contains(f.out.String(), "incus preparation complete") {
		t.Fatalf("err=%v out=%q", err, f.out.String())
	}
	f.clearFail(t, "fail.profile.create")
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
	posts := postPaths(f.calls(t))
	if containsPath(posts, "/1.0/projects") {
		t.Fatalf("retry recreated the project: %v", posts)
	}
	if !containsPath(posts, "/1.0/profiles?project=ifd-dev") {
		t.Fatalf("retry did not create the profile: %v", posts)
	}
	if !strings.Contains(f.out.String(), "incus preparation complete") {
		t.Fatalf("retry did not complete: %q", f.out.String())
	}
}

func TestAmbiguousProjectCreateNeverClaimsSuccess(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.fail(t, "fail.project.create", "connection reset by peer")
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "may or may not have been created") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(f.out.String(), "incus preparation complete") {
		t.Fatalf("false completion: %q", f.out.String())
	}
}

func TestAmbiguousProjectCreateThatAppliedIsReconciled(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.fail(t, "fail.project.create.after", "connection reset by peer")
	if err := f.run(t, true); err == nil {
		t.Fatal("ambiguous create reported success")
	}
	f.clearFail(t, "fail.project.create.after")
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
	if countPath(postPaths(f.calls(t)), "/1.0/projects") != 1 {
		t.Fatalf("project was recreated: %v", postPaths(f.calls(t)))
	}
	if !strings.Contains(f.out.String(), "incus preparation complete") {
		t.Fatalf("retry did not complete: %q", f.out.String())
	}
}

func TestAmbiguousProfileCreateThatAppliedIsReconciled(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.seedProject(t, nil)
	f.fail(t, "fail.profile.create.after", "connection reset by peer")
	if err := f.run(t, true); err == nil || !strings.Contains(err.Error(), "partial state") {
		t.Fatalf("err=%v", err)
	}
	f.clearFail(t, "fail.profile.create.after")
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
	if countPath(postPaths(f.calls(t)), "/1.0/profiles?project=ifd-dev") != 1 {
		t.Fatalf("profile was recreated: %v", postPaths(f.calls(t)))
	}
	if !strings.Contains(f.out.String(), "incus preparation complete") {
		t.Fatalf("retry did not complete: %q", f.out.String())
	}
}

func countPath(paths []string, want string) int {
	count := 0
	for _, path := range paths {
		if path == want {
			count++
		}
	}
	return count
}

func TestQueryErrorsAreNotReportedAsMissing(t *testing.T) {
	f := newFake(t)
	f.fail(t, "fail.project.get", "Permission denied")
	err := f.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(f.out.String(), "plan only") || strings.Contains(f.out.String(), "project:  ifd-dev (missing)") {
		t.Fatalf("query failure reported as absence: %q", f.out.String())
	}
	if err := f.run(t, false); err == nil {
		t.Fatal("query failure accepted")
	}
}

func TestProfileQueryErrorIsNotMissing(t *testing.T) {
	f := newFake(t)
	f.seedProject(t, nil)
	f.fail(t, "fail.profile.get", "Error: not authorized")
	err := f.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "not authorized") || strings.Contains(err.Error(), "plan only") {
		t.Fatalf("err=%v", err)
	}
}

func TestMalformedQueryJSONFails(t *testing.T) {
	f := newFake(t)
	writeFile(t, filepath.Join(f.state, "project.json"), "{not json")
	err := f.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("err=%v", err)
	}
}

func TestDaemonTrustAndTransportFailures(t *testing.T) {
	t.Run("untrusted", func(t *testing.T) {
		f := newFake(t)
		writeJSON(t, filepath.Join(f.state, "server.json"), map[string]any{"environment": map[string]any{"server": "incus"}, "auth": "untrusted"})
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "does not trust") {
			t.Fatalf("err=%v", err)
		}
		if calls := f.calls(t); len(calls) != 1 {
			t.Fatalf("continued after trust failure: %v", getPaths(calls))
		}
	})
	t.Run("transport", func(t *testing.T) {
		f := newFake(t)
		f.fail(t, "fail.server", "Unable to connect to: /var/lib/incus/unix.socket: no such file or directory")
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "cannot query the Incus daemon") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestCommentsAndPrefixesCannotSatisfyTemplates(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.writeTemplate(t, "project.yaml", `# restricted: "true"
# restricted.networks.access: incusbr0
description: Test project
config:
  features.images: "true"
  features.profiles: "true"
  restricted: "true"
  restricted.containers.privilege: isolated
  restricted.containers.nesting: block
  restricted.containers.lowlevel: block
  restricted.devices.disk: block
  restricted.devices.nic: managed
  restricted.devices.proxy: block
  restricted.networks.access: incusbr0
`)
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "features.images") {
		t.Fatalf("commented field satisfied validation: %v", err)
	}
	if posts := postPaths(f.calls(t)); len(posts) != 0 {
		t.Fatalf("invalid template caused writes: %v", posts)
	}
}

func TestCommentWithWrongNetworkDoesNotSatisfy(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.writeTemplate(t, "project.yaml", strings.Replace(validProjectTemplate,
		"restricted.networks.access: incusbr0",
		"restricted.networks.access: othernet\n  # restricted.networks.access: incusbr0", 1))
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "incusbr0") {
		t.Fatalf("err=%v", err)
	}
}

func TestQuotedValuesAndCommentsAreAccepted(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.writeTemplate(t, "project.yaml", strings.Replace(validProjectTemplate, "limits.cpu: 2\n", "limits.cpu: \"2\"\n", 1)+
		"  # a comment mentioning restricted: \"false\"\n")
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateKeysRejected(t *testing.T) {
	f := newFake(t)
	f.writeTemplate(t, "project.yaml", validProjectTemplate+"  restricted: \"false\"\n")
	_, _, err := desired(f.opts)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err=%v", err)
	}
}

func TestMultipleDocumentsRejected(t *testing.T) {
	f := newFake(t)
	f.writeTemplate(t, "project.yaml", validProjectTemplate+"---\ndescription: second\n")
	_, _, err := desired(f.opts)
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("err=%v", err)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	f := newFake(t)
	f.writeTemplate(t, "project.yaml", validProjectTemplate+"unexpected: value\n")
	_, _, err := desired(f.opts)
	if err == nil || !strings.Contains(err.Error(), "unexpected field") {
		t.Fatalf("err=%v", err)
	}
}

func TestProjectSecuritySettings(t *testing.T) {
	cases := map[string]struct {
		old, new string
		want     string
	}{
		"restricted disabled":      {`restricted: "true"`, `restricted: "false"`, "restricted"},
		"images enabled":           {`features.images: "false"`, `features.images: "true"`, "features.images"},
		"privilege allowed":        {"restricted.containers.privilege: isolated", "restricted.containers.privilege: allow", "restricted.containers.privilege"},
		"nesting allowed":          {"restricted.containers.nesting: block", "restricted.containers.nesting: allow", "restricted.containers.nesting"},
		"lowlevel allowed":         {"restricted.containers.lowlevel: block", "restricted.containers.lowlevel: allow", "restricted.containers.lowlevel"},
		"host disks allowed":       {"restricted.devices.disk: block", "restricted.devices.disk: allow", "restricted.devices.disk"},
		"all nics allowed":         {"restricted.devices.nic: managed", "restricted.devices.nic: all", "restricted.devices.nic"},
		"proxy allowed":            {"restricted.devices.proxy: block", "restricted.devices.proxy: allow", "restricted.devices.proxy"},
		"network mismatch":         {"restricted.networks.access: incusbr0", "restricted.networks.access: othernet", "othernet"},
		"network prefix":           {"restricted.networks.access: incusbr0", "restricted.networks.access: incusbr0-extra", "incusbr0-extra"},
		"profiles disabled":        {`features.profiles: "true"`, `features.profiles: "false"`, "features.profiles"},
		"unsupported feature":      {`features.images: "false"`, "features.images: \"false\"\n  features.buckets: \"true\"", "unsupported"},
		"unsafe custom config":     {`restricted: "true"`, "restricted: \"true\"\n  security.raw: \"true\"", "unsupported"},
		"networks feature enabled": {`features.networks: "false"`, `features.networks: "true"`, "features.networks"},
		"zero container limit":     {`limits.containers: "1"`, `limits.containers: "0"`, "limits.containers"},
		"virtual machines allowed": {`limits.virtual-machines: "0"`, `limits.virtual-machines: "1"`, "limits.virtual-machines"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			if !strings.Contains(validProjectTemplate, tc.old) {
				t.Fatalf("fixture does not contain %q", tc.old)
			}
			f.writeTemplate(t, "project.yaml", strings.Replace(validProjectTemplate, tc.old, tc.new, 1))
			_, _, err := desired(f.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestProjectFeaturePinning(t *testing.T) {
	f := newFake(t)
	project, _ := f.plans(t)
	if project.Config["features.storage.volumes"] != "true" {
		t.Fatalf("volumes not pinned: %q", project.Config["features.storage.volumes"])
	}
	if project.Config["features.storage.buckets"] != "false" {
		t.Fatalf("buckets not pinned: %q", project.Config["features.storage.buckets"])
	}

	cases := []struct {
		name, old, new, want string
	}{
		{"volumes required", "  features.storage.volumes: \"true\"\n", "", "features.storage.volumes"},
		{"buckets required", "  features.storage.buckets: \"false\"\n", "", "features.storage.buckets"},
		{"volumes pinned true", `features.storage.volumes: "true"`, `features.storage.volumes: "false"`, "features.storage.volumes"},
		{"buckets pinned false", `features.storage.buckets: "false"`, `features.storage.buckets: "true"`, "features.storage.buckets"},
		{"zones must be false", `restricted: "true"`, "features.networks.zones: \"true\"\n  restricted: \"true\"", "features.networks.zones"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			if !strings.Contains(validProjectTemplate, tc.old) {
				t.Fatalf("fixture does not contain %q", tc.old)
			}
			f.writeTemplate(t, "project.yaml", strings.Replace(validProjectTemplate, tc.old, tc.new, 1))
			_, _, err := desired(f.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want substring %q", err, tc.want)
			}
		})
	}

	f = newFake(t)
	f.writeTemplate(t, "project.yaml", validProjectTemplate+"  features.networks.zones: \"false\"\n")
	project, _ = f.plans(t)
	if project.Config["features.networks.zones"] != "false" {
		t.Fatalf("explicit zones=false not preserved: %+v", project.Config)
	}
}

func TestMissingClassificationRejectsAuthAndTransportText(t *testing.T) {
	if !isMissing([]byte("Error: Project not found")) {
		t.Fatal("not-found response not classified as missing")
	}
	for _, text := range []string{
		"Error: Permission denied: project not found",
		"Unable to connect to: no such file or directory",
		"Error: Access denied",
	} {
		if isMissing([]byte(text)) {
			t.Errorf("classified %q as missing", text)
		}
	}
}

func TestProfileSecuritySettings(t *testing.T) {
	cases := map[string]struct {
		old, new string
		want     string
	}{
		"privileged":   {`security.privileged: "false"`, `security.privileged: "true"`, "security.privileged"},
		"nesting":      {`security.nesting: "false"`, `security.nesting: "true"`, "security.nesting"},
		"not isolated": {`security.idmap.isolated: "true"`, `security.idmap.isolated: "false"`, "security.idmap.isolated"},
		"autostart":    {`boot.autostart: "false"`, `boot.autostart: "true"`, "boot.autostart"},
		"zero cpu":     {`limits.cpu: "1"`, `limits.cpu: "0"`, "limits.cpu"},
		"zero memory":  {"limits.memory: 2GiB", `limits.memory: "0GiB"`, "limits.memory"},
		"raw config":   {`security.privileged: "false"`, "security.privileged: \"false\"\n  raw.lxc: \"lxc.apparmor.profile=unconfined\"", "raw.lxc"},
		"environment":  {`security.privileged: "false"`, "security.privileged: \"false\"\n  environment.foo: bar", "environment.foo"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			if !strings.Contains(validProfileTemplate, tc.old) {
				t.Fatalf("fixture does not contain %q", tc.old)
			}
			f.writeTemplate(t, "profile.yaml", strings.Replace(validProfileTemplate, tc.old, tc.new, 1))
			_, _, err := desired(f.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestProfileUnsafeDevicesRejected(t *testing.T) {
	cases := map[string]struct {
		mutate func(string) string
		want   string
	}{
		"host path disk": {
			mutate: func(base string) string {
				return strings.Replace(base, "  root:\n    path: /", "  root:\n    source: /etc\n    path: /", 1)
			},
			want: "source",
		},
		"unmanaged nic": {
			mutate: func(base string) string {
				return strings.Replace(base, "    network: incusbr0\n", "    network: incusbr0\n    nictype: macvlan\n", 1)
			},
			want: "nictype",
		},
		"host nic parent": {
			mutate: func(base string) string {
				return strings.Replace(base, "    network: incusbr0\n", "    network: incusbr0\n    parent: eth0\n", 1)
			},
			want: "parent",
		},
		"proxy device": {
			mutate: func(base string) string {
				return base + "  proxy0:\n    type: proxy\n    listen: tcp:127.0.0.1:8080\n    connect: tcp:127.0.0.1:80\n"
			},
			want: "type",
		},
		"unix device": {
			mutate: func(base string) string {
				return base + "  dev:\n    type: unix-char\n    source: /dev/kvm\n    path: /dev/kvm\n"
			},
			want: "type",
		},
		"second disk": {
			mutate: func(base string) string {
				return base + "  data:\n    type: disk\n    path: /data\n    pool: devpool\n"
			},
			want: "path /",
		},
		"mac filtering disabled": {
			mutate: func(base string) string {
				return strings.Replace(base, `security.mac_filtering: "true"`, `security.mac_filtering: "false"`, 1)
			},
			want: "mac_filtering",
		},
		"pool prefix": {
			mutate: func(base string) string {
				return strings.Replace(base, "pool: devpool", "pool: devpool-extra", 1)
			},
			want: "uses pool",
		},
		"network prefix": {
			mutate: func(base string) string {
				return strings.Replace(base, "network: incusbr0", "network: incusbr0-extra", 1)
			},
			want: "uses network",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.writeTemplate(t, "profile.yaml", tc.mutate(validProfileTemplate))
			_, _, err := desired(f.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestCustomPoolAndNetworkOverrideMustMatchTemplate(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.opts.pool = "pool-b"
	f.opts.network = "net-b"
	writeJSON(t, filepath.Join(f.state, "pool.json"), map[string]any{"name": "pool-b", "driver": "dir"})
	writeJSON(t, filepath.Join(f.state, "network.json"), map[string]any{"name": "net-b", "type": "bridge", "managed": true})
	f.writeTemplate(t, "project.yaml", strings.Replace(validProjectTemplate, "restricted.networks.access: incusbr0", "restricted.networks.access: net-b", 1))
	f.writeTemplate(t, "profile.yaml", strings.NewReplacer("pool: devpool", "pool: pool-b", "network: incusbr0", "network: net-b").Replace(validProfileTemplate))
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
	project := readProject(t, f)
	profile := readProfile(t, f)
	if project.Config["restricted.networks.access"] != "net-b" {
		t.Fatalf("project config: %+v", project.Config)
	}
	if profile.Devices["root"]["pool"] != "pool-b" || profile.Devices["eth0"]["network"] != "net-b" {
		t.Fatalf("profile devices: %+v", profile.Devices)
	}

	f2 := newFake(t)
	f2.opts.pool = "pool-b"
	writeJSON(t, filepath.Join(f2.state, "pool.json"), map[string]any{"name": "pool-b", "driver": "dir"})
	if err := f2.run(t, true); err == nil || !strings.Contains(err.Error(), "uses pool") {
		t.Fatalf("err=%v", err)
	}
	if posts := postPaths(f2.calls(t)); len(posts) != 0 {
		t.Fatalf("mismatched pool caused writes: %v", posts)
	}
}

func TestImageReferenceForms(t *testing.T) {
	t.Run("local prefix", func(t *testing.T) {
		f := newFake(t)
		f.opts.image = "local:ifd-image"
		if err := f.run(t, false); err != nil {
			t.Fatal(err)
		}
		if !containsPath(getPaths(f.calls(t)), "/1.0/images/aliases/ifd-image") {
			t.Fatalf("alias not resolved: %v", getPaths(f.calls(t)))
		}
	})
	t.Run("fingerprint", func(t *testing.T) {
		f := newFake(t)
		f.opts.image = testFingerprint
		if err := f.run(t, false); err != nil {
			t.Fatal(err)
		}
		if !containsPath(getPaths(f.calls(t)), "/1.0/images/"+testFingerprint) {
			t.Fatalf("image not resolved by fingerprint: %v", getPaths(f.calls(t)))
		}
	})
	t.Run("remote rejected", func(t *testing.T) {
		f := newFake(t)
		f.opts.image = "images:ubuntu/24.04"
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "remote image") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("missing alias", func(t *testing.T) {
		f := newFake(t)
		if err := os.Remove(filepath.Join(f.state, "alias.json")); err != nil {
			t.Fatal(err)
		}
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "not cached locally") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("alias target mismatch", func(t *testing.T) {
		f := newFake(t)
		writeJSON(t, filepath.Join(f.state, "alias.json"), map[string]any{"name": "ifd-image", "target": strings.Repeat("b", 64)})
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("virtual machine image rejected", func(t *testing.T) {
		f := newFake(t)
		writeJSON(t, filepath.Join(f.state, "image.json"), map[string]any{"fingerprint": testFingerprint, "type": "virtual-machine"})
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "not a container image") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestStrictNames(t *testing.T) {
	invalid := []string{"", "default", "ifd-", "-ifd", "IFD", "ifd_dev", strings.Repeat("a", 49), "1ifd"}
	for _, value := range invalid {
		for _, field := range []string{"project", "profile", "pool", "network"} {
			if value == "default" && (field == "pool" || field == "network") {
				continue
			}
			f := newFake(t)
			switch field {
			case "project":
				f.opts.project = value
			case "profile":
				f.opts.profile = value
			case "pool":
				f.opts.pool = value
			case "network":
				f.opts.network = value
			}
			if err := f.run(t, false); err == nil {
				t.Errorf("accepted %s %q", field, value)
			}
		}
	}
	f := newFake(t)
	f.opts.project = strings.Repeat("a", 48)
	f.opts.templates = filepath.Join(t.TempDir(), "missing")
	if err := f.run(t, false); err != nil {
		t.Fatalf("48-character slug rejected: %v", err)
	}
}

func TestApplyRequiresRootBeforeProbes(t *testing.T) {
	previous := geteuid
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { geteuid = previous })
	f := newFake(t)
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("err=%v", err)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatalf("probes ran without root: %v", calls)
	}
}

func TestApplyRequiresTrustedExecutableBeforeProbes(t *testing.T) {
	stubRoot(t)
	checkExecutable = func(string) error { return os.ErrPermission }
	f := newFake(t)
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "untrusted Incus executable") {
		t.Fatalf("err=%v", err)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatalf("probes ran with an untrusted executable: %v", calls)
	}
}

func TestRootPlanRequiresTrustedExecutableBeforeProbes(t *testing.T) {
	stubRoot(t)
	checkExecutable = func(string) error { return os.ErrPermission }
	f := newFake(t)
	err := f.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "untrusted Incus executable") {
		t.Fatalf("err=%v", err)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatalf("probes ran with an untrusted executable: %v", calls)
	}
}

func TestTrustedExecutableRejectsSymlinksAndWritableFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-incus")
	writeFile(t, target, "#!/bin/sh\nexit 1\n")
	link := filepath.Join(dir, "linked-incus")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := trustedIncusExecutable(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
	writable := filepath.Join(dir, "writable-incus")
	writeFile(t, writable, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := trustedIncusExecutable(writable); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("err=%v", err)
	}
	if err := trustedIncusExecutable(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing executable accepted")
	}
}

func TestMissingPoolAndNetworkFailClosed(t *testing.T) {
	t.Run("unmanaged bridge", func(t *testing.T) {
		stubRoot(t)
		f := newFake(t)
		writeJSON(t, filepath.Join(f.state, "network.json"), map[string]any{"name": "incusbr0", "type": "bridge", "managed": false})
		err := f.run(t, true)
		if err == nil || !strings.Contains(err.Error(), "not managed") {
			t.Fatalf("err=%v", err)
		}
		if got := postPaths(f.calls(t)); len(got) != 0 {
			t.Fatalf("unmanaged bridge caused writes: %v", got)
		}
	})
	t.Run("pool", func(t *testing.T) {
		f := newFake(t)
		if err := os.Remove(filepath.Join(f.state, "pool.json")); err != nil {
			t.Fatal(err)
		}
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "storage pool") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("network", func(t *testing.T) {
		f := newFake(t)
		if err := os.Remove(filepath.Join(f.state, "network.json")); err != nil {
			t.Fatal(err)
		}
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "network") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("unmanaged network", func(t *testing.T) {
		f := newFake(t)
		writeJSON(t, filepath.Join(f.state, "network.json"), map[string]any{"name": "incusbr0", "type": "macvlan"})
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "managed bridge") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("ovn network", func(t *testing.T) {
		f := newFake(t)
		writeJSON(t, filepath.Join(f.state, "network.json"), map[string]any{"name": "incusbr0", "type": "ovn"})
		err := f.run(t, false)
		if err == nil || !strings.Contains(err.Error(), "OVN is not supported") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestOVNRejectedBeforeAnyWrite(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	writeJSON(t, filepath.Join(f.state, "network.json"), map[string]any{"name": "incusbr0", "type": "ovn"})
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "OVN is not supported") {
		t.Fatalf("err=%v", err)
	}
	if posts := postPaths(f.calls(t)); len(posts) != 0 {
		t.Fatalf("OVN rejection wrote resources: %v", posts)
	}
}

func TestPlanDoesNotRequireTemplates(t *testing.T) {
	f := newFake(t)
	f.opts.templates = filepath.Join(t.TempDir(), "missing")
	if err := f.run(t, false); err != nil {
		t.Fatal(err)
	}
}

func TestApplyPrevalidatesBeforeAnyWrite(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.opts.templates = filepath.Join(t.TempDir(), "missing")
	err := f.run(t, true)
	if err == nil || !strings.Contains(err.Error(), "project template") {
		t.Fatalf("err=%v", err)
	}
	if posts := postPaths(f.calls(t)); len(posts) != 0 {
		t.Fatalf("apply wrote before template validation: %v", posts)
	}
}

func TestImageFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"image":"local:ifd-image","listen":"127.0.0.1:2222"}`)
	image, err := imageFromConfig(path)
	if err != nil || image != "local:ifd-image" {
		t.Fatalf("image=%q err=%v", image, err)
	}
	if _, err := imageFromConfig(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing config accepted")
	}
	writeFile(t, filepath.Join(dir, "empty.json"), `{}`)
	if _, err := imageFromConfig(filepath.Join(dir, "empty.json")); err == nil {
		t.Fatal("config without image accepted")
	}
	overflow := filepath.Join(dir, "large.json")
	writeFile(t, overflow, strings.Repeat("a", maxFileBytes+1))
	if _, err := imageFromConfig(overflow); err == nil {
		t.Fatal("oversized config accepted")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := imageFromConfig(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked config err=%v", err)
	}
}

func TestConfigImageOverride(t *testing.T) {
	f := newFake(t)
	configPath := filepath.Join(f.state, "config.json")
	writeJSON(t, configPath, map[string]any{"image": "local:ifd-image"})
	f.opts.config = configPath
	f.opts.image = "images:ubuntu/24.04"
	if err := f.run(t, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "image:    local:ifd-image") {
		t.Fatalf("config image not used: %q", f.out.String())
	}
}

func TestSafeReadRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	writeFile(t, good, "ok")
	if data, err := safeRead(good, "test file"); err != nil || string(data) != "ok" {
		t.Fatalf("data=%q err=%v", data, err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := safeRead(link, "test file"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink err=%v", err)
	}

	writable := filepath.Join(dir, "writable")
	writeFile(t, writable, "ok")
	if err := os.Chmod(writable, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := safeRead(writable, "test file"); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("writable err=%v", err)
	}

	large := filepath.Join(dir, "large")
	writeFile(t, large, strings.Repeat("a", maxFileBytes+1))
	if _, err := safeRead(large, "test file"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("large err=%v", err)
	}

	empty := filepath.Join(dir, "empty")
	writeFile(t, empty, "")
	if _, err := safeRead(empty, "test file"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty err=%v", err)
	}

	if _, err := safeRead(dir, "test file"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory err=%v", err)
	}
}

func TestTemplatesRejectUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := templates(dir); err == nil {
		t.Fatal("missing templates accepted")
	}
	writeFile(t, filepath.Join(dir, "project.yaml"), strings.Repeat("a", maxFileBytes+1))
	writeFile(t, filepath.Join(dir, "profile.yaml"), "x")
	if _, _, err := templates(dir); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized template err=%v", err)
	}

	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir2, "target.yaml"), "x")
	if err := os.Symlink(filepath.Join(dir2, "target.yaml"), filepath.Join(dir2, "project.yaml")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir2, "profile.yaml"), "x")
	if _, _, err := templates(dir2); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked template err=%v", err)
	}
}

func TestExistingProfileDeviceNameAnnotationIsIgnored(t *testing.T) {
	stubRoot(t)
	f := newFake(t)
	f.seedProject(t, nil)
	profile := f.seedProfile(t, nil)
	profile.Devices["eth0"]["name"] = "eth0"
	profile.Devices["root"]["name"] = "root"
	writeJSON(t, filepath.Join(f.state, "profile.json"), profileCreate{Name: profile.Name, Description: profile.Description, Config: profile.Config, Devices: profile.Devices})
	if err := f.run(t, true); err != nil {
		t.Fatal(err)
	}
	if posts := postPaths(f.calls(t)); len(posts) != 0 {
		t.Fatalf("cosmetic device name caused a rewrite: %v", posts)
	}
}
