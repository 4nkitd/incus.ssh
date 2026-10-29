# incus.ssh

Remote Linux environments. Your server. Just SSH.

## Install

Run on a fresh Ubuntu 24.04 amd64 server:

```sh
curl -fsSL https://raw.githubusercontent.com/4nkitd/incus.ssh/main/install.sh | sudo bash
```

Paste your **public** SSH key and confirm the setup. The script installs Incus and the released gateway, prepares storage, networking and a native Ubuntu image, then starts a private listener. It will not overwrite an existing gateway. [Installation and laptop access](https://4nkitd.github.io/incus.ssh/docs.html#install).

## Use

An [exe.dev](https://exe.dev)-inspired workflow, powered by [Incus](https://linuxcontainers.org/incus/) on hardware you control. Create persistent environments for development, trusted sandboxing and hobby projects. Enter a guest root shell, install packages and keep your files between sessions.

```text
incus> new --name=melon
incus> ssh melon
root@melon:~# uname -s
Linux
```

![Real incus.ssh session creating a native Incus environment, entering its shell and running commands](https://4nkitd.github.io/incus.ssh/assets/demo.gif)

These are VM-like **Linux containers with a shared kernel**, not hardware VMs or a hostile-tenant isolation boundary. You own backups, network policy and uptime.

## Links

[Website](https://4nkitd.github.io/incus.ssh/) · [Documentation](https://4nkitd.github.io/incus.ssh/docs.html) · [Releases](https://github.com/4nkitd/incus.ssh/releases) · [Issues](https://github.com/4nkitd/incus.ssh/issues) · [Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [MIT license](LICENSE)

Current release: [v0.4.0](https://github.com/4nkitd/incus.ssh/releases/tag/v0.4.0). SCP/SFTP needs a guest SFTP server. [Explicit per-port HTTPS sharing](https://4nkitd.github.io/incus.ssh/docs.html#sharing) is opt-in and experimental. The executable and service are named `incus-for-devs`.
