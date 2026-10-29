# Verification records

## v0.3.0 verification

Recorded on October 29, 2026. Results distinguish real Incus/VM checks from unit and fake-backend fixtures.

### Environment

- Clean Ubuntu 24.04.5 VM: 2 CPU, 4GB RAM, 15GB disk; initially no Incus, daemon, CA trust store or dnsmasq.
- Go 1.27.3; Incus 6.0.0.
- The minimal test VM lacked `/dev/stdin`, `/dev/stdout` and `/dev/stderr` symlinks. They were restored as a test fixture for nft behavior; this is not installer behavior or a product requirement.
- The disposable VM was destroyed after the checks, and its absence was verified.

### Installer (REAL)

- Fresh `setup --install-incus --apply` performed apt update, a bounded simulation with `--no-remove`, then installed the requested set `incus`, `incus-client`, `ca-certificates` and `dnsmasq-base` plus distribution dependencies. No removals or upgrades were observed on the clean host.
- `policy-rc.d` 101 left the daemon not started, and the installer reported not verified rather than ready.
- Re-running with a stopped daemon failed without APT. After the operator started the daemon separately, the no-op path detected trusted binaries and ready `/1.0` metadata and skipped APT.
- Separate `setup` created the restricted project and profile; a repeat reported both already matching.

### Runtime images (REAL)

- The operator created a directory pool and managed bridge with no added packages, then copied Ubuntu 24.04 and Debian 12 over HTTPS using the installed CA trust.
- Real SSH probes on both images passed alias, `local:ALIAS` and fingerprint selection. Omitted-default creation was checked on Ubuntu only. Each passed new creation, `ls --json` fingerprint, six-column `ls -l`, guest OS identity, 512MiB and 128-process cgroup limits, file persistence across stop/start/restart, owned-only cleanup and empty final inventory.
- A 1-CPU request was not separately verified against guest CPU visibility, so it is not claimed.

### Security and API (REAL + UNIT)

- Real SSH with a fake backend covered authentication, ownership, cache-mutation refusal and invalid image references; unit tests assert strict CLI argument vectors. The real QA listener rejected raw `incus`, `image pull` and remote image references.
- Ambiguous native images, VM filtering, malicious metadata and alias-reassignment races were unit fixtures only, not real cache-mutation E2E.

### Automated checks

- `go test -race` passed for the full suite as root and non-root with `NO_COLOR`.
- `go vet` and `go mod verify` passed.
- `govulncheck` 1.9.0 found zero reachable vulnerabilities and zero affected imported packages; one advisory was in an unused required module.
- Independent reviews after fixes found no blocker.

### Builds and website

- Linux amd64 and macOS arm64 archives passed SHA-256 and explicit content-allowlist checks. Extracted binaries ran `version` and operator help on their native platforms.
- Helium at 1440, 390 and 320 pixels on both pages passed anchors and focusable-table checks, with no overflow, zero external assets and zero runtime exceptions. Screenshots were manually reviewed.

### Not claimed

- Fresh installer run on Debian, APT proxy deployment, host reboot recovery and hostile-tenant isolation.
- Real-cache E2E for image ambiguity, VM filtering, malicious metadata or alias-reassignment races; those remain unit fixtures.
- Docker or OCI daemons, nesting, real virtual machines, privileged fallback or KVM-less VM support.

## v0.2.0 verification

Recorded on October 28, 2026. These results are a release check, not continuous health monitoring or a security guarantee.

### Environment

- Linux amd64, Ubuntu 24.04, Incus 6.0.
- Go 1.27.3. The module requires Go 1.26 or newer; this release was not separately tested with every supported compiler version.
- A dedicated restricted Incus test project, fixed Ubuntu image and managed bridge. Existing user containers were excluded from lifecycle tests.
- The test project used maxima of 2 CPUs, 4 GiB RAM and 512 processes, with one container at a time.

### Automated checks

```sh
go mod verify
go test -race -count=1 -timeout 180s ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

The complete test suite, race detector, module checks and vet passed. The vulnerability scan found zero reachable vulnerabilities and zero affected imported packages; it identified one advisory in an unused OpenPGP package from a required module.

Unit tests use a fake Incus CLI. SSH integration tests use real loopback SSH connections with temporary keys and a fake backend. Neither is a substitute for running against Incus.

### Real Incus checks

- First `setup --apply` created and verified a restricted project and profile using separate atomic create operations. The pair is not transactional; partial failures are reported and can be retried. A repeat run reported both already matching without changing them.
- Generated fruit name and explicit-name creation, listing and cleanup.
- Defaults of 1 CPU, 2 GiB and 256 processes.
- Explicit 2 CPU, 4 GiB and 512-process requests, verified against guest cgroup limits and CPU visibility.
- Requests above configured CPU/process maxima and the instance cap were rejected.
- Binary stdin/stdout round-trip, including NUL and non-UTF-8 bytes, with guest exit status 37.
- Stop/start, stop/resume and restart, with filesystem markers retained.
- Unsupported `pause` rejected. Test cleanup removed only the disposable containers and checked the final inventory.

### Terminal controls

Real SSH checks covered colored prompts/help, Up/Down command history, restoring an in-progress draft, `clear` and Ctrl-L. Separate regression tests verify that JSON, non-PTY output and guest byte streams are not decorated; command history is bounded and session-local. Server-side `NO_COLOR` disables colors without disabling Ctrl-L.

Independent reviews covered setup validation and partial failure, SSH transport, ownership, capacity admission, lifecycle semantics and release archive contents. Review findings were fixed and regression-tested before the release.

### Website

The landing page and documentation were rendered in Helium at 1440, 390 and 320-pixel widths. Checks covered local navigation, documentation deep-link selection, keyboard-focusable table regions, page overflow and runtime exceptions. No external assets were loaded. This is not a complete accessibility audit.

### Remaining limits

- Containers share the host kernel; this is not VM isolation or hostile-tenant containment.
- RAM admission is conservative and project-scoped. It is not a global resource scheduler or a swap policy.
- Disk quotas depend on the actual storage backend and require separate operator verification.
- A cancelled lifecycle request can leave the Incus operation running until its own graceful deadline; cancellation is not reported as confirmed completion.
- Host reboot recovery and crash/backup restoration were not tested.
- Docker nesting, OpenTunnel, SFTP/SCP, direct guest hostnames and public previews are not included in this release.
