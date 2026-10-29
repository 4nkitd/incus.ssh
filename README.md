# incus.ssh

SSH into a development container without opening the host. A small Go gateway authenticates a public key, maps it to an app user and a restricted Incus project, then exposes a narrow management interface and a guest shell.

The project is **incus.ssh**; the CLI binary and service are named `incus-for-devs`.

**Released: v0.3.0.** [Download archives and checksums](https://github.com/4nkitd/incus.ssh/releases/tag/v0.3.0) for Linux amd64 and macOS arm64, or build from source with Go 1.26 or newer. Verified with Go 1.27.3 and Incus 6.0.0 on Ubuntu 24.04. See [TESTING.md](TESTING.md) for the verification record and remaining limits.

Website: **https://4nkitd.github.io/incus.ssh/** · [Documentation](https://4nkitd.github.io/incus.ssh/docs.html) · [Source](https://github.com/4nkitd/incus.ssh)

```sh
ssh -p 2222 you@GATEWAY help
ssh -p 2222 you@GATEWAY new --name=demo
ssh -p 2222 you@GATEWAY ls
ssh -p 2222 you@GATEWAY stop demo
ssh -p 2222 you@GATEWAY start demo
ssh -t -p 2222 you@GATEWAY ssh demo
```

The bare connection opens a management prompt, never a host shell. `ssh demo` enters a container you own. Plain OpenSSH is enough; the compiled client is optional.

Interactive management output uses color: a cyan prompt and help, green for running containers, amber for stopped, and red for plain-text diagnostics. JSON, piped output and guest program output are never color-wrapped. Color is off for `TERM=dumb` or an empty `TERM`, and when `NO_COLOR` is nonempty in the gateway server process; the client's local environment is not consulted.

In the management prompt, Up/Down recall at most 100 commands for the current session only; history is never written to disk or logs, and guest keystrokes are not intercepted. `clear` and Ctrl-L clear the display; `clear` needs a PTY and a known, non-`dumb` `TERM`, takes no `--json` (failures still follow the usual JSON convention), and otherwise fails.

## What it is

- One Go binary with server and client modes
- Public-key SSH, with app identities separate from host accounts
- Containers use an operator-approved image, fixed profile and project
- A read-only image catalog and per-create image selection from that cache
- Narrow lifecycle: `new`, `ls`, `ssh`, `start`, `stop`, `restart`, `resume` (alias of `start`) and `rm`
- Generated fruit names, or a name you supply with `--name=`
- Interactive guest shells and remote commands with stdin, stderr and exit status
- JSON responses for management commands

## Defaults and resource limits

New containers default to **1 CPU, 2GiB memory and 256 processes**. The operator's `limits` values in `config.example.json` are per-container maxima, not defaults. A client may request less or, when capacity allows, up to those maxima with `--cpu=`, `--memory=` and `--processes=`.

Memory admission is conservative. Creating or starting a stopped container checks host `MemAvailable`, keeps **1GiB in reserve**, and accounts for memory already committed to the project. Restarting a running container revalidates its stored limits without charging its resident memory a second time. This is an admission check, not a reservation or a guarantee for later.

CPU counts are not arbitrated. The gateway enforces a per-container CPU maximum; it does not schedule CPUs, balance load or stop clients from oversubscribing the host. Treat higher CPU maxima as a trust decision, not isolation.

The host policy is about **RAM, not swap**. The gateway does not configure swap, does not count swap as available memory and does not promise swap-backed overcommit. Size the host for committed memory.

## Names

Without `--name=`, `new` picks a fruit name such as `mango`. Bare fruit names are used first; only when every bare name is taken does it fall back to suffixed names such as `kiwi-2` (suffixes 2-100). A name supplied with `--name=` is used exactly as given, subject to the usual name rules.

## Images

The image catalog is a **cached catalog of predefined native Incus Linux images**, not a registry client. The operator caches an image ahead of time; clients pick from that approved set. `image list [--json]` reports container images already visible to the configured project. Text output gives one image per line: the preferred alias (or a short fingerprint), a 12-character fingerprint prefix, and the OS release. JSON includes the full fingerprint, sorted aliases and optional description, OS, release and architecture. VM images and cache entries without a valid fingerprint are omitted.

```sh
ssh -p 2222 you@GATEWAY image list
ssh -p 2222 you@GATEWAY image list --json
```

`new --image=REF` selects one cached image instead of the configured default. `REF` is an alias from `image list` or a 64-character fingerprint, optionally prefixed with `local:`. Explicit selections reject remote references such as `images:debian/12`, URLs and path traversal. An alias may contain a slash only when the cached image metadata defines that exact alias, for example `debian/12`.

Without `--image`, `new` keeps the operator's configured default unchanged for backward compatibility. An operator-configured remote default can cause Incus to download an image. Use a cached local default to keep all creation offline.

The backend looks the alias up in the cached catalog and passes only the immutable image fingerprint to `incus launch`. A later alias change does not affect an existing container; `ls --json` reports the pinned fingerprint from Incus, never a mutable alias. An alias that is not cached, matches more than one cached image, or resolves to a non-container image fails before creation.

Image caching is an operator approval boundary. A client cannot add images, and the gateway has no command to mutate the cache. Cache and alias images locally as root through the operator mode, for example:

```sh
incus-for-devs incus --help
sudo incus-for-devs incus -- image copy images:debian/12 local: --alias debian-12
```

The restricted project and fixed profile still apply to every image, along with unprivileged execution, the namespace root mapping and the CPU, memory, process and disk limits. These are native Incus containers: they still share the host kernel. The lifecycle is persistent in the container sense: the root filesystem survives `stop` and `start`, while running processes do not. It is not a VM snapshot or a hardware guest.

A VM-like workflow with one container at a time (`limits.max_instances` is `1` in the example configuration):

```sh
ssh -p 2222 you@GATEWAY image list
ssh -p 2222 you@GATEWAY new --image=debian-12 --name=dev
ssh -t -p 2222 you@GATEWAY ssh dev
# inside the container: install packages, keep files across stop and start
```

A predefined native image is still a container, not a hardware virtual machine. There is no Docker daemon, no OCI runtime integration, no nesting and no KVM or real-VM path. Guest commands run through `/bin/sh -lc`; a minimal image without `/bin/sh` can be created but its shell commands will fail. Fix that by caching an image that includes a shell, as the operator, not by granting the gateway or guest more privilege. Arbitrary OCI images, Windows images and real virtual machines are not supported on any host; no KVM or KVM-less VM claim is made.

## Local operator mode

`incus-for-devs incus` is a local application mode for host administration, not an SSH management command. `incus-for-devs incus --help` prints its usage and exits 0 without root. `sudo incus-for-devs incus -- ARGS` runs the native Incus CLI locally with the operator's full administrator authority.

This mode is deliberately unrestricted: it is not a sandbox, it is not exposed through the SSH listener, and no client or guest can invoke it. Treat every invocation as host-root administration. Use it to cache approved images and perform other deliberate host work, not to give a client a general Incus shell.

The configured project is the native CLI's default. Override it with native `--project NAME`, or clear it for global commands that reject a project:

```sh
sudo incus-for-devs incus -- --project= query /1.0
sudo incus-for-devs incus -- --project= list --all-projects
```

## Prepare Incus

Setup has two modes that do not chain.

### Install the Incus dependency

```sh
./bin/incus-for-devs setup --install-incus
sudo ./bin/incus-for-devs setup --install-incus --apply
```

`--install-incus` is a dependency-only stage. Without `--apply` it is a plan: it reads the OS release and file metadata, runs no commands and changes nothing. It cannot check APT candidates or daemon readiness. When packages are needed, an unrecognized distribution fails as unsupported; a recognized distribution with no candidate fails during apply. An existing complete installation can use the read-only verification path on other distributions. Combining install mode with any preparation flag is rejected. Run the default setup separately afterward.

With root `--apply` it updates APT lists first, then runs a bounded simulation with `--no-remove` before installing packages. `--no-remove` is the primary removal guard and blocks package removals. It installs or upgrades the fixed set below plus required dependencies, and does not perform a full system upgrade. It does not run `incus admin init`, initialise storage or networking, prepare a project or profile, configure a public listener, pool or SSH, start the gateway, or add third-party repositories, snap or LXC packages:

- `incus`, `incus-client`: daemon and CLI
- `ca-certificates`: CA trust roots for HTTPS image copies
- `dnsmasq-base`: DHCP/DNS helper required by a managed bridge

Apply skips APT only when the trusted client and daemon binaries exist and the daemon's `/1.0` metadata is reachable. Package presence alone is not the test; a missing helper such as `dnsmasq-base` is not detected or repaired on such a host. If the daemon is stopped or unreachable, apply fails without APT; start or repair it, then retry.

Package maintainer scripts can start Incus units, restart dnsmasq, and change subuid/subgid or sysctl files, so schedule a root maintenance window. Installation is not daemon or service readiness, and the installer does not enable or start the gateway service; start and verify the daemon separately. The package environment ignores `http_proxy`, `https_proxy` and `APT_CONFIG`; configure an APT proxy in `apt.conf.d`.

### Prepare the existing runtime (default mode)

After Incus is installed and the daemon is verified ready, a separate `setup` invocation prepares the restricted project and fixed profile against that existing daemon:

```sh
./bin/incus-for-devs setup --templates deploy
sudo ./bin/incus-for-devs setup --templates deploy --apply
```

Run these from an extracted Linux release archive, which keeps the standard layout of `bin/incus-for-devs` plus `deploy/`, or a source checkout with a Linux build. An installed binary still needs `--templates` to point to the deployment templates.

The default image is the cached local alias `ifd-ubuntu-24-04`. `--image` also accepts a `local:` alias or a 64-character fingerprint. Remote references such as `images:ubuntu/24.04` are rejected; setup never downloads or copies an image. `--config` reads only the image field from a gateway configuration and overrides `--image`.

This runtime mode defaults to a **plan**: it validates the Incus daemon, the existing pool, the managed bridge and the cached image, and changes nothing. It does not parse the templates; template errors appear at apply. The account running it needs access to the local Incus socket. `--apply` requires root, validates the templates as strict typed YAML, and creates only missing project and profile resources. Existing resources that match are left unchanged; any mismatch fails closed instead of overwriting or deleting. This runtime mode never creates pools or networks, installs packages, writes systemd or SSH configuration, or downloads images. When run as root it requires a root-owned, non-writable Incus CLI at an absolute path.

Setup requires `features.storage.volumes=true`, `features.storage.buckets=false`, the restricted container settings and exactly one allowed managed bridge. The shipped project template also sets container-only resource limits. The profile requires fixed security settings plus exactly one root disk and one managed NIC. Network support is bridge-only; OVN is rejected.

`config.example.json` uses 2 CPU, 4GiB memory and 512 processes as operator maxima, matching `deploy/project.yaml`. `deploy/profile.yaml` keeps the 1 CPU, 2GiB, 256-process defaults. The example image is `local:ifd-ubuntu-24-04`. It is a template, not a working deployment; replace the placeholder key and choose a listener.

## Build and test

Go 1.26 or newer. Build and test on Linux, not on the target host.

```sh
go mod verify
go test -race ./...
go vet ./...
go build -trimpath -o bin/incus-for-devs ./cmd/incus-for-devs
```

Do not cross-compile a Darwin binary over `bin/incus-for-devs` and then follow the Linux install steps. Build the server on Linux; give a client build its own output path if you need one. Windows builds are not supported.

## Not included

`pause` is unsupported: Incus containers cannot promise RAM-to-disk suspend. Freeze keeps RAM allocated and stop loses process state, so neither is a substitute.

SFTP/SCP/rsync, web previews, sharing, direct per-container hostnames, Docker or nested containers, and OpenTunnel-style exposure are not included. They are not quietly approximated and are not promises for later phases.

The image catalog does not add arbitrary OCI support, a container registry client, Windows images, real hardware virtual machines, or a privileged fallback for images that expect one. It does not make a container into a VM, and it does not change the shared host kernel. Images are native Incus container images cached by the operator.

## Security

- Username and a registered public key must both match. No passwords.
- The gateway admits exactly one app user. Multiuser use needs global admission limits and a different network review.
- Clients cannot select projects, profiles, host mounts or raw Incus flags. `--image` may only choose from the cached images the operator has approved.
- Unprivileged containers share the host kernel. Guest root is namespace root, whichever cached image the container uses.
- The daemon's Incus socket access is effectively host-root authority. Never give that socket or `incus-admin` to a client or guest.
- Image cache changes are operator work. Clients have no cache-management commands; explicit image selections resolve only cached fingerprints. An operator-configured remote default retains Incus's download behavior.
- Listener validation accepts only loopback or Tailscale addresses on unprivileged ports.
- A directory storage pool may have no enforced disk quota. Missing disk output does not mean disk is bounded.

Read [SECURITY.md](SECURITY.md) before reporting a vulnerability.

## License

[MIT](LICENSE). The project is not affiliated with Incus or exe.dev.
