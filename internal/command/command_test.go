package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
)

type call struct {
	method    string
	name      string
	ctx       context.Context
	principal core.Principal
}

type fakeBackend struct {
	calls        []call
	instances    []core.Instance
	images       []core.ImageInfo
	created      core.Instance
	options      core.CreateOptions
	listError    error
	imagesError  error
	createError  error
	removeError  map[string]error
	resumeError  error
	startError   error
	stopError    error
	restartError error
	execError    error
	execStatus   int
	remote       string
	streams      core.Streams
}

func (f *fakeBackend) List(ctx context.Context, p core.Principal) ([]core.Instance, error) {
	f.calls = append(f.calls, call{"list", "", ctx, p})
	return f.instances, f.listError
}

func (f *fakeBackend) Images(ctx context.Context, p core.Principal) ([]core.ImageInfo, error) {
	f.calls = append(f.calls, call{"images", "", ctx, p})
	return f.images, f.imagesError
}

func (f *fakeBackend) Create(ctx context.Context, p core.Principal, options core.CreateOptions) (core.Instance, error) {
	f.calls = append(f.calls, call{"create", options.Name, ctx, p})
	f.options = options
	return f.created, f.createError
}

func (f *fakeBackend) Remove(ctx context.Context, p core.Principal, name string) error {
	f.calls = append(f.calls, call{"remove", name, ctx, p})
	return f.removeError[name]
}

func (f *fakeBackend) Resume(ctx context.Context, p core.Principal, name string) error {
	f.calls = append(f.calls, call{"resume", name, ctx, p})
	return f.resumeError
}

func (f *fakeBackend) Start(ctx context.Context, p core.Principal, name string) error {
	f.calls = append(f.calls, call{"start", name, ctx, p})
	return f.startError
}

func (f *fakeBackend) Stop(ctx context.Context, p core.Principal, name string) error {
	f.calls = append(f.calls, call{"stop", name, ctx, p})
	return f.stopError
}

func (f *fakeBackend) Restart(ctx context.Context, p core.Principal, name string) error {
	f.calls = append(f.calls, call{"restart", name, ctx, p})
	return f.restartError
}

func (f *fakeBackend) Exec(ctx context.Context, p core.Principal, name, command string, s core.Streams) (int, error) {
	f.calls = append(f.calls, call{"exec", name, ctx, p})
	f.remote, f.streams = command, s
	return f.execStatus, f.execError
}

var principal = core.Principal{Name: "alice", Project: "dev-alice"}

func run(h Handler, line string) (int, string, string) {
	var out, stderr bytes.Buffer
	status := h.Handle(context.Background(), principal, line, core.Streams{Out: &out, Err: &stderr})
	return status, out.String(), stderr.String()
}

func decodeObject(t *testing.T, text string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		t.Fatalf("invalid JSON %q: %v", text, err)
	}
	return object
}

func TestCreate(t *testing.T) {
	for _, line := range []string{
		`new --json --name=work --cpu=2 --memory=512MiB --processes=8 --disk=10GB`,
		`new --name='work' --cpu="2" --memory=512MiB --processes=8 --disk=10GB --json`,
	} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{created: core.Instance{Name: "work", Status: "Running", CPU: "2", Memory: "512MiB", Disk: "10GB"}}
			status, out, stderr := run(Handler{Backend: f}, line)
			if status != 0 || stderr != "" || len(f.calls) != 1 || f.calls[0].method != "create" {
				t.Fatalf("status=%d out=%q stderr=%q calls=%v", status, out, stderr, f.calls)
			}
			want := core.CreateOptions{Name: "work", CPU: 2, Memory: "512MiB", Processes: 8, Disk: "10GB"}
			if f.options != want {
				t.Fatalf("options=%+v, want %+v", f.options, want)
			}
			if got := decodeObject(t, out); !reflect.DeepEqual(got, map[string]any{"vm_name": "work", "status": "Running", "cpu": "2", "memory": "512MiB", "disk": "10GB"}) {
				t.Fatalf("summary=%v", got)
			}
		})
	}
	f := &fakeBackend{created: core.Instance{Name: "generated", Status: "Running"}}
	status, out, stderr := run(Handler{Backend: f, Limits: core.Limits{CPU: 64, Memory: "32GiB", Disk: "100GB"}}, "new")
	if status != 0 || stderr != "" || out != "generated\tRunning\n" || f.options != (core.CreateOptions{}) {
		t.Fatalf("default create: status=%d out=%q stderr=%q options=%+v", status, out, stderr, f.options)
	}
}

func TestListSchemaAndFilters(t *testing.T) {
	for _, line := range []string{`ls --json 'work-*'`, `ls 'work-*' --json`, `ls -l 'work-*' --json`, `ls 'work-*' --json -l`} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{instances: []core.Instance{
				{Name: "work-one", Status: "Running", CPU: "2", Memory: "1GiB"},
				{Name: "other", Status: "Stopped"},
			}}
			status, out, stderr := run(Handler{Backend: f}, line)
			want := map[string]any{"vms": []any{map[string]any{"vm_name": "work-one", "status": "Running", "cpu": "2", "memory": "1GiB"}}}
			if status != 0 || stderr != "" || !reflect.DeepEqual(decodeObject(t, out), want) {
				t.Fatalf("status=%d out=%q stderr=%q", status, out, stderr)
			}
		})
	}
	for _, instances := range [][]core.Instance{nil, {}, {{Name: "other", Status: "Stopped"}}} {
		status, out, stderr := run(Handler{Backend: &fakeBackend{instances: instances}}, "ls missing --json")
		if status != 0 || stderr != "" || out != "{\"vms\":[]}\n" {
			t.Fatalf("empty list: status=%d out=%q stderr=%q", status, out, stderr)
		}
	}
	f := &fakeBackend{instances: []core.Instance{{Name: "work", Status: "Stopped"}}}
	_, out, _ := run(Handler{Backend: f}, "ls --json")
	want := map[string]any{"vms": []any{map[string]any{"vm_name": "work", "status": "Stopped"}}}
	if !reflect.DeepEqual(decodeObject(t, out), want) {
		t.Fatalf("unknown resources must be omitted: %s", out)
	}
	f.instances[0] = core.Instance{Name: "work", Status: "Running", CPU: "2", Memory: "1GiB", Processes: "512", Disk: "10GB"}
	_, out, _ = run(Handler{Backend: f}, "ls -l work")
	if out != "work\tRunning\t2\t1GiB\t512\t10GB\n" {
		t.Fatalf("long list=%q", out)
	}
}

func TestProcessLimitInJSONAndLongList(t *testing.T) {
	created := core.Instance{Name: "work", Status: "Running", CPU: "2", Memory: "1GiB", Processes: "512", Disk: "10GB"}
	f := &fakeBackend{created: created}
	status, out, stderr := run(Handler{Backend: f}, "new --name=work --processes=512 --json")
	if status != 0 || stderr != "" || f.options.Processes != 512 {
		t.Fatalf("status=%d stderr=%q options=%+v", status, stderr, f.options)
	}
	want := map[string]any{"vm_name": "work", "status": "Running", "cpu": "2", "memory": "1GiB", "processes": "512", "disk": "10GB"}
	if got := decodeObject(t, out); !reflect.DeepEqual(got, want) {
		t.Fatalf("new summary=%v want=%v", got, want)
	}

	listed := &fakeBackend{instances: []core.Instance{
		created,
		{Name: "unknown", Status: "Stopped"},
	}}
	_, out, stderr = run(Handler{Backend: listed}, "ls --json")
	if stderr != "" {
		t.Fatalf("ls stderr=%q", stderr)
	}
	wantList := map[string]any{"vms": []any{
		map[string]any{"vm_name": "work", "status": "Running", "cpu": "2", "memory": "1GiB", "processes": "512", "disk": "10GB"},
		map[string]any{"vm_name": "unknown", "status": "Stopped"},
	}}
	if got := decodeObject(t, out); !reflect.DeepEqual(got, wantList) {
		t.Fatalf("ls summary=%v want=%v", got, wantList)
	}

	_, out, _ = run(Handler{Backend: listed}, "ls -l")
	if out != "work\tRunning\t2\t1GiB\t512\t10GB\nunknown\tStopped\t\t\t\t\n" {
		t.Fatalf("long list=%q", out)
	}
}

func TestAcceptedResourceSyntax(t *testing.T) {
	for _, unit := range []string{"B", "kB", "KB", "MB", "GB", "TB", "KiB", "MiB", "GiB", "TiB"} {
		f := &fakeBackend{}
		status, out, stderr := run(Handler{Backend: f}, "new --cpu=2147483647 --memory=1"+unit+" --processes=2147483647 --disk=2"+unit)
		if status != 0 || stderr != "" || len(f.calls) != 1 || f.options.Memory != "1"+unit || f.options.Disk != "2"+unit || f.options.CPU != 2147483647 || f.options.Processes != 2147483647 {
			t.Fatalf("unit=%s status=%d out=%q stderr=%q options=%+v", unit, status, out, stderr, f.options)
		}
	}
	f := &fakeBackend{}
	name := "a" + strings.Repeat("-", 61) + "z"
	status, _, stderr := run(Handler{Backend: f}, "new --name="+name)
	if status != 0 || stderr != "" || f.options.Name != name {
		t.Fatalf("63-byte name: status=%d stderr=%q options=%+v", status, stderr, f.options)
	}
}

func TestRemoveMultipleAndPartialFailures(t *testing.T) {
	for _, line := range []string{"rm --json first second third", "rm first --json second third", "rm first second third --json"} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{removeError: map[string]error{"second": errors.New("not owned")}}
			status, out, stderr := run(Handler{Backend: f}, line)
			want := map[string]any{
				"removed": []any{"first", "third"},
				"errors":  map[string]any{"second": "not owned"},
				"error":   "one or more instances could not be removed",
			}
			if status == 0 || stderr != "" || !reflect.DeepEqual(decodeObject(t, out), want) {
				t.Fatalf("status=%d out=%q stderr=%q", status, out, stderr)
			}
			var names []string
			for _, call := range f.calls {
				if call.method != "remove" {
					t.Fatalf("unexpected call=%+v", call)
				}
				names = append(names, call.name)
			}
			if !reflect.DeepEqual(names, []string{"first", "second", "third"}) {
				t.Fatalf("remove order=%v", names)
			}
		})
	}
	f := &fakeBackend{removeError: map[string]error{"bad": errors.New("not owned")}}
	status, out, stderr := run(Handler{Backend: f}, "rm good bad next")
	if status == 0 || out != "removed good\nremoved next\n" || !strings.Contains(stderr, "bad: not owned") {
		t.Fatalf("text partial failure: status=%d out=%q stderr=%q", status, out, stderr)
	}
	_, out, _ = run(Handler{Backend: f}, "rm bad --json")
	if got := decodeObject(t, out)["removed"]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("removed must be an empty array: %s", out)
	}
	status, out, stderr = run(Handler{Backend: f}, "rm good next --json")
	if status != 0 || stderr != "" || out != "{\"removed\":[\"good\",\"next\"]}\n" {
		t.Fatalf("successful remove: status=%d out=%q stderr=%q", status, out, stderr)
	}
}

func TestLifecycleCommands(t *testing.T) {
	for _, tc := range []struct {
		command string
		method  string
		text    string
	}{
		{"start", "start", "started work\n"},
		{"stop", "stop", "stopped work\n"},
		{"restart", "restart", "restarted work\n"},
		{"resume", "resume", "resumed work\n"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			for _, line := range []string{tc.command + " work --json", tc.command + " --json work"} {
				f := &fakeBackend{}
				status, out, stderr := run(Handler{Backend: f}, line)
				if status != 0 || stderr != "" || out != "{\"vm_name\":\"work\"}\n" || len(f.calls) != 1 || f.calls[0].method != tc.method || f.calls[0].name != "work" {
					t.Fatalf("%s: status=%d out=%q stderr=%q calls=%v", line, status, out, stderr, f.calls)
				}
			}
			f := &fakeBackend{}
			status, out, stderr := run(Handler{Backend: f}, tc.command+" work")
			if status != 0 || stderr != "" || out != tc.text || len(f.calls) != 1 || f.calls[0].method != tc.method || f.calls[0].name != "work" {
				t.Fatalf("%s: status=%d out=%q stderr=%q calls=%v", tc.command, status, out, stderr, f.calls)
			}
		})
	}
}

func TestInvalidInputNeverCallsBackend(t *testing.T) {
	lines := []string{
		"", " \t ", "bogus", "new --project=default", "new --user=root", "new --profile=default",
		"new --name=work --unknown=x", "new --name=work --cpu=0", "new --cpu=-1", "new --cpu=1.5", "new --cpu=+2", "new --cpu=abc", "new --cpu=2147483648", "new --cpu=99999999999999999999999999",
		"new --cpu=", "new --cpu 2", "new --processes=0", "new --processes=-1", "new --processes=1.5", "new --processes=+2", "new --processes=abc", "new --processes=2147483648", "new --processes=", "new --processes 8", "new --processes=1 --processes=2",
		"new --memory=0GiB", "new --memory=-1GiB", "new --memory=1.5GiB", "new --memory=0", "new --memory=512", "new --memory=9999999999999999999999999B", "new --memory=garbage", "new --disk=0GB", "new --disk=1;id",
		"new --image=", "new --image", "new --image=--latest", "new --image=-ubuntu", "new --image=http://example.com/ubuntu", "new --image=images:ubuntu", "new --image=ubuntu:24.04", "new --image=ubuntu/../x", "new --image=ubuntu//x", "new --image=ubuntu/", "new --image=/ubuntu", "new --image=local:", "new --image=local:../x", "new --image='ubuntu 24.04'", "new --image=" + strings.Repeat("a", maxImageRefBytes+1),
		"new --name=", "new --name=-work", "new --name=work-", "new --name=UPPER", "new --name=1work", "new --name=a/b", "new --name='a b'", "new --name=" + strings.Repeat("a", 64),
		"new work", "new --cpu=1 --cpu=2", "ls --json --json", "ls --json=true", "ls --json=false", "ls -x", "ls --all", "ls --", "ls one two", "ls '['", "ls '../*'", "ls '**/foo'", "ls ''", "ls 'a;b'", "ls " + strings.Repeat("a", 129),
		"image", "image list extra", "image list --json=true", "image list -l", "image list --all", "image list --json --json", "image list --remote=images:ubuntu", "image --pull", "image add ubuntu", "image pull ubuntu", "image remove ubuntu", "image rm ubuntu", "image import image.tar", "image export ubuntu", "image copy ubuntu", "image alias list", "image bogus",
		"rm", "rm good ../bad", "rm good --force", "rm good bad/thing --json", "rm good good", "rm '*'", "resume", "resume one two", "resume work --force",
		"start", "start one two", "start work --force", "start work --timeout=5", "start work work", "start --all", "start work --project=dev-bob", "stop", "stop one two", "stop work --force", "stop --all", "stop work --timeout=0", "restart", "restart one two", "restart work --force", "restart work --timeout=0", "restart --profile=ifd",
		"whoami extra", "whoami --keys", "help ls extra", "help unknown", "help --all", "? --unknown",
		"clear now", "clear --json", "clear --json now", "clear --json=true", "clear -x", "clear --all",
		"ssh", "ssh --json work", "ssh ../work id", "ssh 'unterminated", "ls 'unterminated", "new --name='unterminated", `ls \`,
		"ls\x00 --json", "ls\nrm good", "ssh work echo\rtest", "ssh work echo\x1btest", "ls\u0085", "ls\xff", strings.Repeat(" ", maxLineBytes+1),
		"rm " + strings.Repeat("a ", maxArguments+1),
	}
	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{}
			status, out, stderr := run(Handler{Backend: f}, line)
			if status == 0 || len(f.calls) != 0 || out+stderr == "" {
				t.Fatalf("status=%d out=%q stderr=%q calls=%v", status, out, stderr, f.calls)
			}
		})
	}
}

func TestValidationErrorsUseRequestedJSON(t *testing.T) {
	for _, line := range []string{
		"rm good --bad --json", "rm --json good --bad", "resume --json", "start --json", "stop --json", "restart --json", "start work --json --json", "ls --bad --json", "new --cpu=0 --json", "bogus --json", "help nonsense --json", "ls --json 'unterminated", "ls --json --json", "ls --json=true --json", "ls --json " + strings.Repeat("a ", maxArguments+1),
		"new --json --name=bad\x00", "new --name=bad\x00 --json", "ls --json\n", "ls --json \xff", "ls --json " + strings.Repeat("a", maxLineBytes),
	} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{}
			status, out, stderr := run(Handler{Backend: f}, line)
			object := decodeObject(t, out)
			if status == 0 || stderr != "" || object["error"] == nil || len(object) != 1 || len(f.calls) != 0 {
				t.Fatalf("status=%d out=%q stderr=%q calls=%v", status, out, stderr, f.calls)
			}
		})
	}
}

func TestUnsupportedCommands(t *testing.T) {
	for _, command := range []string{"pause", "share", "ssh-key list", "ssh-key add secret", "sftp", "scp", "portforward", "port-forward", "direct-host"} {
		for _, jsonFlag := range []string{"", " --json"} {
			line := command + jsonFlag
			t.Run(line, func(t *testing.T) {
				f := &fakeBackend{}
				status, out, stderr := run(Handler{Backend: f}, line)
				if status == 0 || len(f.calls) != 0 || !strings.Contains(out+stderr, "unsupported") {
					t.Fatalf("status=%d out=%q stderr=%q calls=%v", status, out, stderr, f.calls)
				}
				if command == "pause" && !strings.Contains(out+stderr, "RAM-to-disk suspend") {
					t.Fatalf("pause explanation missing: %s%s", out, stderr)
				}
				if jsonFlag != "" {
					if decodeObject(t, out)["error"] == nil || stderr != "" {
						t.Fatalf("JSON error missing: %q %q", out, stderr)
					}
				}
			})
		}
	}
}

func TestSSHRawShellBoundary(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"ssh work", ""},
		{"ssh work \t ", ""},
		{`ssh work printf '%s' 'a b'`, `printf '%s' 'a b'`},
		{`ssh work 'printf "%s" "a b"'`, `printf "%s" "a b"`},
		{`ssh work "printf \"%s\" \"a b\""`, `printf "%s" "a b"`},
		{`ssh work "printf '%s' '\q'"`, `printf '%s' '\q'`},
		{`ssh work "echo \$HOME \` + "`" + `id\` + "`" + ` \\"`, "echo $HOME `id` \\"},
		{`ssh work echo "$HOME" && id | cat > /tmp/result`, `echo "$HOME" && id | cat > /tmp/result`},
		{`ssh work printf '%s' "$(id)"; echo $USER`, `printf '%s' "$(id)"; echo $USER`},
		{`ssh work printf '%s' 'a;b | $(id)'`, `printf '%s' 'a;b | $(id)'`},
		{`ssh work tool --json --image=anything --project=default`, `tool --json --image=anything --project=default`},
		{`ssh work --json`, `--json`},
		{`ssh work --json --json=true`, `--json --json=true`},
		{`ssh work 'printf' '%s' 'a b'`, `'printf' '%s' 'a b'`},
		{`ssh work "printf" '%s' 'a b'`, `"printf" '%s' 'a b'`},
		{`ssh work 'echo'" hi"`, `'echo'" hi"`},
		{`ssh work 'it'\''s'`, `'it'\''s'`},
		{`ssh work echo a\ b`, `echo a\ b`},
		{"ssh\t'work'\tprintf  '%s'\t'a b'  ", "printf  '%s'\t'a b'  "},
		{"ssh work 'echo a  b'  ", "echo a  b"},
		{`ssh work echo 'unterminated`, `echo 'unterminated`},
		{`ssh work 'unterminated`, `'unterminated`},
		{`ssh work echo # --json`, `echo # --json`},
		{`ssh work sh -c 'printf "%s" "a b"'`, `sh -c 'printf "%s" "a b"'`},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			f := &fakeBackend{execStatus: 37}
			status, out, stderr := run(Handler{Backend: f}, tc.line)
			if status != 37 || out != "" || stderr != "" || f.remote != tc.want || len(f.calls) != 1 || f.calls[0].method != "exec" || f.calls[0].name != "work" {
				t.Fatalf("status=%d out=%q stderr=%q command=%q want=%q calls=%v", status, out, stderr, f.remote, tc.want, f.calls)
			}
		})
	}
}

func TestContextPrincipalAndStreamsPropagation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resize := make(chan core.Window)
	signals := make(chan string)
	s := core.Streams{
		In: strings.NewReader("input"), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, PTY: true,
		Term: "xterm-256color", Window: core.Window{Width: 123, Height: 45}, Resize: resize, Signals: signals,
	}
	for _, line := range []string{"new", "ls", "image list", "rm first second", "resume work", "start work", "stop work", "restart work", "ssh work", "ssh work id"} {
		f := &fakeBackend{}
		h := Handler{Backend: f}
		if status := h.Handle(ctx, principal, line, s); status != 0 {
			t.Fatalf("%s status=%d", line, status)
		}
		for _, call := range f.calls {
			if call.ctx != ctx || call.principal != principal {
				t.Fatalf("%s lost context/principal: %+v", line, call)
			}
		}
		if strings.HasPrefix(line, "ssh") && !reflect.DeepEqual(f.streams, s) {
			t.Fatalf("lost stream fields: %+v", f.streams)
		}
	}
	f := &fakeBackend{}
	if status := (Handler{Backend: f}).Handle(ctx, principal, "ssh work", core.Streams{}); status != 0 || !reflect.DeepEqual(f.streams, core.Streams{}) {
		t.Fatalf("nil streams changed: status=%d streams=%+v", status, f.streams)
	}
}

type countingReader struct {
	reads int
}

func (r *countingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

func TestExplicitEmptyGuestCommandIsNoop(t *testing.T) {
	for _, line := range []string{"ssh work ''", `ssh work ""`, "ssh work '' \t "} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{}
			in := &countingReader{}
			status := (Handler{Backend: f}).Handle(context.Background(), principal, line, core.Streams{In: in})
			if status != 0 || len(f.calls) != 1 || f.calls[0].method != "exec" || f.calls[0].name != "work" {
				t.Fatalf("status=%d calls=%v", status, f.calls)
			}
			if f.remote != noopRemoteCommand {
				t.Fatalf("remote=%q, want the shell no-op %q", f.remote, noopRemoteCommand)
			}
			if in.reads != 0 {
				t.Fatalf("explicit empty command read stdin %d times", in.reads)
			}
		})
	}
}

func TestAbsentGuestCommandStaysInteractive(t *testing.T) {
	for _, line := range []string{"ssh work", "ssh work \t "} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{}
			in := &countingReader{}
			streams := core.Streams{In: in, PTY: true, Term: "xterm-256color"}
			status := (Handler{Backend: f}).Handle(context.Background(), principal, line, streams)
			if status != 0 || len(f.calls) != 1 || f.remote != "" {
				t.Fatalf("status=%d remote=%q calls=%v", status, f.remote, f.calls)
			}
			if in.reads != 0 || f.streams.In != in || !f.streams.PTY {
				t.Fatalf("interactive streams changed before the backend ran: reads=%d streams=%+v", in.reads, f.streams)
			}
		})
	}
}

func TestCommentedJSONFlagDoesNotEnableJSON(t *testing.T) {
	for _, line := range []string{"ls # --json", "ls demo # --json", "new # --json", "rm demo # --json", "resume demo # --json"} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{instances: []core.Instance{{Name: "demo", Status: "Running"}}}
			status, out, stderr := run(Handler{Backend: f}, line)
			if status != 0 || stderr != "" || strings.HasPrefix(strings.TrimSpace(out), "{") {
				t.Fatalf("comment enabled JSON or failed: status=%d out=%q stderr=%q", status, out, stderr)
			}
		})
	}
	f := &fakeBackend{instances: []core.Instance{{Name: "demo", Status: "Running"}}}
	status, out, stderr := run(Handler{Backend: f}, "ls --json # trailing comment")
	if status != 0 || stderr != "" || !strings.HasPrefix(out, "{\"vms\":") {
		t.Fatalf("JSON before a comment must stay enabled: status=%d out=%q stderr=%q", status, out, stderr)
	}
}

func TestRequestedJSONScan(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"ls --json", true},
		{"image list --json", true},
		{"image --json list", true},
		{"image --json list --json", true},
		{"image list --json # trailing comment", true},
		{"ls --json # trailing comment", true},
		{"ls # --json", false},
		{"new --name=work # --json", false},
		{"rm work # --json", false},
		{"ssh work --json", false},
		{"ssh --json work", false},
		{"ls --json 'unterminated", true},
		{"ls 'unterminated --json", false},
		{"ls --json " + strings.Repeat("a", maxLineBytes), true},
		{strings.Repeat("a", maxLineBytes+1), false},
	} {
		if got := requestedJSON(tc.line); got != tc.want {
			t.Fatalf("requestedJSON(%q)=%v want %v", tc.line, got, tc.want)
		}
	}
}

func TestBackendErrors(t *testing.T) {
	err := errors.New("backend refused")
	for _, line := range []string{"new", "ls", "image list", "rm work", "resume work", "start work", "stop work", "restart work", "ssh work --json"} {
		for _, jsonFlag := range []string{"", " --json"} {
			f := &fakeBackend{listError: err, imagesError: err, createError: err, removeError: map[string]error{"work": err}, resumeError: err, startError: err, stopError: err, restartError: err, execError: err, execStatus: 42}
			status, out, stderr := run(Handler{Backend: f}, line+jsonFlag)
			if status == 0 || !strings.Contains(out+stderr, err.Error()) {
				t.Fatalf("%s%s: status=%d out=%q stderr=%q", line, jsonFlag, status, out, stderr)
			}
			if jsonFlag != "" && !strings.HasPrefix(line, "ssh") {
				if decodeObject(t, out)["error"] == nil || stderr != "" {
					t.Fatalf("JSON error: out=%q stderr=%q", out, stderr)
				}
			} else if out != "" || stderr == "" {
				t.Fatalf("text error: out=%q stderr=%q", out, stderr)
			}
		}
	}
}

func flattened(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func assertHelpText(t *testing.T, text string) {
	t.Helper()
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("help is not newline terminated: %q", text)
	}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if len(line) > 80 {
			t.Fatalf("help line %d is %d columns, exceeding a narrow 80-column terminal: %q", i+1, len(line), line)
		}
		if strings.ContainsRune(line, '\t') {
			t.Fatalf("help line %d contains a tab: %q", i+1, line)
		}
		if line != strings.TrimRight(line, " ") {
			t.Fatalf("help line %d has trailing spaces: %q", i+1, line)
		}
	}
	flat := flattened(text)
	for _, command := range commands {
		if !strings.Contains(flat, command.Usage) || !strings.Contains(flat, command.Description) {
			t.Fatalf("help omits the current %s entry: %q", command.Name, flat)
		}
	}
	for _, want := range []string{
		"Commands:", "Omitted resources default to 1 CPU, 2GiB memory and 256 processes",
		"per-container maxima, not defaults", "1GiB host memory reserve",
		"data is preserved", "without asking for confirmation", "Unsupported: pause (no RAM-to-disk suspend guarantee)",
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("help omits %q: %q", want, flat)
		}
	}
}

func TestHelpPlainTextIsCompleteAndNarrow(t *testing.T) {
	var out, stderr bytes.Buffer
	status := (Handler{}).Handle(context.Background(), principal, "help all", core.Streams{Out: &out, Err: &stderr})
	if status != 0 || stderr.Len() != 0 {
		t.Fatalf("status=%d stderr=%q", status, stderr.String())
	}
	if strings.ContainsRune(out.String(), '\r') {
		t.Fatalf("plain help contains CR: %q", out.String())
	}
	assertHelpText(t, out.String())
}

func TestHelpListsExactlySupportedCommands(t *testing.T) {
	var out, stderr bytes.Buffer
	status := (Handler{}).Handle(context.Background(), principal, "help --json", core.Streams{Out: &out, Err: &stderr})
	if status != 0 || stderr.Len() != 0 {
		t.Fatal(status, stderr.String())
	}
	object := decodeObject(t, out.String())
	listed := make(map[string]bool)
	for _, item := range object["commands"].([]any) {
		entry := item.(map[string]any)
		name := entry["name"].(string)
		listed[name] = true
		if usage, ok := entry["usage"].(string); !ok || usage == "" {
			t.Fatalf("%s has no usage: %v", name, entry)
		}
		if description, ok := entry["description"].(string); !ok || description == "" {
			t.Fatalf("%s has no description: %v", name, entry)
		}
		if err := unsupported(name); err != nil {
			t.Fatalf("help lists unsupported %s: %v", name, err)
		}
	}
	for _, name := range []string{"help", "whoami", "new", "ls", "image", "clear", "rm", "start", "stop", "restart", "resume", "ssh"} {
		if !listed[name] {
			t.Fatalf("help omits %s: %v", name, listed)
		}
	}
	if len(listed) != 12 {
		t.Fatalf("help lists unexpected commands: %v", listed)
	}
	flat := flattened(object["description"].(string))
	if !strings.Contains(flat, "setup runs locally") || strings.Contains(flat, "setup over SSH") {
		t.Fatalf("administrator setup must stay a local command: %q", flat)
	}
}

func TestHelpTopicsStateLifecycleAndDestroySemantics(t *testing.T) {
	for _, topic := range []string{"start", "stop", "restart", "resume"} {
		_, out, stderr := run(Handler{}, "help "+topic)
		if stderr != "" || !strings.Contains(flattened(out), "data is preserved") {
			t.Fatalf("help %s does not state data preservation: %q %q", topic, out, stderr)
		}
	}
	_, out, _ := run(Handler{}, "help rm")
	if !strings.Contains(flattened(out), "without asking for confirmation") {
		t.Fatalf("rm help must state that removal is unconfirmed: %q", out)
	}
	_, out, _ = run(Handler{}, "help new")
	flat := flattened(out)
	if !strings.Contains(flat, "default to 1 CPU, 2GiB memory and 256 processes") || !strings.Contains(flat, "capped down by the operator maxima") {
		t.Fatalf("new help confuses defaults with maxima: %q", flat)
	}
}

func TestHelpAndIdentity(t *testing.T) {
	for _, line := range []string{"help", "?", "help all", "help ssh", "? new"} {
		f := &fakeBackend{}
		status, out, stderr := run(Handler{Backend: f}, line)
		if status != 0 || stderr != "" || !strings.Contains(out, "incus-for-devs") || !strings.Contains(out, "subset") || len(f.calls) != 0 {
			t.Fatalf("%s: status=%d out=%q stderr=%q calls=%v", line, status, out, stderr, f.calls)
		}
	}
	_, newHelp, _ := run(Handler{}, "help new")
	if !strings.Contains(newHelp, "--processes=N") {
		t.Fatalf("new help omits process limit: %q", newHelp)
	}
	_, startHelp, _ := run(Handler{}, "help start")
	if !strings.Contains(startHelp, "start NAME [--json]") || !strings.Contains(startHelp, "resume") {
		t.Fatalf("start help omits usage or resume alias: %q", startHelp)
	}
	_, resumeHelp, _ := run(Handler{}, "help resume")
	if !strings.Contains(resumeHelp, "Alias of start") {
		t.Fatalf("resume help omits start alias: %q", resumeHelp)
	}
	_, allHelp, _ := run(Handler{}, "help all")
	for _, usage := range []string{"stop NAME [--json]", "restart NAME [--json]", "resume NAME [--json]"} {
		if !strings.Contains(allHelp, usage) {
			t.Fatalf("help omits %q: %q", usage, allHelp)
		}
	}
	if strings.Contains(unsupportedDescription, "stop") || strings.Contains(unsupportedDescription, "restart") {
		t.Fatalf("help still lists stop/restart as unsupported: %q", unsupportedDescription)
	}
	if !strings.Contains(unsupportedDescription, "share") || !strings.Contains(unsupportedDescription, "ssh-key") || !strings.Contains(unsupportedDescription, "SFTP") {
		t.Fatalf("help lost unsupported commands: %q", unsupportedDescription)
	}
	limits := core.Limits{MaxInstances: 3, CPU: 4, Memory: "8GiB", Processes: 512, Disk: "20GB"}
	for _, line := range []string{"help ssh --json", "help --json ssh", "? --json ssh"} {
		status, out, stderr := run(Handler{Limits: limits}, line)
		object := decodeObject(t, out)
		if status != 0 || stderr != "" || object["program"] != "incus-for-devs" {
			t.Fatalf("%s: status=%d out=%q stderr=%q", line, status, out, stderr)
		}
		if got := object["commands"].([]any); len(got) != 1 || got[0].(map[string]any)["name"] != "ssh" {
			t.Fatalf("topic help=%v", got)
		}
		if got := object["limits"].(map[string]any); got["cpu"] != float64(4) || got["memory"] != "8GiB" || got["processes"] != float64(512) || got["max_instances"] != float64(3) {
			t.Fatalf("help limits=%v", got)
		}
	}
	_, out, _ := run(Handler{}, "help all --json")
	if got := decodeObject(t, out)["commands"].([]any); len(got) != len(commands) {
		t.Fatalf("all commands=%v", got)
	}
	status, out, stderr := run(Handler{}, "whoami --json")
	if status != 0 || stderr != "" || !reflect.DeepEqual(decodeObject(t, out), map[string]any{"name": "alice", "project": "dev-alice"}) {
		t.Fatalf("identity: status=%d out=%q stderr=%q", status, out, stderr)
	}
	status, out, stderr = run(Handler{}, "whoami")
	if status != 0 || stderr != "" || out != "alice\tdev-alice\n" {
		t.Fatalf("text identity: status=%d out=%q stderr=%q", status, out, stderr)
	}
}

func TestMissingBackendAndNilWriters(t *testing.T) {
	status, out, stderr := run(Handler{}, "ls --json")
	if status == 0 || stderr != "" || decodeObject(t, out)["error"] != "backend is not configured" {
		t.Fatalf("status=%d out=%q stderr=%q", status, out, stderr)
	}
	h := Handler{}
	if status := h.Handle(context.Background(), principal, "help", core.Streams{}); status != 0 {
		t.Fatalf("nil streams status=%d", status)
	}
	if status := h.Handle(context.Background(), principal, "unknown", core.Streams{}); status == 0 {
		t.Fatal("nil streams suppressed error status")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestOutputWriteFailure(t *testing.T) {
	for _, line := range []string{"help", "whoami --json", "ls --json", "ls", "image list", "image list --json", "new", "resume work", "start work", "stop work --json", "restart work", "rm work --json", "rm work", "bogus --json"} {
		f := &fakeBackend{instances: []core.Instance{{Name: "work"}}, images: []core.ImageInfo{{Fingerprint: "0123456789abcdef"}}}
		var stderr bytes.Buffer
		status := (Handler{Backend: f}).Handle(context.Background(), principal, line, core.Streams{Out: brokenWriter{}, Err: &stderr})
		if status == 0 || !strings.Contains(stderr.String(), "closed pipe") {
			t.Fatalf("%s: status=%d stderr=%q", line, status, stderr.String())
		}
	}
}
