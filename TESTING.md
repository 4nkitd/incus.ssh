# Verification records

## v0.4.0 release preparation

The source and onboarding checks below establish the tested behavior for v0.4.0. Release archives are built with an embedded version on Linux amd64, verified against an explicit file allowlist, and accompanied by SHA-256 checksums. The standalone installer is pinned after archive creation so its checksum does not form a circular dependency with an archive containing itself. Final artifact/publication checks are recorded in the GitHub release notes and checksum asset.

SCP/SFTP is included in this version and requires a guest SFTP server. HTTPS sharing is included as an experimental, optional capability. Public hosted OpenTunnel API/ACME and long-lived renewal remain unverified; the loopback/native checks below do not imply hosted-service or production activation.

## Onboarding and website pre-release checkpoint

October 29, 2026. A second approved disposable Ubuntu 24.04.5 amd64 host used 2 CPU, 4GB RAM and 40GB disk.

- The bootstrap downloaded the published v0.3.0 Linux archive over HTTPS and verified its pinned SHA-256 before execution. A successful run started with no Incus binary, no gateway configuration and no Incus state; dependency packages installed during an earlier failed attempt remained after the disposable-host reset.
- The staged installer was piped from a local `file://` URL because its public GitHub raw URL is not published yet. That transport difference is not a public-URL end-to-end check. The actual release binary download used GitHub HTTPS.
- Native setup completed storage, bridge, cached Ubuntu image, restricted project/profile, service account, config, host key and service start. Poisoned `INCUS_SOCKET`, `INCUS_DIR` and `INCUS_PROJECT` environment values did not redirect operations.
- The presented SSH host key matched the fingerprint returned by the pinned Go key generator. An initial check using OpenSSH against the Go PKCS#8 private-key encoding failed; the final implementation no longer passes the private key to OpenSSH.
- Authenticated stock OpenSSH login returned the configured `dev` identity and empty owned inventory. A second installer run preserved the same service PID and configuration checksum.
- The terminal GIF records actual native container creation, entry into its root shell, Linux commands and file creation. A separate stop/start check verified the recorded file persisted. No terminal output was fabricated. The GIF is 40,926 bytes at 842x510, with a static reduced-motion poster and text transcript.
- All six Node site tests passed, covering links/assets, feature anchors, local fonts, the GIF/poster, exact copy behavior and responsive guide navigation. Twelve dedicated-CDP browser checks passed for the image-enabled landing page and feature guide at 1440, 390 and 320 pixels in system light/dark modes, without whole-page horizontal overflow. Feature anchors scrolled into view, mobile closing-section gutters remained intact, copy returned the exact command, and reduced motion disabled animation and selected the static PNG. No app-origin browser exceptions were recorded.
- Installer code reviews and native verification passed. Website review fixes corrected an incomplete automation example and mobile footer gutters.
- Demo assets and logs were retained locally, no private test key was retrieved, and the disposable VM was deleted. Its absence from provider inventory was verified. No production gateway or public hosted tunnel changed.

Public publication/deployment and the public installer URL remain separate approval gates. The local preview is not a production activation.

## Phase 3 local pre-release checkpoint

October 29, 2026, macOS arm64, Go 1.27.3. These results are local fixtures, not live Incus or hosted-OpenTunnel verification.

- `go test -race ./integration ./internal/filetransfer ./internal/command ./internal/sharing ./internal/opentunnel` passed. The SSH transport is real; guest filesystems are in-memory fixtures and the relay is a loopback HTTP/WebSocket server.
- Stock OpenSSH `scp`, with pinned generated host/user keys, passed upload, download and recursive copies through the local gateway. Binary bytes and filenames with spaces and shell metacharacters survived. Host-path reads, foreign-container access and cross-container rename were denied.
- Native Incus argument fixtures for service identity, lease validation, recreation and container-only SFTP process startup passed under the race detector. These do not substitute for running against the Linux daemon.
- Local TLS visitor handshakes and binary round trips through the Go bridge passed. Unenabled routes, deeper names and mismatched inner TLS SNI were refused before target dialing. Reconnect, heartbeat, private/resumable identity storage, API creation retry safety, expired-identity attach recovery and cancellation tests passed.
- Manager fixtures passed private-by-default behavior, separate consent/URLs for two ports on one machine, independent disable, foreign-user isolation and shutdown/recreation behavior.
- `go vet ./...` and `go mod verify` passed. `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` found zero reachable vulnerabilities and zero affected imported packages, with one advisory in an unused required module. The older installed scanner could not parse the current Go version and was not used for this result.
- The full local race suite did **not** pass: `TestPTYStreamsOutputAndPreservesExitCode` hangs on this Mac. The same targeted test hung in a separate clean export of untouched `HEAD`, confirming that it predates Phase 3. Linux root/non-root regression checks remain required.

Runtime review passed after fixes for consent/lifecycle races, post-dial boot verification, API retry/cancellation behavior and SFTP protocol/metadata handling. Repeated scoped race runs and the `NO_COLOR=1` transport suite passed. The broad suite passed only with the documented existing PTY/process tests excluded; this is not a full-suite pass. SFTP wire tests cover malformed attributes, exact OPEN permissions, native handle metadata, inner-path preservation and a shared file/directory handle budget.

The next checkpoint uses a separately approved disposable Linux VM for real Incus and guest-only package tests. No hosted tunnel was created, no production binary/configuration was changed and no production gateway service was restarted for the local checkpoint. See [ROADMAP.md](ROADMAP.md) and [docs/phase3.md](docs/phase3.md).

## Phase 3 native Linux pre-release checkpoint

October 29, 2026. A separately approved disposable Ubuntu 24.04.5 amd64 host used 2 CPU, 4GB RAM and 40GB disk, with Go 1.27.3 and Incus 6.0.0. No production server changes or hosted tunnel were authorized.

- Full `go test -race ./...` passed as root without exclusions, then as an unprivileged user with `NO_COLOR=1`. The Mac PTY baseline hang did not reproduce on Linux.
- `go vet ./...` and `go mod verify` passed.
- An initial Linux run found valid TLS bursts exhausting the old eight-frame receive queue. A 256KiB byte-bounded buffer replaced it, with fragmented-frame and EOF regressions. OpenTunnel race tests then passed 30 consecutive runs on both macOS and Linux.
- Opt-in `TestNativeIncusSCPAndPrivateRelay` passed against a real native Ubuntu 24.04 container in a disposable restricted project. The guest used 1 CPU, 512MiB RAM and a 256-process limit. Only the disposable guest received `openssh-sftp-server` and `busybox-static`.
- Stock SCP uploaded and downloaded binary files with spaces/quotes in their names, preserved `0600` and `0700` permissions with `-p`, and copied directories recursively. Native SFTP kept symlink-plus-`..` resolution inside the selected guest and denied host-path and foreign-owner access.
- Real TLS visitor traffic crossed a loopback-only synthetic OpenTunnel relay and reached the native guest's HTTP service. Repeated visitors preserved the same bridge. Explicit disable stopped access. An out-of-band native guest restart changed its trusted boot identity, blocked the old approval and revoked the stale route. Copied data survived restart.
- The final QA project inventory was empty after owned guest cleanup.
- Tested Linux binary SHA-256: `e348f6bd43c2a04b087c81c9e586cf9e0f4ecb6e54797184b6fc5b61d783e47e`. This is an unreleased candidate, not an activated production binary.

Not verified: the public hosted OpenTunnel API/ACME service, long-lived real certificate renewal, an actual production gateway restart, or production file transfers. These remain separate approval gates. QA results and the checksum-verified candidate binary are retained privately. The disposable VM was deleted after retrieval, and its absence from the provider inventory was verified.

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
