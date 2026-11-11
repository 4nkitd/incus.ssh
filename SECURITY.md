# Security policy

## Supported versions

Version 0.5.0 is the current release. It adds bearer-authenticated REST/MCP, the hosting UI and native OCI application support. Release builds use Go 1.27.4 or newer, fixing standard-library advisory `GO-2026-6859`. Public hosted OpenTunnel API/ACME behavior and long-lived renewal are not yet verified. Security fixes target the current `main` branch.

## Reporting a vulnerability

Do not open a public issue for a vulnerability, exposed credential or a working exploit.

Report it privately to the maintainer through the contact method on the GitHub profile [@4nkitd](https://github.com/4nkitd), or open a minimal issue asking for a private channel without technical details.

Include:

- Affected version or commit
- A description of the issue and its impact
- Reproduction steps or a proof of concept, if safe to share
- Any suggested mitigation

Expect an acknowledgement within a few days. Please allow time for a fix before public disclosure.

## Scope

This project is a single-user SSH gateway for trusted development containers. The following are documented limitations, not vulnerabilities by themselves:

- Containers share the host kernel; this is not VM isolation
- A configured app user is trusted and can run commands inside their own containers
- The service account has effective host-root authority through the Incus socket by design
- Creating or starting a stopped instance checks host availability with a 1GiB reserve; an active restart does not charge its resident memory again. Admission is not a reservation or swap guarantee
- Start and restart revalidate the instance's stored CPU, memory and process limits against current operator maxima
- CPU maxima are per-container caps; the gateway does not schedule or fairly share CPU time
- Running-only admission is opt-in and enforced by one gateway process per project. It sums active containers' configured memory limits, with a 256-MiB minimum reservation, within the operator's running-memory budget. Only a confirmed stopped state releases capacity; frozen and uncertain states remain charged. Administrative Incus commands and other gateway processes bypass that admission lock
- Native Incus aggregate inventory quotas count stopped instances too. Running-only deployments must remove those conflicting quotas deliberately; per-instance cgroup limits, project restrictions and `limits.virtual-machines=0` remain. This is not a host-global allocator or hostile-tenant quota boundary
- Directory storage may not enforce disk quotas
- Reboot recovery is not verified
- `pause`, nested Docker management, Docker Compose, direct host-port forwarding and multiuser access are unsupported. OCI images run directly through Incus, not through a host or nested Docker daemon

Reports that abuse the trust boundary, escape project restrictions, cross app-user ownership, inject host-shell commands, bypass authentication or leak secrets are in scope. Reports about unsupported features are not vulnerabilities unless they bypass a documented failure and widen access.

## File-transfer and sharing boundaries

HTTPS sharing must require an authenticated user's explicit enable for each owned container and individual port. A public URL is not authentication. Service payloads and visitor-supplied forwarding headers are passed unchanged; applications must not use the gateway's peer address or those headers to bypass authentication. Host interface targets, wildcard publication and inherited approval on a recreated machine are outside the allowed boundary.

SCP/SFTP paths select an owned guest through their first component. Neither a path nor a symlink may select the host filesystem or another app user's guest. Transfers have the guest-root authority already available through the guest shell. The gateway must not run client-supplied paths in a host shell or stage file data in the host filesystem.

See [the feature guide](docs/phase3.md) for exact supported behavior and remaining verification gates. A release never authorizes automatic upgrades, service restarts or public exposure on an existing installation.

## API, UI and OCI boundaries

Bearer tokens grant lifecycle and guest-root execution within the configured project. Tokens are generated over SSH, stored only as hashes on the gateway, and kept only in browser tab memory by the UI. Activity history excludes tokens, environment values and command text. Environment values are stored in Incus configuration and remain readable by host administrators.

Clients may select images only from operator-approved HTTPS OCI registries. Registry definitions are not accepted from API clients. The configured image publishers must be trusted; image imports consume host storage. OCI images in the shared cache cannot bypass this workflow through native cached-image selection.

Hosted apps must use a different origin from the management panel. Publishing binds an explicit port to an owned container's immutable boot identity and verified private bridge lease. App traffic never selects an arbitrary host address. Management-token revocation does not unpublish previously published apps; unpublish or stop them explicitly.
