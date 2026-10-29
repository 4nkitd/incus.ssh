# Changelog

Notable changes to incus.ssh.

## v0.3.0 - 2026-10-29

#### Added

- `image list [--json]`, a read-only catalog of predefined native Incus Linux container images cached by the operator. Text output shows the preferred alias or short fingerprint, fingerprint prefix and OS release; JSON includes full fingerprints, sorted aliases and optional metadata. VM images are excluded.
- `new --image=REF`, selecting one operator-cached image per container. `REF` is an alias from `image list` or a 64-character fingerprint, optionally prefixed with `local:`. Omitting it keeps the configured default image.
- Per-create image resolution that pins the immutable cached fingerprint before `incus launch`; a later alias change does not affect existing containers.
- A separate `setup --install-incus [--apply]` dependency-only mode for a fresh host. The plan reads OS release and file metadata without running commands or changing files. Package installation supports Ubuntu 24.04 and Debian 12/13 when configured repositories provide the packages; existing complete installations can use read-only verification on other distributions. Combining install mode with preparation flags is rejected; run default `setup` separately.
- Apply updates APT lists first, then runs a bounded simulation with `--no-remove` before installing packages. `--no-remove` is the primary removal guard and blocks package removals. It installs or upgrades the fixed set `incus`, `incus-client`, `ca-certificates` and `dnsmasq-base` plus required dependencies, without a full system upgrade. It does not run `incus admin init`, initialise storage or networking, prepare a project or profile, configure a public listener, pool or SSH, start the gateway, or add third-party repositories, snap or LXC packages.
- Apply skips APT when trusted client and daemon binaries exist and the daemon's `/1.0` metadata is reachable; package presence alone is not the test, and a missing helper such as `dnsmasq-base` is not detected or repaired on such a host. A stopped or unreachable daemon fails without APT. Maintainer scripts can start units, restart dnsmasq, and change subuid/subgid or sysctl files, so plan a maintenance window and do not treat install as daemon readiness. The package environment ignores `http_proxy`, `https_proxy` and `APT_CONFIG`; configure an APT proxy in `apt.conf.d`.
- Local operator mode `incus-for-devs incus`. `incus-for-devs incus --help` exits 0 without root, and `sudo incus-for-devs incus -- ARGS` forwards to native Incus with full local administrator authority. It is never exposed as an SSH management command.
- Documentation for the predefined-image workflow, the operator image approval boundary and the VM-like single-container example.

#### Changed

- `ls --json` container summaries include `image`, the immutable base-image fingerprint recorded by Incus, when available. `ls -l` text output keeps its six columns.
- `new --json` includes the pinned `image` fingerprint when `--image` was requested.
- Cached native images are documented as ordinary unprivileged Incus containers: the same project, profile, SSH path, ownership checks, namespace root mapping and CPU, memory, process and disk limits apply. The lifecycle is persistent in the container sense: the root filesystem survives stop and start, while processes do not. It is not a VM snapshot.

#### Security

- Clients cannot pull, copy, import, alias or delete images, and no registry credentials are used. The image cache remains an operator and root responsibility.
- The gateway never forwards a client-supplied image string to Incus. Only a fingerprint resolved from the cached catalog is passed to launch.
- Image selection does not add a privileged fallback, Docker or OCI daemon, nesting, KVM or a real-VM path. Containers still share the host kernel.
- The local `incus-for-devs incus` operator mode has full administrator authority. It is local-only and never exposed as an SSH management command.
- The v0.3.0 installer is host maintenance. It installs the fixed set `incus`, `incus-client`, `ca-certificates` and `dnsmasq-base`; `ca-certificates` supplies CA trust roots for HTTPS image copies, and `dnsmasq-base` supplies the DHCP/DNS helper needed by a managed bridge. APT is skipped only when trusted client and daemon binaries exist and the daemon's `/1.0` metadata is reachable; package presence alone is not enough, a missing helper is not repaired on a complete host, and a stopped or unreachable daemon fails before APT. Package maintainer scripts can start Incus units, restart dnsmasq, or change subuid/subgid or sysctl files, though a `policy-rc.d` of 101 can block service start. The package environment ignores `http_proxy`, `https_proxy` and `APT_CONFIG`; configure an APT proxy through `apt.conf.d`. Plan a maintenance window; a completed install is not daemon readiness.
- Images without `/bin/sh` can be launched but guest commands fail; recovery requires the operator to cache a suitable image, not to raise gateway privileges.

See [TESTING.md](TESTING.md) for the v0.3.0 verification record, including real Incus, installer, unit, website and remaining-limit notes.

## v0.2.0 - 2026-10-28

#### Added

- `start`, `stop` and `restart` management commands. `resume` becomes an alias of `start`.
- `--processes=` on `new`, with a default of 256 and an operator-configured maximum.
- Fruit-based container names: bare fruit names first, suffixed names such as `kiwi-2` only on collision. `--name=` still takes the name exactly as supplied.
- `setup` subcommand for local Incus preparation: plan only by default, explicit root `--apply`, cached local image alias or fingerprint only, strict typed YAML templates, and bridge-only networks. It creates missing project and profile resources, verifies the result and never downloads images.
- Host memory admission that keeps a 1GiB reserve, accounts for committed project memory and revalidates stored limits on start and restart.
- Interactive terminal color: cyan prompt and help, green running containers, amber stopped, and red plain-text diagnostics. JSON, piped and guest program output stay plain; `TERM=dumb` or empty and nonempty `NO_COLOR` in the server process disable color.
- Session-only prompt history (Up/Down, at most 100 entries, never persisted) and display clearing with `clear` or Ctrl-L; guest keystrokes pass through. `clear` requires a PTY with a known `TERM`, takes no `--json` (the usual JSON error convention remains), and is the eleventh management command.
- CHANGELOG.md.

#### Changed

- Container defaults are now 1 CPU, 2GiB memory and 256 processes. Configured CPU, memory and process values are per-container maxima rather than fixed ceilings applied to every container.
- The example configuration and project template use 2 CPU, 4GiB memory and 512 processes as maxima. The profile keeps 1 CPU, 2GiB and 256 processes as defaults.
- `setup` reuses matching existing project and profile resources, fails closed on a mismatch and never overwrites or deletes them.
- `setup` requires `features.storage.buckets=false` for container-only projects and rejects OVN networks, accepting only a managed bridge.
- `ls -l` now has six tab-separated columns: name, status, CPU, memory, processes and disk. The disk field moved from column 5 to column 6.
- JSON instance summaries include an optional `processes` string.
- An explicitly empty guest command, `ssh NAME ''`, is a no-op instead of opening an interactive shell. An absent command still opens a shell.
- Restarting an active guest validates operator ceilings without charging its resident memory a second time. Starting a stopped guest still requires capacity admission.
- Documentation covers resource admission at create, start and restart, naming and lifecycle commands.

#### Security

- Setup validates templates and flags as typed values with exact matching, and verifies that a cached local image exists before reporting success.
- Memory admission fails closed when host availability cannot be read.
