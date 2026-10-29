#!/usr/bin/env bash
set -euo pipefail
umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin

version=v0.4.0
archive_sha256=6a1c1341f289dd621118a3cbd3e357b70d76a4bdb52f21d5743098d0c0109489
repo=https://github.com/4nkitd/incus.ssh
config=/etc/incus-for-devs/config.json
state=/var/lib/incus-for-devs
unit=/etc/systemd/system/incus-for-devs.service
binary=/usr/local/bin/incus-for-devs
account=incus-for-devs
listen=127.0.0.1:2222
key_file=
yes=0
work=

usage() {
  cat <<'USAGE'
Install incus.ssh on a fresh Ubuntu 24.04 amd64 host.

  curl -fsSL https://raw.githubusercontent.com/4nkitd/incus.ssh/main/install.sh | sudo bash

Options:
  --ssh-key-file FILE  Your public SSH key, not a private key.
  --listen IP:PORT     Loopback or a local Tailscale address; default 127.0.0.1:2222.
  --yes               Confirm noninteractive setup; requires --ssh-key-file.
  --help              Show this help without changing anything.

Installs released v0.4.0, Incus, a private SSH gateway, a directory pool,
a managed bridge, a cached Ubuntu image and one restricted user/project.
Never upgrades or restarts an existing gateway. No guest port is published.
USAGE
}

fail() { printf 'incus.ssh: %s\n' "$*" >&2; exit 1; }
say() { printf '\n%s\n' "$*"; }
incus() { env -i PATH="$PATH" HOME=/root LANG=C LC_ALL=C /usr/bin/incus --force-local "$@"; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --yes) yes=1; shift ;;
    --ssh-key-file|--listen)
      [ "$#" -ge 2 ] || fail "$1 requires a value"
      case "$1" in --ssh-key-file) key_file=$2 ;; --listen) listen=$2 ;; esac
      shift 2 ;;
    *) fail "unknown option $1; use --help" ;;
  esac
done

[[ "$archive_sha256" =~ ^[a-f0-9]{64}$ ]] || fail 'release checksum has not been finalized'
[ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] || fail 'server installation requires Linux amd64; use the manual guide for other platforms'
[ "$(id -u)" -eq 0 ] || fail 'run this installer with sudo bash or as root'
[ -f /etc/os-release ] || fail '/etc/os-release is missing'
. /etc/os-release
[ "${ID:-}" = ubuntu ] && [ "${VERSION_ID:-}" = 24.04 ] || fail 'this installer supports Ubuntu 24.04; see the manual installation guide'
command -v systemctl >/dev/null || fail 'systemd is required'
[ "$(ps -p 1 -o comm=)" = systemd ] || fail 'systemd must be running as PID 1'
unit_load=$(systemctl show --property=LoadState --value incus-for-devs.service 2>/dev/null || true)
[ -n "$unit_load" ] || fail 'could not inspect the gateway service state'

if [ -e "$config" ] || [ -L "$config" ] || [ -e "$unit" ] || [ -L "$unit" ] || [ "$unit_load" != not-found ]; then
  if [ -f /etc/incus-for-devs/install-version ] && [ ! -L /etc/incus-for-devs/install-version ] &&
     [ "$(cat /etc/incus-for-devs/install-version)" = "$version" ] &&
     [ -f "$config" ] && [ ! -L "$config" ] && systemctl is-active --quiet incus-for-devs.service; then
    say "incus.ssh $version is already installed. No files changed or services restarted."
    printf '%s\n' 'Configuration: /etc/incus-for-devs/config.json' 'Guide: https://4nkitd.github.io/incus.ssh/docs.html'
    exit 0
  fi
  fail 'an existing gateway or partial installation was found; refusing to overwrite it. Use the manual guide'
fi
for path in "$binary" "$state" /etc/incus-for-devs; do
  [ ! -e "$path" ] && [ ! -L "$path" ] || fail "existing path $path; refusing to overwrite it"
done
! getent passwd "$account" >/dev/null || fail "service account $account already exists; use the manual guide"
! getent group "$account" >/dev/null || fail "service group $account already exists; use the manual guide"
for tool in curl tar sha256sum ssh-keygen ssh-keyscan ss ip runuser; do
  command -v "$tool" >/dev/null || fail "$tool is required before installation"
done

host=${listen%:*}
port=${listen##*:}
host=${host#\[}
host=${host%\]}
case "$port" in ''|*[!0-9]*) fail 'listen port must be a number from 1024 to 65535' ;; esac
[ "${#port}" -le 5 ] || fail 'listen port is too long'
port=$((10#$port))
[ "$port" -ge 1024 ] && [ "$port" -le 65535 ] || fail 'listen port must be from 1024 to 65535'
if [[ "$host" == *:* ]]; then
  [[ "$listen" == \[*\]:* ]] || fail 'IPv6 listeners must use [address]:port'
  [[ "$host" == ::1 || "$host" == fd7a:115c:a1e0:* ]] || fail 'use IPv6 loopback or a Tailscale IPv6 address'
  [[ "$host" != *[!0-9a-fA-F:]* ]] || fail 'invalid IPv6 listener'
else
  IFS=. read -r a b c d extra <<< "$host"
  [ -n "${d:-}" ] && [ -z "${extra:-}" ] || fail 'listen must be a literal private IP:port'
  for octet in "$a" "$b" "$c" "$d"; do
    [[ "$octet" =~ ^[0-9]{1,3}$ ]] && [ "$((10#$octet))" -le 255 ] || fail 'invalid IPv4 listener'
  done
  [ "$((10#$a))" -eq 127 ] || { [ "$((10#$a))" -eq 100 ] && [ "$((10#$b))" -ge 64 ] && [ "$((10#$b))" -le 127 ]; } || fail 'use loopback or a Tailscale IPv4 address, never a public/wildcard listener'
fi
addresses=$(ip -o address show | awk '{sub(/\/.*/, "", $4); print $4}') || fail 'could not inspect host addresses'
grep -Fxq "$host" <<< "$addresses" || fail 'the listener address is not assigned to this host'
listeners=$(ss -H -ltn "sport = :$port") || fail 'could not inspect listener ports'
[ -z "$listeners" ] || fail "TCP port $port is already in use"

if [ "$yes" -eq 1 ] && [ -z "$key_file" ]; then
  fail '--yes requires --ssh-key-file; no identity is selected automatically'
fi
if [ -n "$key_file" ]; then
  [ -f "$key_file" ] && [ "$(wc -c < "$key_file")" -le 16384 ] || fail 'public key must be a regular file smaller than 16KiB'
  public_key=$(cat "$key_file")
else
  [ -r /dev/tty ] && [ -w /dev/tty ] || fail 'interactive setup needs a terminal; use --yes --ssh-key-file FILE'
  printf '\nPaste your PUBLIC SSH key for the dev login: ' > /dev/tty
  IFS= read -r public_key < /dev/tty || fail 'no public key supplied'
fi
[[ "$public_key" != *$'\n'* && "$public_key" != *$'\r'* && "$public_key" != *$'\t'* ]] || fail 'supply exactly one public key without control characters'
IFS=' ' read -r key_type key_blob _ <<< "$public_key"
case "$key_type" in ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521) ;; *) fail 'unsupported public key type; private keys, certificates and authorized-key options are not accepted' ;; esac
[[ "${key_blob:-}" =~ ^[A-Za-z0-9+/=]+$ ]] || fail 'invalid public key data'
public_key="$key_type $key_blob"
work=$(mktemp -d -t incus-ssh-install.XXXXXXXX)
cleanup() { if [[ "$work" == */incus-ssh-install.* && -d "$work" ]]; then rm -rf -- "$work"; fi; }
trap cleanup EXIT
printf '%s\n' "$public_key" > "$work/user.pub"
ssh-keygen -lf "$work/user.pub" >/dev/null || fail 'the public key could not be parsed'

had_incus=0
if [ -e /usr/bin/incus ]; then
  had_incus=1
  incus --quiet query /1.0 >/dev/null || fail 'existing Incus is not ready; start or repair it deliberately before retrying'
  for kind_name in 'storage ifd-pool' 'network ifdbr0' 'project ifd-dev'; do
    read -r kind name <<< "$kind_name"
    if incus "$kind" show "$name" >/dev/null 2>&1; then
      fail "existing $kind $name; refusing to adopt or modify it"
    fi
  done
  aliases=$(incus --quiet query /1.0/images/aliases) || fail 'could not inspect existing image aliases'
  if grep -Fq '"/1.0/images/aliases/ifd-ubuntu-24-04"' <<< "$aliases"; then
    fail 'image alias ifd-ubuntu-24-04 already exists; refusing to replace it'
  fi
fi

say "Install incus.ssh $version"
printf '%s\n' \
  'This is host maintenance. Run it on a fresh development server.' \
  'Installs Incus packages/dependencies, creates ifd-pool/ifdbr0/ifd-dev,' \
  'caches native Ubuntu 24.04, and starts a new private gateway service.' \
  'App login: dev. One container at a time by default.' \
  "Listener: $listen. No guest service is published." \
  'Administrative SSH, Tailscale and existing containers are not reconfigured.'
printf 'Authorized public key: '
ssh-keygen -lf "$work/user.pub"
if [ "$yes" -ne 1 ]; then
  printf '\nProceed? [y/N] ' > /dev/tty
  IFS= read -r answer < /dev/tty || fail 'installation not confirmed'
  [[ "$answer" == y || "$answer" == Y || "$answer" == yes ]] || fail 'cancelled; no installation performed'
fi

say 'Download and verify the released binary'
archive="incus.ssh_${version}_linux_amd64.tar.gz"
curl -fL --proto '=https' --tlsv1.2 --retry 3 --connect-timeout 15 --max-time 180 \
  "$repo/releases/download/$version/$archive" -o "$work/release.tar.gz"
printf '%s  %s\n' "$archive_sha256" "$work/release.tar.gz" | sha256sum -c -
tar -xzf "$work/release.tar.gz" -C "$work"
app=$work/bin/incus-for-devs
[ "$("$app" version)" = "incus.ssh $version (incus-for-devs CLI)" ] || fail 'release version mismatch'

say 'Prepare native Incus'
"$app" setup --install-incus --apply
if [ "$had_incus" -eq 0 ]; then systemctl start incus.socket incus.service; fi
incus --quiet query /1.0 >/dev/null || fail 'Incus is not ready; inspect it before retrying'
incus storage create ifd-pool dir
incus network create ifdbr0 ipv4.address=auto ipv4.nat=true ipv6.address=none
fingerprint=$(incus image info images:ubuntu/24.04 | awk '$1 == "Fingerprint:" {print $2}')
[[ "$fingerprint" =~ ^[a-f0-9]{64}$ ]] || fail 'could not resolve the native Ubuntu image fingerprint'
incus image copy "images:$fingerprint" local:
incus image alias create ifd-ubuntu-24-04 "$fingerprint"
sed -i 's/restricted.networks.access: incusbr0/restricted.networks.access: ifdbr0/' "$work/deploy/project.yaml"
sed -i -e 's/network: incusbr0/network: ifdbr0/' -e 's/pool: devpool/pool: ifd-pool/' "$work/deploy/profile.yaml"
"$app" setup --templates "$work/deploy" --project ifd-dev --profile ifd \
  --pool ifd-pool --network ifdbr0 --image ifd-ubuntu-24-04 --apply

say 'Install the private gateway'
useradd --system --user-group --home-dir "$state" --shell /usr/sbin/nologin "$account"
install -d -o "$account" -g "$account" -m 0700 "$state"
install -d -o root -g "$account" -m 0750 /etc/incus-for-devs
install -o root -g root -m 0755 "$app" "$binary"
cores=$(getconf _NPROCESSORS_ONLN)
cpu=2
if [ "$cores" -lt "$cpu" ]; then cpu=$cores; fi
cat > "$work/config.json" <<CONFIG
{
  "listen": "$listen",
  "host_key": "$state/host.key",
  "incus_binary": "/usr/bin/incus",
  "image": "local:ifd-ubuntu-24-04",
  "profile": "ifd",
  "operation_timeout_seconds": 120,
  "max_sessions": 8,
  "limits": {"max_instances": 1, "cpu": $cpu, "memory": "4GiB", "processes": 512},
  "users": [{"name": "dev", "project": "ifd-dev", "authorized_keys": ["$public_key"]}]
}
CONFIG
install -o root -g "$account" -m 0640 "$work/config.json" "$config"
keygen_output=$(runuser -u "$account" -- "$binary" keygen --path "$state/host.key")
printf '%s\n' "$keygen_output"
expected=${keygen_output##* }
[[ "$expected" =~ ^SHA256:[A-Za-z0-9+/]{43}$ ]] || fail 'host-key generator did not return a valid fingerprint'
install -o root -g root -m 0644 "$work/deploy/incus-for-devs.service" "$unit"
systemctl daemon-reload
systemctl enable --now incus-for-devs.service
ready=0
for attempt in 1 2 3 4 5; do
  if ssh-keyscan -T 2 -t ed25519 -p "$port" "$host" > "$work/presented.pub" 2>/dev/null && [ -s "$work/presented.pub" ]; then ready=1; break; fi
  sleep 1
done
[ "$ready" -eq 1 ] || fail 'gateway did not become ready; inspect journalctl -u incus-for-devs. No existing service was restarted'
presented=$(ssh-keygen -lf "$work/presented.pub" | awk '{print $2}')
[ "$expected" = "$presented" ] || fail 'the gateway presented an unexpected host key; inspect the service before connecting'
printf '%s\n' "$version" > /etc/incus-for-devs/install-version
chmod 0600 /etc/incus-for-devs/install-version
say 'Installed. The SSH listener presented the expected host key.'
printf 'Verify and pin this fingerprint before first login: %s\n' "$expected"
case "$host" in
  127.*|::1)
    forward_host=$host
    if [ "$host" = ::1 ]; then forward_host='[::1]'; fi
    printf '\n%s\n' 'From your laptop, keep an administrative SSH tunnel open:'
    printf '  ssh -N -L %s:%s:%s YOUR_ADMIN_USER@YOUR_SERVER\n' "$port" "$forward_host" "$port"
    printf 'Then connect in another terminal:\n  ssh -p %s dev@127.0.0.1\n' "$port" ;;
  *) printf '\nConnect over your tailnet:\n  ssh -p %s dev@%s\n' "$port" "$host" ;;
esac
printf '\n%s\n' 'Inside the gateway: new --name=demo, then ssh demo.' \
  'Guide: https://4nkitd.github.io/incus.ssh/docs.html' \
  'Containers share the host kernel. Keep backups and review guest network policy.'
