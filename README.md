# incus.ssh

SSH into a development container without opening the host. A small Go gateway authenticates a public key, maps it to an app user and a restricted Incus project, then exposes a narrow management interface and a guest shell.

The project is **incus.ssh**; the CLI binary and service are named `incus-for-devs`.

**Version 0.2.0.** [Download release archives and checksums](https://github.com/4nkitd/incus.ssh/releases/tag/v0.2.0) for Linux amd64 and macOS arm64, or build from source. Requires Go 1.26+ to build; verified with Go 1.27.3 and Incus 6.0 on Ubuntu 24.04. See [TESTING.md](TESTING.md) for the verification record and remaining limits.

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

## What it is

- One Go binary with server and client modes
- Public-key SSH, with app identities separate from host accounts
- Containers created from an operator-owned image, profile and project
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

## Prepare Incus

Setup prepares the restricted project and fixed profile only:

```sh
./bin/incus-for-devs setup --templates deploy
sudo ./bin/incus-for-devs setup --templates deploy --apply
```

Run these from an extracted Linux release archive, which contains `bin/incus-for-devs` and `deploy/`, or a source checkout with a Linux build. An installed binary still needs `--templates` to point to the deployment templates.

The default image is the cached local alias `ifd-ubuntu-24-04`. `--image` also accepts a `local:` alias or a 64-character fingerprint. Remote references such as `images:ubuntu/24.04` are rejected; setup never downloads or copies an image. `--config` reads only the image field from a gateway configuration and overrides `--image`.

The default is a **plan**: it validates the Incus daemon, the existing pool, the managed bridge and the cached image, and changes nothing. It does not parse the templates; template errors appear at apply. The account running it needs access to the local Incus socket. `--apply` requires root, validates the templates as strict typed YAML, and creates only missing project and profile resources. Existing resources that match are left unchanged; any mismatch fails closed instead of overwriting or deleting. Setup never creates pools or networks, installs packages, writes systemd or SSH configuration, or downloads images. When run as root it requires a root-owned, non-writable Incus CLI at an absolute path.

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

## Not in Phase 1

`pause` is unsupported: Incus containers cannot promise RAM-to-disk suspend. Freeze keeps RAM allocated and stop loses process state, so neither is a substitute.

SFTP/SCP/rsync, web previews, sharing, direct per-container hostnames, Docker or nested containers, and OpenTunnel-style exposure are not part of Phase 1. They are not quietly approximated and are not promises for later phases.

## Security

- Username and a registered public key must both match. No passwords.
- Phase 1 admits exactly one app user. Multiuser use needs global admission limits and a different network review.
- Clients cannot select projects, profiles, images, host mounts or raw Incus flags.
- Unprivileged containers share the host kernel. Guest root is namespace root.
- The daemon's Incus socket access is effectively host-root authority. Never give that socket or `incus-admin` to a client or guest.
- Listener validation accepts only loopback or Tailscale addresses on unprivileged ports.
- A directory storage pool may have no enforced disk quota. Missing disk output does not mean disk is bounded.

Read [SECURITY.md](SECURITY.md) before reporting a vulnerability.

## License

[MIT](LICENSE). The project is not affiliated with Incus or exe.dev.
