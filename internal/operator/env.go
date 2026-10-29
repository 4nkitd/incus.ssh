package operator

import (
	"regexp"
	"strings"
)

const (
	minimalPath = "/usr/sbin:/usr/bin:/sbin:/bin"
	rootHome    = "/root"
)

var termPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._-]{0,63}$`)

// buildEnv removes loader and Incus/LXD client overrides from the root-run
// native CLI. HOME is pinned to root's home so image configuration credentials
// stay in the administrator context, PATH stays minimal, and TERM is validated
// before it reaches the terminal-aware Incus output code.
func buildEnv(environ []string) []string {
	values := map[string]string{}
	order := make([]string, 0, len(environ))
	set := func(name, value string) {
		if _, ok := values[name]; !ok {
			order = append(order, name)
		}
		values[name] = value
	}
	term := ""
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		switch name {
		case "HOME", "PATH":
			continue
		case "TERM":
			term = value
			continue
		}
		if strippedEnvironment(name) {
			continue
		}
		set(name, value)
	}
	set("HOME", rootHome)
	set("PATH", minimalPath)
	if !termPattern.MatchString(term) {
		term = "dumb"
	}
	set("TERM", term)
	env := make([]string, 0, len(order))
	for _, name := range order {
		env = append(env, name+"="+values[name])
	}
	return env
}

func strippedEnvironment(name string) bool {
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"LD_", "DYLD_", "INCUS_", "LXD_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}
