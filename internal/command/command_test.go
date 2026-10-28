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
	calls       []call
	instances   []core.Instance
	created     core.Instance
	options     core.CreateOptions
	listError   error
	createError error
	removeError map[string]error
	resumeError error
	execError   error
	execStatus  int
	remote      string
	streams     core.Streams
}

func (f *fakeBackend) List(ctx context.Context, p core.Principal) ([]core.Instance, error) {
	f.calls = append(f.calls, call{"list", "", ctx, p})
	return f.instances, f.listError
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
		`new --json --name=work --cpu=2 --memory=512MiB --disk=10GB`,
		`new --name='work' --cpu="2" --memory=512MiB --disk=10GB --json`,
	} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{created: core.Instance{Name: "work", Status: "Running", CPU: "2", Memory: "512MiB", Disk: "10GB"}}
			status, out, stderr := run(Handler{Backend: f}, line)
			if status != 0 || stderr != "" || len(f.calls) != 1 || f.calls[0].method != "create" {
				t.Fatalf("status=%d out=%q stderr=%q calls=%v", status, out, stderr, f.calls)
			}
			want := core.CreateOptions{Name: "work", CPU: 2, Memory: "512MiB", Disk: "10GB"}
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
	f.instances[0] = core.Instance{Name: "work", Status: "Running", CPU: "2", Memory: "1GiB", Disk: "10GB"}
	_, out, _ = run(Handler{Backend: f}, "ls -l work")
	if out != "work\tRunning\t2\t1GiB\t10GB\n" {
		t.Fatalf("long list=%q", out)
	}
}

func TestAcceptedResourceSyntax(t *testing.T) {
	for _, unit := range []string{"B", "kB", "KB", "MB", "GB", "TB", "KiB", "MiB", "GiB", "TiB"} {
		f := &fakeBackend{}
		status, out, stderr := run(Handler{Backend: f}, "new --cpu=2147483647 --memory=1"+unit+" --disk=2"+unit)
		if status != 0 || stderr != "" || len(f.calls) != 1 || f.options.Memory != "1"+unit || f.options.Disk != "2"+unit || f.options.CPU != 2147483647 {
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

func TestResume(t *testing.T) {
	for _, line := range []string{"resume work --json", "resume --json work"} {
		f := &fakeBackend{}
		status, out, stderr := run(Handler{Backend: f}, line)
		if status != 0 || stderr != "" || out != "{\"vm_name\":\"work\"}\n" || len(f.calls) != 1 || f.calls[0].method != "resume" || f.calls[0].name != "work" {
			t.Fatalf("%s: status=%d out=%q stderr=%q calls=%v", line, status, out, stderr, f.calls)
		}
	}
}

func TestInvalidInputNeverCallsBackend(t *testing.T) {
	lines := []string{
		"", " \t ", "bogus", "new --image=ubuntu", "new --project=default", "new --user=root", "new --profile=default",
		"new --name=work --unknown=x", "new --name=work --cpu=0", "new --cpu=-1", "new --cpu=1.5", "new --cpu=+2", "new --cpu=abc", "new --cpu=2147483648", "new --cpu=99999999999999999999999999",
		"new --cpu=", "new --cpu 2", "new --memory=0GiB", "new --memory=-1GiB", "new --memory=1.5GiB", "new --memory=0", "new --memory=512", "new --memory=9999999999999999999999999B", "new --memory=garbage", "new --disk=0GB", "new --disk=1;id",
		"new --name=", "new --name=-work", "new --name=work-", "new --name=UPPER", "new --name=1work", "new --name=a/b", "new --name='a b'", "new --name=" + strings.Repeat("a", 64),
		"new work", "new --cpu=1 --cpu=2", "ls --json --json", "ls --json=true", "ls --json=false", "ls -x", "ls --all", "ls --", "ls one two", "ls '['", "ls '../*'", "ls '**/foo'", "ls ''", "ls 'a;b'", "ls " + strings.Repeat("a", 129),
		"rm", "rm good ../bad", "rm good --force", "rm good bad/thing --json", "rm good good", "rm '*'", "resume", "resume one two", "resume work --force", "whoami extra", "whoami --keys", "help ls extra", "help unknown", "help --all", "? --unknown",
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
		"rm good --bad --json", "rm --json good --bad", "resume --json", "ls --bad --json", "new --cpu=0 --json", "bogus --json", "help nonsense --json", "ls --json 'unterminated", "ls --json --json", "ls --json=true --json", "ls --json " + strings.Repeat("a ", maxArguments+1),
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
	for _, command := range []string{"pause", "stop", "restart", "share", "ssh-key list", "ssh-key add secret", "sftp", "scp", "portforward", "port-forward", "direct-host"} {
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
	for _, line := range []string{"new", "ls", "rm first second", "resume work", "ssh work", "ssh work id"} {
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

func TestBackendErrors(t *testing.T) {
	err := errors.New("backend refused")
	for _, line := range []string{"new", "ls", "rm work", "resume work", "ssh work --json"} {
		for _, jsonFlag := range []string{"", " --json"} {
			f := &fakeBackend{listError: err, createError: err, removeError: map[string]error{"work": err}, resumeError: err, execError: err, execStatus: 42}
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

func TestHelpAndIdentity(t *testing.T) {
	for _, line := range []string{"help", "?", "help all", "help ssh", "? new"} {
		f := &fakeBackend{}
		status, out, stderr := run(Handler{Backend: f}, line)
		if status != 0 || stderr != "" || !strings.Contains(out, "incus-for-devs") || !strings.Contains(out, "subset") || len(f.calls) != 0 {
			t.Fatalf("%s: status=%d out=%q stderr=%q calls=%v", line, status, out, stderr, f.calls)
		}
	}
	limits := core.Limits{MaxInstances: 3, CPU: 4, Memory: "8GiB", Disk: "20GB"}
	for _, line := range []string{"help ssh --json", "help --json ssh", "? --json ssh"} {
		status, out, stderr := run(Handler{Limits: limits}, line)
		object := decodeObject(t, out)
		if status != 0 || stderr != "" || object["program"] != "incus-for-devs" {
			t.Fatalf("%s: status=%d out=%q stderr=%q", line, status, out, stderr)
		}
		if got := object["commands"].([]any); len(got) != 1 || got[0].(map[string]any)["name"] != "ssh" {
			t.Fatalf("topic help=%v", got)
		}
		if got := object["limits"].(map[string]any); got["cpu"] != float64(4) || got["memory"] != "8GiB" || got["max_instances"] != float64(3) {
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
	for _, line := range []string{"help", "whoami --json", "ls --json", "ls", "new", "resume work", "rm work --json", "rm work", "bogus --json"} {
		f := &fakeBackend{instances: []core.Instance{{Name: "work"}}}
		var stderr bytes.Buffer
		status := (Handler{Backend: f}).Handle(context.Background(), principal, line, core.Streams{Out: brokenWriter{}, Err: &stderr})
		if status == 0 || !strings.Contains(stderr.String(), "closed pipe") {
			t.Fatalf("%s: status=%d stderr=%q", line, status, stderr.String())
		}
	}
}
