package app

import (
	"strings"
	"testing"

	"github.com/google/shlex"
)

func TestClientSafeArguments(t *testing.T) {
	args, err := clientArgs([]string{"--target", "boss@100.64.0.10", "--port", "2222", "ssh", "demo", `printf '%s' "a b"; exit 7`}, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"StrictHostKeyChecking=yes", "ForwardAgent=no", "ClearAllForwardings=yes", "-p 2222", "-- boss@100.64.0.10"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s", want)
		}
	}
	want := `'ssh' 'demo' printf '%s' "a b"; exit 7`
	if args[len(args)-1] != want {
		t.Fatalf("%q != %q", args[len(args)-1], want)
	}
	multi, err := clientArgs([]string{"ssh", "demo", "printf", "%s", "a b"}, "ifd")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := shlex.Split(multi[len(multi)-1])
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 5 || decoded[4] != "a b" {
		t.Fatalf("argument boundary lost: %q", decoded)
	}
}

func TestClientTargetValidation(t *testing.T) {
	for _, target := range []string{"", "-oProxyCommand=bad", "a\nb", "a b", "/tmp/host"} {
		if _, err := clientArgs([]string{"--target", target, "ls"}, ""); err == nil {
			t.Errorf("accepted %q", target)
		}
	}
	if _, err := clientArgs([]string{"--port", "-1", "ls"}, "ifd"); err == nil {
		t.Fatal("negative port accepted")
	}
}

func TestClientPTYAndJSON(t *testing.T) {
	for _, command := range [][]string{nil, {"ssh", "demo"}} {
		args, err := clientArgs(command, "ifd")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range args {
			found = found || a == "-t"
		}
		if !found {
			t.Fatal("missing PTY")
		}
	}
	args, err := clientArgs([]string{"ls", "--json"}, "ifd")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range args {
		if a == "-t" {
			t.Fatal("PTY on machine-readable output")
		}
	}
}
