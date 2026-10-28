#!/bin/sh
set -eu

LC_ALL=C
export LC_ALL
umask 022

usage() {
  printf '%s\n' 'usage: sh scripts/release.sh vMAJOR.MINOR.PATCH[-prerelease][+build]' >&2
  exit 2
}

fail() {
  printf 'release: %s\n' "$*" >&2
  exit 1
}

[ "$#" -eq 1 ] || usage
version=$1

case "$version" in
  v*) ;;
  *) fail "version must start with 'v': $version" ;;
esac
case "$version" in
  *[!A-Za-z0-9.+-]*) fail "version contains invalid characters" ;;
esac

if ! printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?([+][0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'; then
  fail "version is not an exact semantic version: $version"
fi
version_without_build=${version%%+*}
prerelease=${version_without_build#*-}
if [ "$prerelease" != "$version_without_build" ]; then
  previous_ifs=$IFS
  IFS=.
  for identifier in $prerelease; do
    case "$identifier" in
      *[!0-9]*) ;;
      0) ;;
      0*) fail "numeric prerelease identifiers cannot have leading zeros" ;;
    esac
  done
  IFS=$previous_ifs
fi

root=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
cd "$root"

[ -f go.mod ] || fail "go.mod not found under $root"
grep -qx 'module incus-for-devs' go.mod || fail "unexpected module path in go.mod"

command -v go >/dev/null 2>&1 || fail "go not found in PATH"
command -v tar >/dev/null 2>&1 || fail "tar not found in PATH"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum not found in PATH"
[ "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" = linux/amd64 ] || fail "run the release build on a Linux amd64 builder"

for source in \
  go.mod \
  cmd/incus-for-devs/main.go \
  LICENSE \
  THIRD_PARTY_NOTICES.md \
  README.md \
  TESTING.md \
  SECURITY.md \
  CHANGELOG.md \
  config.example.json \
  deploy/project.yaml \
  deploy/profile.yaml \
  deploy/incus-for-devs.service
do
  [ -f "$source" ] || fail "missing required file: $source"
done

dist_dir=$root/dist
stage=$dist_dir/.stage-$version
outdir=$dist_dir/$version

if [ -e "$outdir" ]; then
  fail "refusing to overwrite existing dist/$version"
fi
if [ -e "$stage" ]; then
  fail "refusing to reuse existing scratch directory dist/.stage-$version"
fi

mkdir -p "$dist_dir"
mkdir "$stage" || fail "could not create scratch directory dist/.stage-$version"
stage_created=1
outdir_created=0
finalized=0

cleanup() {
  status=$?
  trap - 0
  if [ "$finalized" -eq 0 ] && [ "$outdir_created" -eq 1 ]; then
    rm -rf "$outdir"
  fi
  if [ "$stage_created" -eq 1 ]; then
    rm -rf "$stage"
  fi
  exit "$status"
}
trap cleanup 0
trap 'exit 130' INT
trap 'exit 143' TERM HUP

mkdir -p "$stage/bin" "$stage/pack" "$stage/out"

package_target() {
  target_os=$1
  target_arch=$2
  target=$target_os-$target_arch
  bin=$stage/bin/$target/incus-for-devs
  pack=$stage/pack/$target
  archive=$stage/out/incus.ssh_${version}_${target_os}_${target_arch}.tar.gz

  printf 'release: building %s (CGO_ENABLED=0, GOOS=%s, GOARCH=%s)\n' "$target" "$target_os" "$target_arch"
  mkdir -p "$stage/bin/$target"
  CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch GOFLAGS= GOWORK=off GOTOOLCHAIN=local \
    go build -mod=readonly -trimpath -buildvcs=false \
      -ldflags "-X incus-for-devs/internal/app.Version=$version" \
      -o "$bin" ./cmd/incus-for-devs

  if [ "$target" = linux-amd64 ]; then
    [ "$("$bin" version)" = "incus.ssh $version (incus-for-devs CLI)" ] \
      || fail "version $version was not embedded in the Linux binary"
  else
    go version -m "$bin" | grep -Fq 'GOOS=darwin' || fail "incorrect Darwin target"
    go version -m "$bin" | grep -Fq 'GOARCH=arm64' || fail "incorrect Darwin architecture"
  fi

  mkdir -p "$pack/bin" "$pack/deploy"
  cp "$bin" "$pack/bin/incus-for-devs"
  cp LICENSE "$pack/LICENSE"
  cp THIRD_PARTY_NOTICES.md "$pack/THIRD_PARTY_NOTICES.md"
  cp README.md "$pack/README.md"
  cp TESTING.md "$pack/TESTING.md"
  cp SECURITY.md "$pack/SECURITY.md"
  cp CHANGELOG.md "$pack/CHANGELOG.md"
  cp config.example.json "$pack/config.example.json"
  cp deploy/project.yaml "$pack/deploy/project.yaml"
  cp deploy/profile.yaml "$pack/deploy/profile.yaml"
  cp deploy/incus-for-devs.service "$pack/deploy/incus-for-devs.service"
  chmod 0755 "$pack/bin/incus-for-devs"
  chmod 0644 \
    "$pack/LICENSE" \
    "$pack/THIRD_PARTY_NOTICES.md" \
    "$pack/README.md" \
    "$pack/TESTING.md" \
    "$pack/SECURITY.md" \
    "$pack/CHANGELOG.md" \
    "$pack/config.example.json" \
    "$pack/deploy/project.yaml" \
    "$pack/deploy/profile.yaml" \
    "$pack/deploy/incus-for-devs.service"

  ( cd "$pack" && tar -czf "$archive" \
      bin/incus-for-devs \
      LICENSE \
      THIRD_PARTY_NOTICES.md \
      README.md \
      TESTING.md \
      SECURITY.md \
      CHANGELOG.md \
      config.example.json \
      deploy/project.yaml \
      deploy/profile.yaml \
      deploy/incus-for-devs.service )

  tar -tzf "$archive" > "$stage/entries"
  while IFS= read -r entry; do
    case "$entry" in
      bin/|bin/incus-for-devs|LICENSE|THIRD_PARTY_NOTICES.md|README.md|TESTING.md|SECURITY.md|CHANGELOG.md|config.example.json|deploy/|deploy/project.yaml|deploy/profile.yaml|deploy/incus-for-devs.service) ;;
      *) fail "unexpected entry in $(basename "$archive"): $entry" ;;
    esac
  done < "$stage/entries"
  for entry in \
    bin/incus-for-devs \
    LICENSE \
    THIRD_PARTY_NOTICES.md \
    README.md \
    TESTING.md \
    SECURITY.md \
    CHANGELOG.md \
    config.example.json \
    deploy/project.yaml \
    deploy/profile.yaml \
    deploy/incus-for-devs.service
  do
    grep -Fxq "$entry" "$stage/entries" || fail "missing entry in $(basename "$archive"): $entry"
  done
  rm -f "$stage/entries"
}

package_target linux amd64
package_target darwin arm64

( cd "$stage/out" && sha256sum ./*.tar.gz > SHA256SUMS )
[ "$(wc -l < "$stage/out/SHA256SUMS")" -eq 2 ] || fail "expected two archive checksums"

mkdir "$outdir" || fail "refusing to overwrite existing dist/$version"
outdir_created=1
for artifact in "$stage/out"/*; do
  mv "$artifact" "$outdir/"
done

( cd "$outdir" && sha256sum -c SHA256SUMS )
finalized=1

printf 'release: version %s\n' "$version"
printf 'release: %s\n' "$(go version)"
printf 'release: wrote dist/%s/\n' "$version"
printf '%s\n' "$outdir/SHA256SUMS"
