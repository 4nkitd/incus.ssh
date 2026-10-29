# Security policy

## Supported versions

Version 0.4.0 is the current release. It includes owner-scoped SCP/SFTP and experimental, explicit per-port HTTPS sharing. Public hosted OpenTunnel API/ACME behavior and long-lived renewal are not yet verified. Security fixes target the current `main` branch.

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
- Directory storage may not enforce disk quotas
- Reboot recovery is not verified
- `pause`, Docker/nested containers, direct host-port forwarding and multiuser access are unsupported

Reports that abuse the trust boundary, escape project restrictions, cross app-user ownership, inject host-shell commands, bypass authentication or leak secrets are in scope. Reports about unsupported features are not vulnerabilities unless they bypass a documented failure and widen access.

## File-transfer and sharing boundaries

HTTPS sharing must require an authenticated user's explicit enable for each owned container and individual port. A public URL is not authentication. Service payloads and visitor-supplied forwarding headers are passed unchanged; applications must not use the gateway's peer address or those headers to bypass authentication. Host interface targets, wildcard publication and inherited approval on a recreated machine are outside the allowed boundary.

SCP/SFTP paths select an owned guest through their first component. Neither a path nor a symlink may select the host filesystem or another app user's guest. Transfers have the guest-root authority already available through the guest shell. The gateway must not run client-supplied paths in a host shell or stage file data in the host filesystem.

See [the feature guide](docs/phase3.md) for exact supported behavior and remaining verification gates. A release never authorizes automatic upgrades, service restarts or public exposure on an existing installation.
