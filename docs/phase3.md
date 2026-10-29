# v0.4.0: explicit HTTPS sharing and file transfer

Included in v0.4.0. SCP/SFTP and native guest behavior passed verification on a disposable Linux host. HTTPS sharing is experimental: loopback-relay tests passed, but the public hosted API/ACME service and long-lived certificate renewal are not yet verified. Deployment and public exposure always require deliberate operator/user action. See [the verification record](../TESTING.md).

## Publish one machine port

Every machine and every port starts private. Enabling the operator capability does not publish anything. Only an authenticated user's explicit `share enable NAME PORT` publishes that exact owned container and port.

```sh
ssh -p 2222 you@GATEWAY share enable demo 3000
ssh -p 2222 you@GATEWAY share list
ssh -p 2222 you@GATEWAY share list demo --json
ssh -p 2222 you@GATEWAY share disable demo 3000
```

`enable` prints a public HTTPS URL after the relay accepts the route. Ports 3000 and 8080 on the same machine need separate enable commands and get different URLs. There are no wildcards, port ranges, global publishing flags or client-supplied host addresses. `disable` closes the route and its active connections; disabling an already-private pair succeeds.

Opt-ins exist only in the gateway process. A gateway restart starts private again. Gateway lifecycle commands revoke all shares for the selected machine before the operation and block concurrent enables until it finishes. Starting a machine does not publish it, including when `start` is called on an already-running machine. Approval is bound to the Incus UUID and its trusted last-start timestamp. A recreated machine, guest reboot or shutdown/start cycle cannot inherit old consent. Every new target connection rechecks ownership, running state and that boot identity. Out-of-band lifecycle changes are not continuously monitored, but a changed boot fails the connection check and revokes the stale route.

Sharing requires one managed bridge NIC with a verifiable private IPv4 DHCP/static lease. The target comes from the Incus lease matching that NIC's MAC, not from an address reported by guest software. Bridge gateway addresses and host interface addresses are rejected. IPv6-only, unmanaged and ambiguous NIC/lease configurations fail closed. The guest service must listen on its container NIC, not only on guest loopback.

### Operator configuration

Sharing is unavailable unless the operator adds this optional object to the gateway configuration:

```json
{
  "sharing": {
    "api": "https://opentunnel.xyz",
    "state_directory": "/var/lib/incus-for-devs/opentunnel"
  }
}
```

This is a configuration fragment, not a complete replacement file. Omit `sharing` to leave the capability unavailable. The API must be an HTTPS origin. The existing private SSH listener and Incus project's proxy-device restrictions do not change. No host port or guest SSH listener is opened.

The first valid enable creates an OpenTunnel identity for the authenticated user/project and requests its certificate. Identity data survives restarts; exposure approvals do not. State uses private directories, 0600 files, atomic replacements and an exclusive process lock. Protect and back up the token and private key. They are never returned in command output. The directory must be writable by the gateway account and allowed by its systemd sandbox.

The client implements the WebSocket `opentunnel` protocol in Go. It generates a P-256 key and CSR locally, resumes pending certificate issuance, refreshes renewed certificates, reconnects with backoff, checks TLS SNI against the exact enabled route and forwards decrypted bytes to the approved guest port. TLS terminates in the gateway process, not at the relay. The relay sees tunnel and connection metadata, but does not receive the TLS private key. There is no Node, Bun, Rust or external tunnel executable at runtime.

Current fixed limits are 16 published machine-port pairs per gateway, 32 visitor connections per bridge and 32 KiB data frames. Slow connections have bounded queues and are reset rather than buffering indefinitely. PROXY protocol headers and dynamic route editing are not implemented. HTTP payloads and forwarding headers are not rewritten.

### Public means public

The URL is not authentication. Anyone who obtains it can contact the service. Require authentication in the application. Do not expose databases, administrative listeners or an application's local-only endpoints unless you intend to make them public.

The application sees a connection from the gateway, not the visitor. Visitor-controlled `Host`, `Forwarded` and `X-Forwarded-*` values remain visitor-controlled. Never trust those values or the gateway's peer address to bypass authentication. See the upstream [trust boundary](https://github.com/anomalyco/opentunnel/blob/20aacfabbf0049c6ba026109adcaae716d1b3a5d/docs/trust-boundary.md).

## Copy files with stock SCP

Modern OpenSSH `scp` uses SFTP. The first remote path component selects a container owned by the authenticated app user. Everything after it is a path inside that container.

```sh
scp -P 2222 ./main.go you@GATEWAY:demo/root/main.go
scp -P 2222 you@GATEWAY:demo/root/main.go ./main.go
scp -P 2222 -r ./project you@GATEWAY:demo/root/
sftp -P 2222 you@GATEWAY
```

In SFTP, `/` is a virtual list of your running containers. `/demo/root/main.go` means `/root/main.go` in `demo`, never on the host. Transfers run as guest root, like the existing guest shell. They do not enable public sharing and work without configuring OpenTunnel.

Each container needs an SFTP server executable. On the supported Ubuntu/Debian images, install it inside that container as guest root:

```sh
ssh -p 2222 you@GATEWAY ssh demo 'apt-get update && apt-get install -y openssh-sftp-server'
```

This changes the selected guest's packages. It is not run automatically by the gateway. An operator can also include the package in an approved native image. No guest SSH daemon, password, private-key copy, nesting or project privilege change is needed. The gateway starts the SFTP executable through `incus exec` after verifying ownership and running state.

Uploads, downloads, directory traversal, recursive copies, random-access file IO, stat, basic attributes and same-container renames are supported. Native SFTP request attributes and handle-based metadata operations pass through to the guest. Guest paths are not cleaned on the host before guest symlink resolution. Data streams through the gateway without host filesystem staging. A session can access at most four containers and 64 file/directory handles. Packet buffers are capped at 256 KiB; guest directory listings are not retained in host memory. Cross-container rename, readlink/link operations and append-mode opens are refused. Legacy `scp -O`, rsync and arbitrary SSH subsystems remain unsupported. Use the normal SCP command without `-O`.

## Verification and rollout gates

Local tests use a real SSH transport, stock OpenSSH SCP, in-memory guest filesystems and a loopback OpenTunnel relay with real TLS handshakes. A separate approved Linux run also passed against a native Incus guest, including real file permissions, symlink resolution, persistence and boot-bound HTTPS revocation. These do not prove that the hosted relay is ready.

Before activation:

1. Review the completed local and disposable Linux checks in [TESTING.md](../TESTING.md).
2. Obtain approval for hosted OpenTunnel API/certificate verification and any intentional public test service. Real hosted certificate renewal remains unverified.
3. Obtain separate approval for gateway deployment/restart and user-driven public exposure. Preserve administrative SSH, Tailscale, existing containers and their data.

No cloud resource, live public route, guest package install or service restart is authorized by the local implementation alone.
