# Management command contract

`Handler` implements `core.Handler`. It authenticates no one itself. The transport supplies an authenticated `core.Principal`; the backend enforces ownership, project selection, server defaults and resource ceilings. `Handler.Limits` describes configured limits in JSON help. The parser never substitutes those ceilings for defaults.

Management words use the pinned `github.com/google/shlex` parser. Supported commands are `help`, `?`, `whoami`, `new`, `ls`, `rm`, `start`, `stop`, `restart`, `resume` and `ssh`. `help all` and `help COMMAND` work. Help lists actual options, wraps at 78 columns with no tabs or banner, and stays readable on a narrow terminal. Value flags require `--flag=value`. Boolean flags can appear before or after management positional arguments. Duplicate flags, duplicate deletion names, unknown flags, extra arguments and invalid names fail before any backend operation. `--` is not a management option. Clients cannot pass arbitrary Incus flags, projects, images, profiles or mounts.

Names are 1-63 lowercase ASCII characters, start with a letter, end with a letter or digit, and otherwise contain letters, digits or hyphens. `ls` accepts one bounded shell-style glob over names, including `*`, `?` and bracket classes. The gateway applies that filter to the backend's owned list. CPU and processes must be positive 31-bit decimal integers. Memory and disk accept positive 63-bit decimal quantities with required `B`, `kB`, `KB`, `MB`, `GB`, `TB`, `KiB`, `MiB`, `GiB` or `TiB` suffixes. The backend must reject quantities exceeding its limits, including quantities whose unit multiplication would overflow.

Omitted resources are not the operator maxima. They default to 1 CPU, 2GiB memory and 256 processes, each capped down by the configured per-container maximum. A request may use up to that maximum. Creation also requires host memory to remain above a 1GiB reserve, so an oversized request fails explicitly instead of overcommitting the host. These are container limits; the handler makes no scheduler or VM-isolation claim. Administrator setup is a local host command (`incus-for-devs setup`), never a gateway command.

Lifecycle semantics: `start` and its alias `resume` start a stopped container, `stop` stops it gracefully and `restart` stops then starts it. All three preserve the container's data; running processes do not survive `stop` or `restart`. `rm` permanently removes the named containers and their data, never asks for confirmation, and reports partial failures after attempting every valid name.

Lines must be valid UTF-8, at most 16 KiB, and contain no Unicode control characters except tabs. Management argument count is limited to 256. Newlines, carriage returns and NUL are rejected, including inside SSH commands. Empty command lines fail with a help hint.

## SSH shell boundary

Only the command word and instance name are parsed as management words for `ssh`. Everything after the separating spaces or tabs is the remote command string. It goes to `Backend.Exec` unchanged, with this one explicit convenience convention: if the entire nonblank tail is one single-quoted or double-quoted argument, the parser removes that outer quoting layer. This supports an incoming exec string such as `ssh vm 'printf "%s" "a b"'`. For double-quoted envelopes, only shell-special escapes for `$`, backtick, `"` and `\` lose their backslash. Other backslashes remain.

An explicitly empty envelope, `ssh vm ''` or `ssh vm ""`, becomes the container-shell no-op `:`. That keeps "empty command" distinct from "no command": an absent tail still passes an empty string and opens an interactive login shell. The no-op runs through the shell like any other command, so it never opens an interactive shell and never reads the session's stdin.

The envelope convention is necessarily ambiguous with a shell command consisting only of one quoted word. A caller that needs those quotes interpreted by the container shell can use an explicit remote `sh -c` invocation. The transport should pass the SSH exec request's raw string, never reconstruct it from unquoted argument tokens. OpenSSH itself often removes a local quoting layer before sending that string; the gateway cannot recover local argv boundaries that were never transmitted.

Examples of incoming exec strings and the exact `Exec` command:

| Incoming string | Command inside container |
| --- | --- |
| `ssh vm` | empty string, meaning interactive shell |
| `ssh vm ''` | `:`, the shell no-op; stdin is not read |
| `ssh vm printf '%s' 'a b'` | `printf '%s' 'a b'` |
| `ssh vm 'printf "%s" "a b"'` | `printf "%s" "a b"` |
| `ssh vm echo "$HOME" && id \| cat` | `echo "$HOME" && id \| cat` |
| `ssh vm tool --json --image=anything` | `tool --json --image=anything` |
| `ssh vm --json` | `--json`, a remote command, not JSON mode |

No host shell runs here. Shell expansion, pipelines, redirections, command substitution and remote syntax errors belong to the container shell. An unbalanced quote in a raw remote command is passed through for that shell to diagnose. Interactive and command execution receive the original context, principal and all stream/PTY/resize/signal fields. `Exec` exit status is returned unchanged; a backend error returns status 1 and writes to stderr.

## Output

JSON responses are one object plus a newline on stdout. Errors contain an `error` string and return a nonzero status. A complete management `--json` token enables JSON errors, including invalid encoding, controls and overlong lines, if it can be identified within the first 16 KiB before malformed quoting. An unquoted `#` at the start of a word starts a comment, so a commented-out `--json` never enables JSON. Otherwise errors use stderr. SSH never enables management JSON mode from its remote tail.

When the SSH session has a PTY, management text, help, JSON and diagnostics use CRLF line endings so terminals return to column zero. Guest `ssh NAME` output is written by the backend and is never rewritten.

* `ls`: `{"vms":[]}` when empty. Entries contain `vm_name` and `status`, with `cpu`, `memory`, `processes` and `disk` strings only when the backend supplies them. No domains, regions or access destinations are fabricated.
* `new`: the same instance-summary object directly, including `processes` when the backend reports it. Missing resource flags remain zero or empty in `CreateOptions` so the backend chooses defaults.
* `start`, `stop`, `restart`, `resume`: `{"vm_name":"NAME"}` after successful backend completion. They do not invent a newly fetched state.
* `rm`: `{"removed":["NAME"]}` on success. Partial failure includes successful names, `errors` keyed by failed name, and a top-level `error`. Every requested valid name is attempted; any failure returns status 1. Empty `removed` is always `[]`.
* `whoami`: the supplied application `name` and `project` only.
* `help`: `program`, `description`, `commands` with `name`, `usage` and `description`, `unsupported`, and configured `limits`. Topic help uses the same shape with one command. `limits` are per-container maxima, not defaults.

Non-JSON diagnostics go to stderr. Successful text listings use tab-separated fields; `ls -l` prints `name`, `status`, `cpu`, `memory`, `processes` and `disk`, leaving unknown values empty in their column. Ownership failures come from the backend and are never treated as successful removals.

`pause` explicitly explains that Incus containers cannot promise exe.dev RAM-to-disk suspend. It never calls stop or freeze. Other unsupported commands return errors and never call the backend, including `ssh-key list` because the handler has no key-listing context.
