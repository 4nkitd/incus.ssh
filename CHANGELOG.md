# Changelog

Notable changes to incus.ssh.

## Unreleased

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
