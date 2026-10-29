# Roadmap and verification gates

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
