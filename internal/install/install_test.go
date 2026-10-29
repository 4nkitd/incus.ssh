package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	ubuntuRelease = "PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n"
	debian13      = "PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\nVERSION_ID=\"13\"\n"
	debian12      = "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\nVERSION_ID=\"12\"\n"

	serverMetadataJSON = `{"environment":{"server":"incus","server_version":"6.0.0"},"auth":"trusted"}`
)

type commandCall struct {
	name string
	args []string
	env  []string
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   []commandCall
	handler func(context.Context, commandCall, io.Writer, io.Writer) error
}

func (f *fakeRunner) run(ctx context.Context, name string, args, env []string, stdout, stderr io.Writer) error {
	call := commandCall{name: name, args: append([]string(nil), args...), env: append([]string(nil), env...)}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	handler := f.handler
	f.mu.Unlock()
	if handler != nil {
		return handler(ctx, call, stdout, stderr)
	}
	return nil
}

func (f *fakeRunner) recorded() []commandCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]commandCall(nil), f.calls...)
}

type hostStub struct {
	runner  *fakeRunner
	trusted map[string]error
	reads   int
}

func newHost(t *testing.T, osRelease string, root, client bool, daemonPath string) *hostStub {
	t.Helper()
	stub := &hostStub{
		runner:  &fakeRunner{},
		trusted: map[string]error{incusBinary: nil, aptGet: nil},
	}
	if daemonPath != "" {
		stub.trusted[daemonPath] = nil
	}
	previousExec, previousRoot, previousRead := execute, geteuid, readFile
	previousTrust, previousStat := checkExecutable, statPath
	t.Cleanup(func() {
		execute, geteuid, readFile = previousExec, previousRoot, previousRead
		checkExecutable, statPath = previousTrust, previousStat
	})

	execute = stub.runner.run
	geteuid = func() int {
		if root {
			return 0
		}
		return 1000
	}
	readFile = func(path string) ([]byte, error) {
		if path != osReleasePath {
			return nil, fs.ErrNotExist
		}
		stub.reads++
		return []byte(osRelease), nil
	}
	checkExecutable = func(path string) error {
		if err, ok := stub.trusted[path]; ok {
			return err
		}
		return fs.ErrNotExist
	}
	statPath = func(path string) (fs.FileInfo, error) {
		if (path == incusBinary && client) || (daemonPath != "" && path == daemonPath) {
			return fakeFileInfo{name: "binary"}, nil
		}
		return nil, fs.ErrNotExist
	}
	return stub
}

type fakeFileInfo struct {
	name string
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o755 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func TestPlanMakesNoChanges(t *testing.T) {
	stub := newHost(t, ubuntuRelease, false, false, "")
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{}, &out, &errOut); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 0 {
		t.Fatalf("plan started commands: %v", calls)
	}
	plan := out.String()
	for _, want := range []string{
		"incus install plan",
		aptGet + " " + strings.Join(updateArgs(), " "),
		aptGet + " " + strings.Join(simulateArgs(), " "),
		aptGet + " " + strings.Join(installArgs(), " "),
		"--no-remove",
		"ca-certificates",
		"dnsmasq-base",
		"full simulation output is bounded and checked for removals",
		"no files were written and no command was run",
		"initialize Incus",
		"setup --apply",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
}

func TestPlanForCompleteInstallationIsReadOnly(t *testing.T) {
	stub := newHost(t, ubuntuRelease, false, true, "/usr/sbin/incusd")
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{}, &out, &errOut); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 0 {
		t.Fatalf("plan ran commands: %v", calls)
	}
	output := out.String()
	if !strings.Contains(output, "query /1.0") {
		t.Fatalf("plan did not describe the read-only daemon query:\n%s", output)
	}
	if strings.Contains(output, aptGet+" update") {
		t.Fatalf("existing plan promised apt updates:\n%s", output)
	}
}

func TestCompleteInstallationOnOtherDistributionDoesNotInstall(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(fmt.Sprintf("apply=%t", apply), func(t *testing.T) {
			stub := newHost(t, "ID=alpine\nVERSION_ID=3.20\n", true, true, "/usr/sbin/incusd")
			stub.runner.handler = func(_ context.Context, call commandCall, stdout, _ io.Writer) error {
				if call.name != incusBinary || !sameArgs(call.args, queryArgs()) {
					t.Fatalf("unexpected command: %+v", call)
				}
				fmt.Fprint(stdout, serverMetadataJSON)
				return nil
			}
			var out, errOut bytes.Buffer
			if err := Run(context.Background(), Options{Apply: apply}, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			wantCalls := 0
			if apply {
				wantCalls = 1
			}
			if calls := stub.runner.recorded(); len(calls) != wantCalls {
				t.Fatalf("commands = %v, want %d read-only queries", calls, wantCalls)
			}
		})
	}
}

func TestPlanClientOnlySchedulesInstall(t *testing.T) {
	stub := newHost(t, ubuntuRelease, false, true, "")
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{}, &out, &errOut); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 0 {
		t.Fatalf("plan ran commands: %v", calls)
	}
	output := out.String()
	if !strings.Contains(output, "client binary only") {
		t.Fatalf("client-only state missing:\n%s", output)
	}
	if !strings.Contains(output, strings.Join(installArgs(), " ")) {
		t.Fatalf("client-only plan did not schedule installation:\n%s", output)
	}
}

func TestApplyRequiresRootBeforeReadsOrCommands(t *testing.T) {
	stub := newHost(t, ubuntuRelease, false, false, "")
	var out, errOut bytes.Buffer
	err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("err=%v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 0 {
		t.Fatalf("commands ran without root: %v", calls)
	}
	if stub.reads != 0 {
		t.Fatalf("OS metadata was read before the root check: %d", stub.reads)
	}
}

func TestExistingCompleteInstallationSkipsApt(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, true, "/usr/sbin/incusd")
	stub.runner.handler = func(_ context.Context, call commandCall, stdout, _ io.Writer) error {
		if call.name != incusBinary || !sameArgs(call.args, queryArgs()) {
			return fmt.Errorf("unexpected command %s %v", call.name, call.args)
		}
		fmt.Fprint(stdout, serverMetadataJSON)
		return nil
	}
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{Apply: true}, &out, &errOut); err != nil {
		t.Fatalf("existing install: %v", err)
	}
	calls := stub.runner.recorded()
	if len(calls) != 1 {
		t.Fatalf("calls=%v", calls)
	}
	if calls[0].name != incusBinary || !sameArgs(calls[0].args, queryArgs()) {
		t.Fatalf("unexpected probe: %v", calls[0])
	}
	if hasEnv(calls[0].env, "DEBIAN_FRONTEND", "noninteractive") {
		t.Fatal("daemon query inherited the apt non-interactive environment")
	}
	output := out.String()
	for _, want := range []string{"daemon:   reachable", "incus 6.0.0", "no apt or global package update was run"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
}

func TestExistingEnvelopeServerMetadataSkipsApt(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, true, "/usr/libexec/incus/incusd")
	stub.runner.handler = func(_ context.Context, _ commandCall, stdout, _ io.Writer) error {
		fmt.Fprint(stdout, `{"type":"sync","status":"Success","metadata":{"environment":{"server":"incus","server_version":"6.0.0"},"auth":"trusted"}}`)
		return nil
	}
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{Apply: true}, &out, &errOut); err != nil {
		t.Fatalf("envelope metadata: %v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 1 || calls[0].name != incusBinary {
		t.Fatalf("unexpected calls: %v", calls)
	}
	if !strings.Contains(out.String(), "incus 6.0.0") {
		t.Fatalf("output=%s", out.String())
	}
}

func TestExistingDistroDaemonPathSkipsApt(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, true, "/usr/libexec/incus/incusd")
	stub.runner.handler = func(_ context.Context, _ commandCall, stdout, _ io.Writer) error {
		fmt.Fprint(stdout, serverMetadataJSON)
		return nil
	}
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{Apply: true}, &out, &errOut); err != nil {
		t.Fatalf("distro daemon path: %v", err)
	}
	calls := stub.runner.recorded()
	if len(calls) != 1 || calls[0].name != incusBinary {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestExistingClientOnlyFallsThroughToInstall(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, true, "")
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{Apply: true}, &out, &errOut); err != nil {
		t.Fatalf("client-only apply: %v", err)
	}
	calls := stub.runner.recorded()
	if len(calls) != 3 {
		t.Fatalf("calls=%v", calls)
	}
	for _, call := range calls {
		if call.name != aptGet {
			t.Fatalf("client-only did not install packages: %v", calls)
		}
	}
	if !strings.Contains(out.String(), "incus packages installed") {
		t.Fatalf("output=%s", out.String())
	}
}

func TestExistingDaemonOnlyFallsThroughToInstall(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, false, "/usr/bin/incusd")
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{Apply: true}, &out, &errOut); err != nil {
		t.Fatalf("daemon-only apply: %v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 3 {
		t.Fatalf("daemon-only did not install packages: %v", calls)
	}
}

func TestExistingUnreachableDaemonIsNotANoop(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, true, "/usr/sbin/incusd")
	stub.runner.handler = func(_ context.Context, _ commandCall, _, stderr io.Writer) error {
		fmt.Fprint(stderr, "Error: Get "+"http://unix.socket/1.0: dial unix: connection refused")
		return errors.New("exit status 1")
	}
	var out, errOut bytes.Buffer
	err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
	if err == nil {
		t.Fatal("unreachable daemon was reported as a no-op")
	}
	for _, want := range []string{"daemon may be stopped", "No apt command was run", "reinstalling is not the fix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if calls := stub.runner.recorded(); len(calls) != 1 || calls[0].name != incusBinary {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestExistingMalformedServerMetadataIsNotReady(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{"empty", "", "decode server metadata"},
		{"no metadata", "{}", "environment or version"},
		{"missing version", `{"environment":{"server":"incus"},"auth":"trusted"}`, "environment or version"},
		{"untrusted", `{"environment":{"server":"incus","server_version":"6.0.0"},"auth":"untrusted"}`, "not trusted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := newHost(t, ubuntuRelease, true, true, "/usr/bin/incusd")
			stub.runner.handler = func(_ context.Context, _ commandCall, stdout, _ io.Writer) error {
				fmt.Fprint(stdout, test.output)
				return nil
			}
			var out, errOut bytes.Buffer
			err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v", err)
			}
			if calls := stub.runner.recorded(); len(calls) != 1 {
				t.Fatalf("unexpected calls: %v", calls)
			}
		})
	}
}

func TestUntrustedExistingBinariesFailClosed(t *testing.T) {
	t.Run("client", func(t *testing.T) {
		stub := newHost(t, ubuntuRelease, true, true, "/usr/sbin/incusd")
		stub.trusted[incusBinary] = errors.New("path must not be writable by group or others")
		var out, errOut bytes.Buffer
		err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "never replaces") {
			t.Fatalf("err=%v", err)
		}
		if calls := stub.runner.recorded(); len(calls) != 0 {
			t.Fatalf("untrusted client was invoked: %v", calls)
		}
	})
	t.Run("daemon", func(t *testing.T) {
		stub := newHost(t, ubuntuRelease, true, true, "/usr/sbin/incusd")
		stub.trusted["/usr/sbin/incusd"] = errors.New("path must not be a symlink")
		var out, errOut bytes.Buffer
		err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "symlink") || !strings.Contains(err.Error(), "never replaces") {
			t.Fatalf("err=%v", err)
		}
		if calls := stub.runner.recorded(); len(calls) != 0 {
			t.Fatalf("untrusted daemon was invoked: %v", calls)
		}
	})
}

func TestUnsupportedOSFailsBeforeCommands(t *testing.T) {
	stub := newHost(t, "PRETTY_NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.20\n", false, false, "")
	var out, errOut bytes.Buffer
	err := Run(context.Background(), Options{}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "unsupported operating system") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "Ubuntu 24.04") || !strings.Contains(err.Error(), "No repositories were added") {
		t.Fatalf("err lacks instructions: %v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 0 {
		t.Fatalf("unsupported OS started commands: %v", calls)
	}
}

func TestMissingOrUntrustedAptFailsBeforeExec(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		stub := newHost(t, ubuntuRelease, true, false, "")
		stub.trusted[aptGet] = fs.ErrNotExist
		var out, errOut bytes.Buffer
		err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), aptGet) {
			t.Fatalf("err=%v", err)
		}
		if calls := stub.runner.recorded(); len(calls) != 0 {
			t.Fatalf("missing apt still ran commands: %v", calls)
		}
	})
	t.Run("untrusted", func(t *testing.T) {
		stub := newHost(t, ubuntuRelease, true, false, "")
		stub.trusted[aptGet] = errors.New("path must not be writable by group or others")
		var out, errOut bytes.Buffer
		err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "untrusted") {
			t.Fatalf("err=%v", err)
		}
		if calls := stub.runner.recorded(); len(calls) != 0 {
			t.Fatalf("untrusted apt was invoked: %v", calls)
		}
	})
}

func TestDebianWithoutCandidateStopsBeforeInstall(t *testing.T) {
	stub := newHost(t, debian13, true, false, "")
	stub.runner.handler = func(_ context.Context, call commandCall, _, stderr io.Writer) error {
		switch {
		case sameArgs(call.args, updateArgs()):
			return nil
		case sameArgs(call.args, simulateArgs()):
			fmt.Fprint(stderr, "E: Package 'incus' has no installation candidate\n")
			return errors.New("exit status 100")
		default:
			return fmt.Errorf("unexpected command %s %v", call.name, call.args)
		}
	}
	var out, errOut bytes.Buffer
	err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "no packages were installed") || !strings.Contains(err.Error(), "does not add repositories") {
		t.Fatalf("err lacks partial-state guidance: %v", err)
	}
	calls := stub.runner.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls=%v", calls)
	}
	if sameArgs(calls[1].args, installArgs()) {
		t.Fatalf("install ran without a candidate: %v", calls)
	}
}

func TestUnrelatedMissingDependencyIsNotCalledUnsupported(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, false, "")
	stub.runner.handler = func(_ context.Context, call commandCall, _, stderr io.Writer) error {
		if sameArgs(call.args, simulateArgs()) {
			fmt.Fprint(stderr, "E: Package 'libfoo' has no installation candidate\n")
			return errors.New("exit status 100")
		}
		return nil
	}
	var out, errOut bytes.Buffer
	err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "package preflight failed") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), "are not available from the configured") {
		t.Fatalf("unrelated dependency was mislabeled: %v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 2 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestRequestedPackageArgumentsExact(t *testing.T) {
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "simulate",
			got:  simulateArgs(),
			want: []string{"install", "--simulate", "--no-remove", "--no-install-recommends", "--yes", "incus", "incus-client", "ca-certificates", "dnsmasq-base"},
		},
		{
			name: "install",
			got:  installArgs(),
			want: []string{"install", "--no-remove", "--no-install-recommends", "--yes", "incus", "incus-client", "ca-certificates", "dnsmasq-base"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !sameArgs(test.got, test.want) {
				t.Fatalf("args=%v want=%v", test.got, test.want)
			}
		})
	}
}

func TestMissingRequestedPackageDetection(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"E: Package 'incus' has no installation candidate", true},
		{"E: Package 'incus-client' has no installation candidate", true},
		{"E: Package 'ca-certificates' has no installation candidate", true},
		{"E: Package 'dnsmasq-base' has no installation candidate", true},
		{"E: Unable to locate package ca-certificates", true},
		{"E: Unable to locate package dnsmasq-base", true},
		{"E: Package 'libfoo' has no installation candidate", false},
		{"E: Package 'incus-extra' has no installation candidate", false},
	}
	for _, test := range tests {
		if got := missingRequestedPackage(test.line); got != test.want {
			t.Errorf("%q: got %v want %v", test.line, got, test.want)
		}
	}
}

func TestDebianPlanDoesNotPromiseCandidate(t *testing.T) {
	stub := newHost(t, debian12, false, false, "")
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{}, &out, &errOut); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(out.String(), "may not provide incus") {
		t.Fatalf("Debian 12 caveat missing:\n%s", out.String())
	}
	if calls := stub.runner.recorded(); len(calls) != 0 {
		t.Fatalf("plan ran commands: %v", calls)
	}
}

func TestApplyUsesExactWhitelistAndSanitizedEnv(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, false, "")
	t.Setenv("APT_CONFIG", "/tmp/apt.conf")
	t.Setenv("LD_PRELOAD", "/tmp/loader.so")
	t.Setenv("DYLD_INSERT_LIBRARIES", "/tmp/loader.dylib")
	t.Setenv("http_proxy", "http://proxy.invalid:8080")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:8080")
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{Apply: true}, &out, &errOut); err != nil {
		t.Fatalf("apply: %v", err)
	}
	calls := stub.runner.recorded()
	want := [][]string{updateArgs(), simulateArgs(), installArgs()}
	if len(calls) != len(want) {
		t.Fatalf("calls=%v", calls)
	}
	for index, call := range calls {
		if call.name != aptGet {
			t.Errorf("call %d used %q", index, call.name)
		}
		if !sameArgs(call.args, want[index]) {
			t.Errorf("call %d args=%v want=%v", index, call.args, want[index])
		}
		if !hasEnv(call.env, "DEBIAN_FRONTEND", "noninteractive") {
			t.Errorf("call %d did not force non-interactive apt", index)
		}
		for _, key := range []string{"APT_CONFIG", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "http_proxy", "HTTPS_PROXY"} {
			if envKeyPresent(call.env, key) {
				t.Errorf("call %d inherited %s", index, key)
			}
		}
	}
	for _, call := range calls {
		joined := strings.ToLower(call.name + " " + strings.Join(call.args, " "))
		for _, banned := range []string{"upgrade", "dist-upgrade", "purge", "snap", "brew", "docker", "lxc", "curl", "sh -c", "/bin/sh"} {
			if strings.Contains(joined, banned) {
				t.Errorf("unexpected package action %q in %q", banned, joined)
			}
		}
	}
	if !strings.Contains(out.String(), "incus packages installed") {
		t.Fatalf("output=%s", out.String())
	}
}

func TestSimulationWithRemovalsAbortsBeforeInstall(t *testing.T) {
	for _, line := range []string{"Remv libfoo [1.0]", "REMOVED libfoo"} {
		t.Run(line, func(t *testing.T) {
			stub := newHost(t, ubuntuRelease, true, false, "")
			stub.runner.handler = func(_ context.Context, call commandCall, stdout, _ io.Writer) error {
				if sameArgs(call.args, simulateArgs()) {
					fmt.Fprintln(stdout, line)
				}
				return nil
			}
			var out, errOut bytes.Buffer
			err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
			if err == nil || !strings.Contains(err.Error(), "removals") || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("err=%v", err)
			}
			if calls := stub.runner.recorded(); len(calls) != 2 {
				t.Fatalf("install ran despite a removal plan: %v", calls)
			}
		})
	}
}

func TestOversizedSimulationFailsClosed(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, false, "")
	stub.runner.handler = func(_ context.Context, call commandCall, stdout, _ io.Writer) error {
		if sameArgs(call.args, simulateArgs()) {
			fmt.Fprint(stdout, strings.Repeat("Inst dependency\n", maxSimulationOutput/9+32))
		}
		return nil
	}
	var out, errOut bytes.Buffer
	err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "exceeded") || !strings.Contains(err.Error(), "could not be fully checked") {
		t.Fatalf("err=%v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 2 {
		t.Fatalf("install ran with truncated simulation: %v", calls)
	}
}

func TestSimulationWarnsAboutDependencyActions(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, false, "")
	stub.runner.handler = func(_ context.Context, call commandCall, stdout, _ io.Writer) error {
		if sameArgs(call.args, simulateArgs()) {
			fmt.Fprintln(stdout, "Inst libfoo [1.0] (2.0 repository)")
			fmt.Fprintln(stdout, "Inst incus (6.0 repository)")
		}
		return nil
	}
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), Options{Apply: true}, &out, &errOut); err != nil {
		t.Fatalf("apply: %v", err)
	}
	output := out.String()
	for _, want := range []string{"simulation package actions: libfoo, incus", "dependency packages may be installed or upgraded"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
	if calls := stub.runner.recorded(); len(calls) != 3 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestRemoveDisabledAbortIsSafetyRefusal(t *testing.T) {
	const output = "E: Packages need to be removed but remove is disabled.\n"
	t.Run("preflight", func(t *testing.T) {
		stub := newHost(t, ubuntuRelease, true, false, "")
		stub.runner.handler = func(_ context.Context, call commandCall, _, stderr io.Writer) error {
			if sameArgs(call.args, simulateArgs()) {
				fmt.Fprint(stderr, output)
				return errors.New("exit status 100")
			}
			return nil
		}
		var out, errOut bytes.Buffer
		err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "intentional safety refusal") {
			t.Fatalf("err=%v", err)
		}
		assertNoRecoveryGuidance(t, err)
		if !strings.Contains(err.Error(), "no packages were installed") {
			t.Fatalf("missing stage state: %v", err)
		}
		if calls := stub.runner.recorded(); len(calls) != 2 {
			t.Fatalf("install ran after --no-remove refusal: %v", calls)
		}
	})
	t.Run("install", func(t *testing.T) {
		stub := newHost(t, ubuntuRelease, true, false, "")
		stub.runner.handler = func(_ context.Context, call commandCall, _, stderr io.Writer) error {
			if sameArgs(call.args, installArgs()) {
				fmt.Fprint(stderr, output)
				return errors.New("exit status 100")
			}
			return nil
		}
		var out, errOut bytes.Buffer
		err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "intentional safety refusal") {
			t.Fatalf("err=%v", err)
		}
		assertNoRecoveryGuidance(t, err)
		if !strings.Contains(err.Error(), "no packages were installed") {
			t.Fatalf("missing stage state: %v", err)
		}
		if calls := stub.runner.recorded(); len(calls) != 3 {
			t.Fatalf("calls=%v", calls)
		}
	})
}

func TestLingeringInstallGuidanceWaitsBeforeRecovery(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, false, "")
	stub.runner.handler = func(_ context.Context, call commandCall, _, _ io.Writer) error {
		if sameArgs(call.args, installArgs()) {
			return fmt.Errorf("install: %w", &lingeringProcessError{name: aptGet, pid: 4242, grace: time.Minute})
		}
		return nil
	}
	var out, errOut bytes.Buffer
	err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
	if err == nil {
		t.Fatal("lingering install succeeded")
	}
	message := err.Error()
	wait := strings.Index(message, "wait until it and any dpkg child have exited")
	recovery := strings.Index(message, "`dpkg --configure -a`")
	if wait < 0 || recovery < 0 || wait > recovery {
		t.Fatalf("recovery was not gated after exit: %v", err)
	}
	for _, want := range []string{"still running", "not force-killed", "After it has exited", "`apt-get -f install`"} {
		if !strings.Contains(message, want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if calls := stub.runner.recorded(); len(calls) != 3 {
		t.Fatalf("calls=%v", calls)
	}
}

func assertNoRecoveryGuidance(t *testing.T, err error) {
	t.Helper()
	for _, unwanted := range []string{"dpkg --configure", "apt-get -f install", "Fix the package-manager", "network error"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("safety refusal contains %q: %v", unwanted, err)
		}
	}
}

func TestCancellationReportsRecoveryOnlyAfterExit(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, false, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub.runner.handler = func(ctx context.Context, call commandCall, _, stderr io.Writer) error {
		if sameArgs(call.args, installArgs()) {
			cancel()
			<-ctx.Done()
			fmt.Fprint(stderr, "E: dpkg was interrupted")
			return ctx.Err()
		}
		return nil
	}
	var out, errOut bytes.Buffer
	err := Run(ctx, Options{Apply: true}, &out, &errOut)
	if err == nil {
		t.Fatal("cancelled install succeeded")
	}
	for _, want := range []string{"cancelled", "dpkg was interrupted", "wait until apt-get and dpkg have exited", "`dpkg --configure -a`", "only after it has exited"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if calls := stub.runner.recorded(); len(calls) != 3 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestFailureDetailIsBounded(t *testing.T) {
	stub := newHost(t, ubuntuRelease, true, false, "")
	stub.runner.handler = func(_ context.Context, call commandCall, _, stderr io.Writer) error {
		if sameArgs(call.args, installArgs()) {
			fmt.Fprintln(stderr, strings.Repeat("x", 1<<20))
			return errors.New("exit status 100")
		}
		return nil
	}
	var out, errOut bytes.Buffer
	err := Run(context.Background(), Options{Apply: true}, &out, &errOut)
	if err == nil {
		t.Fatal("oversized failure succeeded")
	}
	if len(err.Error()) > 2000 {
		t.Fatalf("error grew to %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "...") {
		t.Fatalf("bounded detail missing ellipsis: %v", err)
	}
}

func TestExecuteCommandReportsLingeringProcess(t *testing.T) {
	previous := cancelGrace
	cancelGrace = 100 * time.Millisecond
	t.Cleanup(func() { cancelGrace = previous })

	pidFile := filepath.Join(t.TempDir(), "pid")
	// Test-only cleanup for the trap loop; it never targets apt or dpkg.
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(200*time.Millisecond, cancel)
	script := `printf '%s' "$$" > "$1"; trap '' TERM; while :; do sleep 1; done`
	err := executeCommand(ctx, "/bin/sh", []string{"-c", script, "sh", pidFile}, os.Environ(), io.Discard, io.Discard)

	var lingering *lingeringProcessError
	if !errors.As(err, &lingering) {
		t.Fatalf("err=%v", err)
	}
	if lingering.pid <= 0 {
		t.Fatalf("pid=%d", lingering.pid)
	}
	if !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "not force-killed") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseOSReleaseRejectsDuplicateRelevantKeys(t *testing.T) {
	_, err := parseOSRelease([]byte("ID=ubuntu\nID=debian\nVERSION_ID=\"24.04\"\n"))
	if err == nil || !strings.Contains(err.Error(), "duplicate ID") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseOSReleaseIgnoresUnknownMalformedKeys(t *testing.T) {
	data := "BROKEN=\"unterminated\nID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nexport NOPE=1\nWEIRD=$(touch /tmp/should-not-run)\n"
	info, err := parseOSRelease([]byte(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.id != "ubuntu" || info.versionID != "24.04" || info.name != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("info=%+v", info)
	}
}

func TestParseOSReleaseRejectsMalformedRelevantKey(t *testing.T) {
	_, err := parseOSRelease([]byte("ID=ubuntu\nVERSION_ID=\"unterminated\n"))
	if err == nil || !strings.Contains(err.Error(), "invalid VERSION_ID") {
		t.Fatalf("err=%v", err)
	}
}

func TestMissingOSReleaseFailsClearly(t *testing.T) {
	stub := newHost(t, ubuntuRelease, false, false, "")
	readFile = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	var out, errOut bytes.Buffer
	err := Run(context.Background(), Options{}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "cannot identify the operating system") {
		t.Fatalf("err=%v", err)
	}
	if calls := stub.runner.recorded(); len(calls) != 0 {
		t.Fatalf("calls=%v", calls)
	}
}

func hasEnv(env []string, key, value string) bool {
	for _, entry := range env {
		if entry == key+"="+value {
			return true
		}
	}
	return false
}

func envKeyPresent(env []string, key string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return true
		}
	}
	return false
}
