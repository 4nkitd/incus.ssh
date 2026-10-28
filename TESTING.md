# v0.2.0 verification

Recorded on October 28, 2026. These results are a release check, not continuous health monitoring or a security guarantee.

## Environment

- Linux amd64, Ubuntu 24.04, Incus 6.0.
- Go 1.27.3. The module requires Go 1.26 or newer; this release was not separately tested with every supported compiler version.
- A dedicated restricted Incus test project, fixed Ubuntu image and managed bridge. Existing user containers were excluded from lifecycle tests.
- The test project used maxima of 2 CPUs, 4 GiB RAM and 512 processes, with one container at a time.

## Automated checks

```sh
go mod verify
go test -race -count=1 -timeout 180s ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

The complete test suite, race detector, module checks and vet passed. The vulnerability scan found zero reachable vulnerabilities and zero affected imported packages; it identified one advisory in an unused OpenPGP package from a required module.

Unit tests use a fake Incus CLI. SSH integration tests use real loopback SSH connections with temporary keys and a fake backend. Neither is a substitute for running against Incus.

## Real Incus checks

- First `setup --apply` created and verified a restricted project and profile using separate atomic create operations. The pair is not transactional; partial failures are reported and can be retried. A repeat run reported both already matching without changing them.
- Generated fruit name and explicit-name creation, listing and cleanup.
- Defaults of 1 CPU, 2 GiB and 256 processes.
- Explicit 2 CPU, 4 GiB and 512-process requests, verified against guest cgroup limits and CPU visibility.
- Requests above configured CPU/process maxima and the instance cap were rejected.
- Binary stdin/stdout round-trip, including NUL and non-UTF-8 bytes, with guest exit status 37.
- Stop/start, stop/resume and restart, with filesystem markers retained.
- Unsupported `pause` rejected. Test cleanup removed only the disposable containers and checked the final inventory.

## Terminal controls

Real SSH checks covered colored prompts/help, Up/Down command history, restoring an in-progress draft, `clear` and Ctrl-L. Separate regression tests verify that JSON, non-PTY output and guest byte streams are not decorated; command history is bounded and session-local. Server-side `NO_COLOR` disables colors without disabling Ctrl-L.

Independent reviews covered setup validation and partial failure, SSH transport, ownership, capacity admission, lifecycle semantics and release archive contents. Review findings were fixed and regression-tested before the release.

## Website

The landing page and documentation were rendered in Helium at 1440, 390 and 320-pixel widths. Checks covered local navigation, documentation deep-link selection, keyboard-focusable table regions, page overflow and runtime exceptions. No external assets were loaded. This is not a complete accessibility audit.

## Remaining limits

- Containers share the host kernel; this is not VM isolation or hostile-tenant containment.
- RAM admission is conservative and project-scoped. It is not a global resource scheduler or a swap policy.
- Disk quotas depend on the actual storage backend and require separate operator verification.
- A cancelled lifecycle request can leave the Incus operation running until its own graceful deadline; cancellation is not reported as confirmed completion.
- Host reboot recovery and crash/backup restoration were not tested.
- Docker nesting, OpenTunnel, SFTP/SCP, direct guest hostnames and public previews are not included in this release.
