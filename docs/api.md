# REST, MCP and Caddy setup

REST and HTTP MCP are included in v0.5.0. Install the matching gateway binary before using these commands. Do not run a new setup CLI against an older service binary.

The API manages the same owned Incus containers as SSH. These are not hardware VMs. REST and MCP share one bearer token, the configured user/project, backend ownership checks and resource limits. The token grants container creation/deletion and guest-root command execution. Sharing controls, file transfer and host administration remain outside the API.

## Set up public HTTPS

On a standard Ubuntu 24.04 gateway installation, point a DNS hostname such as `api.example.com` to the server. Any published AAAA record must also reach this server. Allow inbound TCP 80 and 443. Caddy obtains and renews public certificates automatically. The app does not change DNS or firewall rules.

Preview without changing anything:

```sh
sudo /usr/local/bin/incus-for-devs setup-api --domain api.example.com
```

During an approved maintenance window:

```sh
sudo /usr/local/bin/incus-for-devs setup-api --domain api.example.com --apply
```

`--apply` installs the distribution's Caddy package, writes its HTTPS configuration, backs up the gateway config to `/etc/incus-for-devs/config.json.before-api`, and restarts the gateway. That restart disconnects SSH sessions and revokes active guest shares. Containers remain running.

Setup refuses existing Caddy files/services, an existing API block, a previous backup, or occupied ports 80, 443 and 8080. It never merges an existing proxy configuration or replaces another server. Package startup is masked until the generated configuration passes `caddy validate`. Caddy's admin API and access logging are disabled. Subsequent Caddy configuration changes require an operator-approved service restart, not `caddy reload`.

The automatic path assumes the standard service, paths and service account from the installer. If setup fails after installing packages or writing files, inspect the error and current service/configuration state before recovery. Partial installations are intentionally not overwritten on retry. The config backup is a recovery copy, not an automatic rollback. If gateway restart fails, Caddy is left stopped. Never restore a backup or restart an active gateway without a maintenance window.

Service startup does not prove certificate issuance. Verify the unauthenticated public endpoint before generating a token:

```sh
curl --fail-with-body --include https://api.example.com/api/v1/vms
```

Expect **HTTP 401** with `valid API bearer token required` and a valid certificate. `curl --fail-with-body` returns exit status 22 for this expected 401. A TLS error, redirect, 502 or unrelated page means setup is not verified. Do not use `--insecure`. Inspect `journalctl -u caddy` and `journalctl -u incus-for-devs` when necessary.

## Enable access and generate a token

Connect through the existing authenticated SSH gateway. At `incus>`:

```text
api enable
api status
api rotate
api disable
```

`enable` generates a cryptographically random 256-bit token and shows it once, along with the REST and MCP URLs. Store it in your credential manager. Only its SHA-256 hash is saved in the private API state directory. A gateway restart preserves the enabled state and hash. `status` never returns the token. `enable` refuses to replace an existing token; use `rotate` if it is lost or compromised.

`rotate` replaces the shared token for all clients. `disable` revokes it. Both cancel active requests authenticated with the old token and prevent new requests. Cancellation cannot undo completed mutations or guarantee termination of detached guest processes. These credential commands are available only over authenticated SSH, never through REST or MCP. They also accept `--json`; treat their output as a secret when generating a token.

Disabling access leaves Caddy and the loopback listener running, returning 401. It does not uninstall HTTPS, stop the gateway or alter guest containers.

## REST

Every request requires `Authorization: Bearer TOKEN`. Use HTTPS directly. Never place the token in a URL, browser cookie, command history, repository, screenshot or log. The API rejects query parameters and does not enable cross-origin browser access.

Example using a token already supplied securely in `IFD_TOKEN`:

```sh
curl --fail-with-body https://api.example.com/api/v1/vms \
  --header "Authorization: Bearer $IFD_TOKEN"

curl --fail-with-body https://api.example.com/api/v1/vms \
  --header "Authorization: Bearer $IFD_TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"name":"demo","cpu":1,"memory":"2GiB"}'

curl --fail-with-body https://api.example.com/api/v1/vms/demo/exec \
  --header "Authorization: Bearer $IFD_TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"command":"uname -a"}'
```

Shell-expanded headers can be visible to local process inspection. Use a protected client credential store or curl configuration file where that matters. Do not enable curl tracing with a live token.

| Method | Path | Input / response |
| --- | --- | --- |
| GET | `/api/v1/vms` | `{"vms":[{"name":"demo","status":"Running"}]}` |
| GET | `/api/v1/images` | `{"images":[...]}` |
| POST | `/api/v1/vms` | Optional `name`, `image`, `cpu`, `memory`, `processes`, `disk`. Returns 201 and `{"vm":{...}}`. |
| POST | `/api/v1/vms/NAME/start` | No body. Returns `{"name":"NAME","action":"start"}`. |
| POST | `/api/v1/vms/NAME/stop` | No body. Graceful stop, preserves data. |
| POST | `/api/v1/vms/NAME/restart` | No body. Interrupts guest processes, preserves data. |
| DELETE | `/api/v1/vms/NAME` | No body. Permanently deletes the container and its data, without another confirmation. |
| POST | `/api/v1/vms/NAME/exec` | Required `command`, optional `stdin`. Returns `exit_code`, `stdout`, `stderr`, `truncated`. |

Execution is noninteractive, without a PTY. Commands run through the guest shell, never a host shell. A nonzero guest exit code is a successful HTTP response with that `exit_code`, not an HTTP error. Output is captured after completion, capped at 1 MiB per stream. Additional output is discarded and `truncated` becomes true. Timeouts/errors do not return partial captured output.

Requests are limited to 64 KiB, commands to 16 KiB, stdin to 32 KiB. The operator's `operation_timeout_seconds` also bounds API execution. At most `max_sessions` API HTTP requests run concurrently, separate from SSH's session limit. Excess API concurrency returns 429 with `Retry-After: 1`. Unknown fields and invalid inputs return 400, oversized bodies 413, backend failures 502, timeouts 504, and canceled operations 503. Error responses have an `error` string and omit backend stderr. No mutation is retried automatically; inspect machine state before retrying after a lost response.

## MCP

Use a client supporting HTTP MCP with a custom bearer header:

- Endpoint: `https://api.example.com/mcp`
- Transport: Streamable HTTP, stateless JSON responses.
- Header: `Authorization: Bearer TOKEN`, injected from your client's secret store.

The official Go MCP SDK handles protocol negotiation, initialization and discovery. There is no stdio transport, legacy SSE endpoint, OAuth login or interactive terminal. Clients that require OAuth rather than a configured bearer header need an external adapter.

Tools: `list_vms`, `list_images`, `create_vm`, `start_vm`, `stop_vm`, `restart_vm`, `delete_vm`, `exec_vm`. Lifecycle tools take `name`. `exec_vm` takes `name`, `command` and optional `stdin`. `create_vm` accepts the REST creation fields. Tools use the same backend operations, limits and sharing revocation as REST. Tool descriptions warn about destructive actions, but descriptions are not an authorization or confirmation mechanism. Configure your agent to ask before deletion, stopping services or destructive shell commands.

## Existing Caddy or another reverse proxy

Automatic setup deliberately refuses existing proxy installations. An operator can instead merge the following API block into the gateway config, retaining all existing settings:

```json
{
  "api": {
    "listen": "127.0.0.1:8080",
    "origin": "https://api.example.com",
    "state_directory": "/var/lib/incus-for-devs/api"
  }
}
```

The state directory is created with mode 0700 by the gateway service user. It must be inside a service-writable path. The HTTP listener accepts only loopback addresses, never public, wildcard or Tailscale addresses. The origin must be an HTTPS DNS hostname without a port or path.

Merge this site into your existing Caddy configuration using your normal validation/deployment process:

```caddyfile
api.example.com {
    @management path /api/v1/* /mcp
    handle @management {
        reverse_proxy 127.0.0.1:8080
    }
    handle {
        respond "Not found" 404
    }
}
```

Preserve the public Host header, Authorization and MCP headers. Do not buffer or retry mutation requests, cache responses, rewrite the MCP path, log Authorization/request bodies, or publish the plaintext loopback port. The gateway checks the exact configured Host and rejects other Origins. There is no automatic token grant when the operator configures HTTPS; the user still runs `api enable` over SSH.

## Verification status

Local tests exercise real HTTP requests and an official MCP client against a fake backend, including token persistence, revocation, deadlines, request validation and output limits. Existing Incus tests cover its ownership/resource checks. These do not verify package installation, systemd operations, ACME certificate issuance or real Incus lifecycle/execution through public HTTPS. Those require an approved Linux-host acceptance test before release.
