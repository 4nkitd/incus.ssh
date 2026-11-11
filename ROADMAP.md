# Roadmap and verification gates

## Delivered in v0.5.0

- Opt-in, shared bearer token generated over SSH; hash-only storage, rotation and revocation.
- REST and official-SDK HTTP MCP for owned container lifecycle and bounded guest execution.
- Integrated `setup-api --domain` Caddy installation/configuration, plan-only by default. `--apply` requires an approved gateway restart and refuses existing proxy installations.
- Local HTTP/MCP contract and security tests use a fake backend. Linux Caddy package/systemd setup, public ACME HTTPS and real Incus API acceptance remain release gates. No live deployment is implied.

## Hosting panel

- Embedded `/ui`, compact service workspaces, light/dark themes, SVG action icons, guided deployment and toast feedback.
- Docker Hub and operator-approved public HTTPS OCI registries. Free-form image selection; quick picks are optional.
- Kali rolling, Alpine, Go, PHP and Python development-box presets with fixed guest keepalive startup.
- Live Incus counters, redacted environment inspection/editing, bounded logs, one-off commands and private persisted API activity.
- Separate-origin HTTP app publishing, explicit consent and boot-bound revocation.
- Native Incus 7.0.2 tests cover Docker Hub/GHCR deployment, environment updates, metrics, logs, publishing and data persistence. All five development-box toolchain probes pass after fixing entrypoint syntax and preserving OCI image PATH.
- Existing Incus 6.0 hosts need a separately approved daemon upgrade for native OCI hosting. No Compose, archive imports, private-registry authentication, backup/volume manager, interactive browser terminal or image rollback is claimed.
- Dynamic running-only admission is opt-in through `limits.running_memory`, with per-instance caps, a minimum reservation and host headroom checks. Stopped inventory does not consume that running budget.

## Delivered in v0.4.0

- Pure-Go OpenTunnel client with local key generation, private identity storage and TLS relay tests.
- Explicit `share enable NAME PORT`, `share disable NAME PORT` and `share list [NAME]`.
- Private-by-default publishing, ownership/boot checks, independent port consent and lifecycle revocation.
- Modern SCP/SFTP with container-in-path addressing and no guest SSH listener.
- Fresh-host bootstrap, real terminal demo and system light/dark documentation site.
- Linux root/non-root race suites and real native Incus file permissions, persistence and loopback-relay checks.

## Remaining verification

- Hosted OpenTunnel API/ACME behavior and long-lived real certificate renewal. HTTPS sharing remains experimental until these are verified.
- Host-reboot recovery. Guest stop/start persistence is not a host-reboot or backup guarantee.
- Broader distribution and architecture coverage beyond the tested release platforms.

The macOS backend PTY subprocess test also hangs on the pre-v0.4.0 baseline. Full Linux suites pass; do not treat filtered macOS results as a full-suite pass. See [TESTING.md](TESTING.md).

## Deployment boundary

Publishing a release does not activate it on an existing server. Upgrades require deliberate backup, installation, service restart and end-to-end verification by the operator. Preserve administrative SSH, Tailscale, existing guests and their data. New installs and new machine-port pairs remain private until explicitly configured and enabled.

Direct host-port mappings, nested Docker/OCI integration, hardware VMs, hostile-tenant isolation and multiuser cloud admission remain outside the current scope.
