package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"incus-for-devs/internal/command"
	"incus-for-devs/internal/core"
)

type clientContractBackend struct {
	core.Backend
	calls     int
	principal core.Principal
	name      string
	remote    string
	streams   core.Streams
	options   core.CreateOptions
	exec      func(core.Streams) (int, error)
}

func (b *clientContractBackend) Exec(_ context.Context, p core.Principal, name, remote string, streams core.Streams) (int, error) {
	b.calls++
	b.principal, b.name, b.remote, b.streams = p, name, remote, streams
	if b.exec != nil {
		return b.exec(streams)
	}
	return 0, nil
}

func (b *clientContractBackend) Create(_ context.Context, p core.Principal, options core.CreateOptions) (core.Instance, error) {
	b.calls++
	b.principal, b.options = p, options
	return core.Instance{Name: options.Name, Status: "Running"}, nil
}

func (b *clientContractBackend) List(_ context.Context, p core.Principal) ([]core.Instance, error) {
	b.calls++
	b.principal = p
	return []core.Instance{{Name: "work-one", Status: "Running"}, {Name: "other", Status: "Stopped"}}, nil
}

func clientContractLine(t *testing.T, args []string) string {
	t.Helper()
	sshArgs, err := clientArgs(append([]string{"--target", "alice@gateway", "--port", "2222"}, args...), "")
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range sshArgs {
		if arg == "--" {
			if len(sshArgs[i+1:]) != 2 || sshArgs[i+1] != "alice@gateway" {
				t.Fatalf("expected one SSH command payload after destination: %q", sshArgs)
			}
			for _, option := range sshArgs[:i] {
				if option == "-t" || option == "-tt" || option == "-n" || option == "StdinNull=yes" {
					t.Fatalf("noninteractive command changes PTY/stdin contract: %q", sshArgs)
				}
			}
			return sshArgs[i+2]
		}
	}
	t.Fatalf("missing SSH destination separator: %q", sshArgs)
	return ""
}

func TestClientHandlerRawShellContract(t *testing.T) {
	for _, remote := range []string{
		`printf '%s' "it's a quoted string"; printf "%s" 'double "quotes"'`,
		`printf '%s' "$HOME" && printf '%s' 'a;b | $(id)' | cat > result; exit 7`,
		`sh -c 'printf "%s" "a b"'`,
		`tool --json --project=default --image=anything`,
		`printf  '%s'  'a b'  `,
	} {
		t.Run(remote, func(t *testing.T) {
			backend := &clientContractBackend{}
			var out, stderr bytes.Buffer
			principal := core.Principal{Name: "alice", Project: "dev-alice"}
			line := clientContractLine(t, []string{"ssh", "work", remote})
			status := (command.Handler{Backend: backend}).Handle(context.Background(), principal, line, core.Streams{Out: &out, Err: &stderr})
			if status != 0 || out.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("status=%d stdout=%q stderr=%q", status, out.String(), stderr.String())
			}
			if backend.calls != 1 || backend.name != "work" || backend.principal != principal || backend.remote != remote {
				t.Fatalf("calls=%d principal=%+v name=%q command=%q, want command=%q", backend.calls, backend.principal, backend.name, backend.remote, remote)
			}
		})
	}
}

func TestClientHandlerArgumentBoundaries(t *testing.T) {
	values := []string{"a b", "", "it's", `double "quotes"`, "a;b | $(id) > file && echo bad", "$HOME", "*", "back\\slash", "\t", "--json", ""}
	args := append([]string{"ssh", "work", "printf", "%s\\000"}, values...)
	backend := &clientContractBackend{}
	var stderr bytes.Buffer
	status := (command.Handler{Backend: backend}).Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, clientContractLine(t, args), core.Streams{Err: &stderr})
	if status != 0 || backend.calls != 1 || backend.name != "work" || stderr.Len() != 0 {
		t.Fatalf("status=%d calls=%d name=%q stderr=%q", status, backend.calls, backend.name, stderr.String())
	}
	output, err := exec.Command("sh", "-c", backend.remote).Output()
	if err != nil {
		t.Fatalf("execute captured guest command %q: %v", backend.remote, err)
	}
	want := strings.Join(values, "\x00") + "\x00"
	if string(output) != want {
		t.Fatalf("argument boundaries changed: output=%q want=%q command=%q", output, want, backend.remote)
	}
}

func TestClientHandlerManagementJSONContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    string
		options core.CreateOptions
	}{
		{"create", []string{"new", "--name=work", "--cpu=2", "--memory=512MiB", "--disk=10GB", "--json"}, `{"vm_name":"work","status":"Running"}`, core.CreateOptions{Name: "work", CPU: 2, Memory: "512MiB", Disk: "10GB"}},
		{"list trailing JSON", []string{"ls", "-l", "work-*", "--json"}, `{"vms":[{"vm_name":"work-one","status":"Running"}]}`, core.CreateOptions{}},
		{"list leading JSON", []string{"ls", "--json", "work-*", "-l"}, `{"vms":[{"vm_name":"work-one","status":"Running"}]}`, core.CreateOptions{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &clientContractBackend{}
			var out, stderr bytes.Buffer
			principal := core.Principal{Name: "alice", Project: "dev-alice"}
			status := (command.Handler{Backend: backend}).Handle(context.Background(), principal, clientContractLine(t, tc.args), core.Streams{Out: &out, Err: &stderr})
			if status != 0 || stderr.Len() != 0 || backend.calls != 1 || backend.principal != principal || backend.options != tc.options {
				t.Fatalf("status=%d stderr=%q backend=%+v", status, stderr.String(), backend)
			}
			var got, want any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("invalid JSON response %q: %v", out.String(), err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("response=%s want=%s", out.String(), tc.want)
			}
		})
	}
}

func TestClientHandlerJSONValidationPreservesArgumentBoundaries(t *testing.T) {
	for _, args := range [][]string{
		{"new", "--name=work --cpu=2", "--json"},
		{"new", "--name=it's invalid", "--json"},
		{"ls", "", "--json"},
		{"ls", "--json", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			backend := &clientContractBackend{}
			var out, stderr bytes.Buffer
			status := (command.Handler{Backend: backend}).Handle(context.Background(), core.Principal{}, clientContractLine(t, args), core.Streams{Out: &out, Err: &stderr})
			var response struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(out.Bytes(), &response); err != nil {
				t.Fatalf("invalid JSON error %q: %v", out.String(), err)
			}
			if status == 0 || backend.calls != 0 || stderr.Len() != 0 || response.Error == "" {
				t.Fatalf("status=%d calls=%d stdout=%q stderr=%q", status, backend.calls, out.String(), stderr.String())
			}
		})
	}
}

func TestClientHandlerStdinStreamContract(t *testing.T) {
	payload := "#!/bin/sh\nprintf '%s' \"it's stdin\"\n\x00binary\xff\n"
	stdin := strings.NewReader(payload)
	var out, stderr bytes.Buffer
	backend := &clientContractBackend{exec: func(streams core.Streams) (int, error) {
		if _, err := io.Copy(streams.Out, streams.In); err != nil {
			return 1, err
		}
		if _, err := io.WriteString(streams.Err, "guest diagnostic\n"); err != nil {
			return 1, err
		}
		return 23, nil
	}}
	line := clientContractLine(t, []string{"ssh", "work", "sh", "-s"})
	streams := core.Streams{In: stdin, Out: &out, Err: &stderr}
	status := (command.Handler{Backend: backend}).Handle(context.Background(), core.Principal{Name: "alice", Project: "dev-alice"}, line, streams)
	if status != 23 || backend.calls != 1 || backend.remote != `'sh' '-s'` {
		t.Fatalf("status=%d calls=%d command=%q", status, backend.calls, backend.remote)
	}
	if backend.streams.In != stdin || backend.streams.Out != &out || backend.streams.Err != &stderr || backend.streams.PTY {
		t.Fatalf("stream forwarding changed: %+v", backend.streams)
	}
	if out.String() != payload || stderr.String() != "guest diagnostic\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
	}
}
