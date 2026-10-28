# incus-for-devs

SSH into a development container without opening the host. A small Go gateway authenticates a public key, maps it to an app user and a restricted Incus project, then exposes a narrow management interface and a guest shell.

Website and docs: **https://4nkitd.github.io/incus-for-devs/**

```sh
ssh -p 2222 you@GATEWAY help
ssh -p 2222 you@GATEWAY new --name=demo
ssh -t -p 2222 you@GATEWAY ssh demo
```

The bare connection opens a management prompt, never a host shell. `ssh demo` enters the container you own. Plain OpenSSH is enough; the compiled client is optional.

## What it is

- One Go binary with server and client modes
- Public-key SSH, with app identities separate from host accounts
- Containers created from an operator-owned image, profile and project
- Fixed CPU, memory and process ceilings; no arbitrary Incus flags from clients
- Management commands for create, list, resume, remove, identity and help
- Interactive guest shells and remote commands with stdin, stderr and exit status
- JSON responses for management commands

It manages trusted development containers. It is not a multiuser platform, a hostile-tenant sandbox, or a virtual-machine boundary.

## What it is not

`pause` is unsupported because Incus containers cannot promise exe.dev-style RAM-to-disk suspend. Incus freeze keeps RAM allocated; stop loses process state. Neither is a substitute.

V1 also has no SFTP/SCP/rsync endpoint, web previews, sharing, direct per-container hostnames, tags, custom images, snapshots or key self-registration. Those requests fail explicitly.

## Install

There is no one-command installer. The operator prepares Incus, a restricted project, a fixed profile, an image, guest network policy and a private SSH listener. See the [documentation](https://4nkitd.github.io/incus-for-devs/docs.html) for the full procedure.

Short version, run on an approved Linux host:

```sh
incus project create ifd-dev
incus project edit ifd-dev < deploy/project.yaml
incus --project ifd-dev profile create ifd
incus --project ifd-dev profile edit ifd < deploy/profile.yaml

install -o root -g root -m 0755 bin/incus-for-devs /usr/local/bin/incus-for-devs
incus-for-devs keygen --path /var/lib/incus-for-devs/host.key
incus-for-devs server --config /etc/incus-for-devs/config.json
```

`config.example.json` is a template. Replace the placeholder public key, set the image, and choose a listener. It is not a working deployment as shipped.

## Build and test

Go 1.26 or newer is required. Build and test on Linux, not on the target host.

```sh
go mod verify
go test -race ./...
go vet ./...
go build -trimpath -o bin/incus-for-devs ./cmd/incus-for-devs
```

Unit and SSH integration tests use temporary keys and a fake backend. Run real lifecycle tests only against a dedicated project and containers you can delete.

## Security model

- Username and a registered public key must both match. No passwords.
- V1 configuration admits exactly one app user. Multiuser use needs global admission limits and a different network review.
- Clients cannot select projects, profiles, images, host mounts or raw Incus flags.
- Unprivileged containers share the host kernel. Guest root is namespace root.
- The daemon's Incus socket access is effectively host-root authority. Never give that socket or `incus-admin` to a client or guest.
- Listener validation accepts only loopback or Tailscale addresses on unprivileged ports.
- A directory storage pool may have no enforced disk quota. Missing disk output does not mean disk is bounded.

Read [SECURITY.md](SECURITY.md) before reporting a vulnerability.

## Status

V1, tested against Incus 6.0 on Ubuntu 24.04. Host reboot recovery, SFTP, file-transfer endpoints, previews and RAM snapshot pause are not implemented or not verified. The project is not affiliated with Incus or exe.dev.

## License

[MIT](LICENSE)
