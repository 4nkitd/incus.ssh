package operator

import (
	"strings"
	"testing"
)

func TestBuildEnvStripsOverridesAndPinsRootContext(t *testing.T) {
	env := envMap(buildEnv([]string{
		"HOME=/home/app",
		"PATH=/tmp/evil",
		"TERM=screen-256color",
		"LD_PRELOAD=/tmp/evil.so",
		"ld_library_path=/tmp/lib",
		"DYLD_INSERT_LIBRARIES=/tmp/dyld",
		"INCUS_REMOTE=attacker",
		"incus_project=attacker",
		"LXD_DIR=/tmp/lxd",
		"lxd_socket=/tmp/socket",
		"KEEP=one",
	}))
	for _, name := range []string{
		"LD_PRELOAD", "ld_library_path", "DYLD_INSERT_LIBRARIES",
		"INCUS_REMOTE", "incus_project", "LXD_DIR", "lxd_socket",
	} {
		if _, ok := env[name]; ok {
			t.Fatalf("%s must be stripped", name)
		}
	}
	if env["HOME"] != rootHome {
		t.Fatalf("HOME = %q", env["HOME"])
	}
	if env["PATH"] != minimalPath {
		t.Fatalf("PATH = %q", env["PATH"])
	}
	if env["TERM"] != "screen-256color" {
		t.Fatalf("TERM = %q", env["TERM"])
	}
	if env["KEEP"] != "one" {
		t.Fatalf("KEEP = %q", env["KEEP"])
	}
}

func TestBuildEnvFallsBackForUnsafeTerm(t *testing.T) {
	for _, term := range []string{"", "bad term", "xterm;rm -rf /", "a\nb", "-bad"} {
		env := envMap(buildEnv([]string{"TERM=" + term}))
		if env["TERM"] != "dumb" {
			t.Fatalf("TERM %q -> %q, want dumb", term, env["TERM"])
		}
	}
}

func TestBuildEnvRejectsDuplicateNamesAndMalformedEntries(t *testing.T) {
	env := buildEnv([]string{"KEEP=one", "KEEP=two", "MALFORMED", "=nothing", "OK=value"})
	values := envMap(env)
	if values["KEEP"] != "two" || values["OK"] != "value" {
		t.Fatalf("unexpected values %#v", values)
	}
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, "KEEP=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("KEEP appeared %d times", count)
	}
}
