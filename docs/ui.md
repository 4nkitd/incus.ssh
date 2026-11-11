# Hosting panel at `/ui`

The hosting panel is included in v0.5.0. Incus 6.0 LTS can manage existing system containers but does not support native OCI application containers. A daemon upgrade needs a separate maintenance plan and approval; the gateway does not perform it automatically.

Build with Go 1.27.4 or a newer patched toolchain. The module selects 1.27.4 by default because Go 1.27.3 is affected by the HTTP/1 `MaxBytesReader` header-race crash vulnerability [GO-2026-6859](https://pkg.go.dev/vuln/GO-2026-6859). A new source checkout does not patch an already-running binary.

## What the panel does

- Compact service inventory with search, state filtering and configured resource limits.
- A separate workspace for each service: runtime readings, environment, networking, logs, guest commands and activity.
- Three-step deployment: image, configuration, review. Enter a Docker Hub reference or an HTTPS OCI registry URL. Quick picks are suggestions, not a per-image allowlist.
- Environment variable names are visible; saved values are not returned. Changes do not restart a service automatically.
- Runtime CPU, memory, root-disk, network and process readings come from Incus. CPU percentages require two samples. Graphs cover samples collected while that page is open, not historical monitoring. Running is not a health check.
- Searchable console logs with opt-in five-second refresh. Commands are bounded, noninteractive guest-shell requests, not a browser terminal.
- A private-by-default HTTP publishing workflow. Published apps use a different origin from the management panel. A new container boot requires publishing again.
- The latest 200 panel, REST and MCP actions are retained in a private activity file. Commands, environment values and bearer tokens are not recorded. SSH and out-of-band Incus actions are not included. This is not an immutable compliance audit log.
- Success, error and warning toasts. Success toasts auto-dismiss; errors stay until dismissed. Warnings stay until dismissed or their underlying condition clears. Repeated state warnings are suppressed. Notifications pause while a modal is open and return when it closes, rather than covering its controls. Form validation and persistent state indicators remain visible in context.

Pico CSS 2.1.1 supplies the base controls and is bundled locally with its MIT license. The panel uses IBM Plex fonts and bundled SVG action icons. Light mode is the default; the theme preference can be saved in browser storage. The bearer token cannot: it stays only in tab memory and is cleared on refresh or sign-out.

## Built-in development boxes

| Option | Image | Contents |
| --- | --- | --- |
| Kali Linux | `kalilinux/kali-rolling:latest` | Minimal Kali rolling base, not the complete security toolkit |
| Alpine Linux | `alpine:latest` | Small Linux environment and `apk` |
| Go dev box | `golang:latest` | Official Go compiler/tooling image |
| PHP dev box | `php:cli` | PHP CLI; Composer is not preinstalled |
| Python dev box | `python:3` | Python 3 and pip |

These options select development-box mode, which replaces the image startup command with `/bin/sh -c "exec sleep infinity"` **inside the guest**. This keeps shell-oriented images running. Other compatible images can also use this mode; they must contain `/bin/sh` and `sleep`. Ordinary application mode preserves the image's own startup command.

Development boxes require Incus's `instance_oci_entrypoint` extension. OCI app shells preserve the image environment instead of running login profiles that could remove toolchain paths. Native Linux-container shell behavior is unchanged.

The tags above are moving references. They do not pin versions or update existing containers automatically. Use a `sha256` digest for reproducible image selection. Container files persist across stop/start; deletion removes them. There is no external-volume, image-rebuild, backup or rollback workflow in this version.

## Operator setup

Use an Incus release with native OCI support and the required image conversion tools. Native verification used Incus 7.0.2 LTS. Keep the existing restricted project and unprivileged profile. Do not enable nesting, privileged containers, host mounts or the host Docker socket for this feature.

Configure registry remotes under the **gateway service user's** Incus client configuration, not only root's configuration. For a standard installation:

```sh
sudo runuser -u incus-for-devs -- env HOME=/var/lib/incus-for-devs \
  /usr/bin/incus remote add docker https://docker.io --protocol=oci

sudo runuser -u incus-for-devs -- env HOME=/var/lib/incus-for-devs \
  /usr/bin/incus remote add github https://ghcr.io --protocol=oci
```

Merge these settings into the existing gateway config:

```json
{
  "oci_remote": "docker",
  "oci_registries": {"ghcr.io": "github"},
  "oci_images": ["nginx:alpine", "httpd:alpine"]
}
```

`oci_remote` enables Docker Hub. `oci_registries` maps approved HTTPS registry hosts to operator-configured Incus remote names. `oci_images` is an optional quick-pick list. Users can select any repository/tag/digest on the configured registries, including:

```text
nginx:alpine
kalilinux/kali-rolling
ghcr.io/team/app:v1
https://ghcr.io/team/app:v1
registry.example.com:5000/team/app:tag
nginx:alpine@sha256:FULL_64_CHARACTER_DIGEST
```

Custom registry hosts must match the operator mapping. The remote must be a public OCI remote at that exact HTTPS origin, without a credential helper. Images requiring registry authentication are not supported yet. Raw IPs, credentials in URLs, plain HTTP, arbitrary archive downloads and client-supplied Incus remote definitions are rejected.

Image publishers and configured registry hosts must be trusted. Containers share the host kernel; this is not hostile-tenant isolation. Image pulls consume host storage and populate the Incus cache. Operators remain responsible for image/cache retention, storage capacity, guest egress policy and backups.

### Running-only resource budgets

To make the container count dynamic within an active-memory budget, set:

```json
{
  "limits": {
    "max_instances": 0,
    "running_memory": "8GiB",
    "cpu": 2,
    "memory": "4GiB",
    "processes": 512
  }
}
```

This sums configured memory limits for active containers, not their instantaneous RSS. Confirmed stopped containers keep their files but reserve no running capacity. Starting, stopping, frozen and uncertain states remain charged. Each active container reserves at least 256 MiB for admission, even if its own memory cap is smaller. Creates and starts check both the running budget and host available memory with a 1-GiB reserve. An already-active restart is not charged twice. Per-container CPU and PID limits remain enforced; CPU is not reserved or scheduled globally.

New admissions are refused with a retry message until outstanding Incus task operations in the project settle. A cancelled client can leave a daemon-side start running even while the container is reported as stopped. Long-lived shell/console WebSocket operations do not block admission. Stops, deletion and read-only inspection remain available for recovery.

Running-only creates initialize a stopped container before starting it. A failed or cancelled request may therefore leave a stopped container that the user can inspect, start or delete. Starts, restarts and unfreezes use a private, durable `admission/PROJECT.json` journal alongside the configured SSH host key. The service account needs write access to that directory. Include it in gateway backups and preserve it across upgrades.

Client disconnection does not abandon an admitted start. The gateway keeps its mutation lock until Incus completes or the operation deadline expires. If Incus acknowledged the operation, the next admission reconciles its recorded operation ID. If no acknowledgement arrived, new starts remain blocked across gateway restarts. An empty operation list alone is not enough to clear that journal: the original request could still be registering its operation.

For an unacknowledged start, stop the gateway and investigate the named instance and daemon operations. Do not delete the journal merely to retry. If the outcome cannot be proved, arrange a maintenance window to restart Incus, verify that no old request or operation can still run, and inspect instance states and configured memory limits. Archive the reconciled project journal outside `admission/`, then restart the gateway. Keep the Incus database, guest data and other project journals intact. Stops and read-only inspection remain available through Incus while gateway admission is blocked.

Incus's own `limits.instances`, `limits.containers`, `limits.memory`, `limits.cpu` and `limits.processes` project quotas count the complete configured inventory, including stopped containers. An operator must deliberately remove those conflicting aggregate quotas for running-only mode. Keep `restricted=true`, the existing security restrictions and `limits.virtual-machines=0`. Do not remove unrelated disk or storage restrictions.

Use one gateway process per project. Direct administrative Incus commands bypass gateway admission, so this is a trusted-developer resource policy, not hostile-tenant isolation or a host-global allocator. Stopped containers and cached images still consume disk space. Legacy configurations with a positive `max_instances` and no `running_memory` retain their inventory-based limits for compatibility.

### Expose the control panel

Keep the existing HTTPS/API configuration and add `/ui` and `/ui/*` to its Caddy management matcher:

```caddyfile
@management path /api/v1/* /mcp /ui /ui/*
handle @management {
    reverse_proxy 127.0.0.1:8080
}
```

The fresh-host `setup-api` generator includes these paths. Do not run that installer against an existing Caddy installation. Merge and validate the existing configuration during an approved activation window.

The public static login page contains no user data or embedded credentials. All management requests still require the generated bearer token. The browser checks the configured origin and uses a restrictive CSP. Do not insert the token into a URL or disable certificate verification.

### Optional app hostnames

For HTTP publishing, configure a separate suffix in the existing `api` block:

```json
{
  "applications_domain": "apps.example.com"
}
```

Provision DNS for the suffix and add a matching Caddy route to the same loopback backend:

```caddyfile
https://*.apps.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Certificate configuration is operator work. A public wildcard certificate generally needs a DNS challenge integration. Private/Tailscale deployments can use Caddy's internal CA, with explicit client trust, and suitable private DNS. Do not assume Tailscale creates wildcard DNS names or certificates. Preserve existing bind addresses, routes and TLS policy.

An app named `website` can then explicitly publish an HTTP port at `https://website.apps.example.com`. The gateway resolves only its owned container and verifies its current boot identity and private bridge lease before dialing. Requests are never routed to the management API because they use an application hostname. This proxy does not expose arbitrary host IPs or ports.

App traffic is bounded to 8 MiB request bodies and the configured request timeout. WebSocket upgrades, raw TCP publishing and custom per-app domain aliases are not supported in this version. Disabling the API token revokes management access, not already-published app access; unpublish or stop an app to remove that access.

## API additions

All management routes use the existing bearer authentication:

| Method | Route | Purpose |
| --- | --- | --- |
| GET | `/api/v1/system` | Hosting capabilities, approved registry hosts, quick picks, presets and limits |
| POST | `/api/v1/apps` | Deploy `name`, `image`, optional `cpu`, `memory`, `disk`, `environment` and `development_box` |
| GET | `/api/v1/apps/NAME/details` | Runtime counters, limits, environment names and pending-restart state |
| PATCH | `/api/v1/apps/NAME/environment` | `{"values":{"NAME":"new value","REMOVE_ME":null}}`; no automatic restart |
| GET | `/api/v1/apps/NAME/logs` | Last bounded console output |
| POST | `/api/v1/apps/NAME/publish` | `{"port":80}`; port zero unpublishes |
| GET | `/api/v1/events` | Bounded recent API activity and persistence status |

Existing `/vms` lifecycle/execution routes remain unchanged. The native `/images` catalog excludes OCI images; cached OCI images must use the app workflow instead of bypassing it through native container creation.

MCP adds `hosting_status`, `create_app`, `app_logs`, `publish_app`, `app_details` and `set_app_environment` when the backend supports them. These use the same ownership and configuration boundaries as REST.

## Preview and tests

```sh
node scripts/ui-preview.mjs
```

This binds only to loopback at `http://127.0.0.1:8270/ui` and uses the public fixture token `preview-token`. Its sample-data notice is intentional. It does not connect to a real backend. `PORT` can select a different local port; `OCI_UNAVAILABLE=1` exercises the unsupported-server state.

Browser checks use the approved `agent-browser`/Helium workflow, never install Chrome, and close only their test tab/session:

```sh
UI_PREVIEW_URL=http://localhost:8270/ui node scripts/ui-browser-check.mjs
UI_PREVIEW_URL=http://localhost:8270/ui node scripts/ui-race-check.mjs
```

Native tests require an explicitly prepared disposable project/profile and registry remotes. They create and delete only test guests:

```sh
IFD_NATIVE_HOSTING_PROJECT=ifd-ui \
IFD_NATIVE_HOSTING_CUSTOM_IMAGE=https://ghcr.io/stefanprodan/podinfo:6.17.0 \
IFD_NATIVE_DEVELOPMENT_BOXES=1 \
go test -race -timeout 1000s -run '^TestNative(OCIHosting|DevelopmentBoxes)$' ./integration
```

## Design references

The service workspace, configuration separation and troubleshooting controls were informed by [Coolify's interface](https://coolify.io/docs/screenshots), [Dokploy's application workspace](https://docs.dokploy.com/docs/core/interface-overview) and [Portainer's container workflows](https://docs.portainer.io/user/docker/containers/view). Density and hierarchy favor compact Shopify, Cloudflare and Stripe-style administrative interfaces. No reference screenshots or branding are shipped with the UI.
