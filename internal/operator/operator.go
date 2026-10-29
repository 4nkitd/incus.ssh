// Package operator exposes the local root-run Incus CLI.
//
// This is an intentional full-authority administrator interface for the local
// Incus daemon, not a security sandbox and not a remote management or SSH
// exposure path. It only pins the initial local context and a default project
// from the root-owned configuration; the native Incus CLI remains in control of
// everything else.
package operator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"incus-for-devs/internal/host"
)

const defaultIncusBinary = "/usr/bin/incus"

var (
	geteuid           = os.Geteuid
	execProcess       = syscall.Exec
	trustedExecutable = host.TrustedExecutable
	defaultConfigPath = "/etc/incus-for-devs/config.json"
)

type options struct {
	config   string
	explicit bool
	native   []string
	help     bool
}

// Run executes the native Incus CLI. Arguments after -- are passed through
// unchanged. The call replaces the current process on success.
func Run(args []string) error {
	if len(args) == 0 {
		writeHelp()
		return nil
	}
	opts, err := parse(args)
	if err != nil {
		return err
	}
	if opts.help {
		writeHelp()
		return nil
	}
	if geteuid() != 0 {
		return errors.New("the Incus operator interface must run as root; use sudo")
	}
	cfg, err := loadOperatorConfig(opts.config, opts.explicit)
	if err != nil {
		return err
	}
	if err := trustedExecutable(cfg.Binary); err != nil {
		return fmt.Errorf("refusing to run untrusted Incus binary %q: %w", cfg.Binary, err)
	}
	argv := []string{cfg.Binary, "--force-local"}
	if cfg.Project != "" {
		argv = append(argv, "--project", cfg.Project)
	}
	argv = append(argv, opts.native...)
	return execProcess(cfg.Binary, argv, buildEnv(os.Environ()))
}

func parse(args []string) (options, error) {
	opts := options{}
	found := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			opts.native = append([]string(nil), args[i+1:]...)
			found = true
			break
		}
		switch {
		case arg == "-h" || arg == "--help":
			opts.help = true
			return opts, nil
		case arg == "--config":
			if opts.explicit {
				return options{}, errors.New("--config may be specified only once")
			}
			if i+1 >= len(args) {
				return options{}, errors.New("--config requires a path")
			}
			i++
			opts.config = args[i]
			opts.explicit = true
		case strings.HasPrefix(arg, "--config="):
			if opts.explicit {
				return options{}, errors.New("--config may be specified only once")
			}
			opts.config = strings.TrimPrefix(arg, "--config=")
			opts.explicit = true
		case strings.HasPrefix(arg, "-"):
			return options{}, errors.New("native Incus arguments that begin with '-' must follow the -- separator")
		default:
			opts.native = append([]string(nil), args[i:]...)
			found = true
		}
		if found {
			break
		}
	}
	if opts.explicit {
		if opts.config == "" {
			return options{}, errors.New("--config requires a non-empty path")
		}
		if !filepath.IsAbs(opts.config) {
			return options{}, errors.New("--config path must be absolute")
		}
	} else {
		opts.config = defaultConfigPath
	}
	return opts, nil
}

func writeHelp() {
	fmt.Fprint(os.Stdout, helpText)
}

const helpText = `Usage:
  sudo incus-for-devs incus [--config PATH] -- [native Incus arguments...]

Runs the system Incus CLI as root with --force-local and, when an operator
configuration is present, the first configured user's non-default project as
the default project. Arguments after -- are passed through unchanged. This is
local-only: it does not expose remote SSH management.

  --config PATH   operator JSON configuration (default /etc/incus-for-devs/config.json)
  -h, --help      show this help without requiring root

Use -- whenever any native argument begins with "-":
  sudo incus-for-devs incus -- list --format=json
  sudo incus-for-devs incus -- --project other list

Clear the configured project for global commands that reject --project:
  sudo incus-for-devs incus -- --project= query /1.0
  sudo incus-for-devs incus -- --project= list --all-projects

The default configuration is optional only when it does not exist. An explicit
--config must exist and pass root-ownership, regular-file, symlink and
permission checks, including trusted parent directories.

This is a full root administrator interface, not a restricted sandbox. Native
Incus flags can override the configured default project, and remote references
such as image copies from images: remain available. Do not grant this command
through a sudoers argument subset; access to it is equivalent to root.
`
