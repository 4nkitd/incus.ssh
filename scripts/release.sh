#!/bin/sh
set -eu

LC_ALL=C
export LC_ALL
GOFLAGS=
GOWORK=off
export GOFLAGS GOWORK
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
build_go_version=$(go env GOVERSION)
case "$build_go_version" in
  go1.*.*) ;;
  *) fail "release requires a stable Go toolchain" ;;
esac
minor=$(printf '%s' "$build_go_version" | cut -d. -f2)
patch=$(printf '%s' "$build_go_version" | cut -d. -f3)
case "$minor:$patch" in *[!0-9:]*) fail "invalid release toolchain version" ;; esac
[ "$minor" -gt 27 ] || { [ "$minor" -eq 27 ] && [ "$patch" -ge 4 ]; } || fail "Go 1.27.4 or newer is required (GO-2026-6859)"

for source in \
  go.mod \
  cmd/incus-for-devs/main.go \
  LICENSE \
  THIRD_PARTY_NOTICES.md \
  README.md \
  TESTING.md \
  SECURITY.md \
  CHANGELOG.md \
  docs/phase3.md \
  docs/api.md \
  docs/ui.md \
  internal/controlapi/ui/pico.LICENSE.md \
  docs/assets/fonts/LICENSE.txt \
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
mkdir -p "$stage/licenses/$build_go_version"
go mod verify
cp "$(go env GOROOT)/LICENSE" "$stage/licenses/$build_go_version/LICENSE"
: > "$stage/modules.raw"
for target_os in linux darwin; do
  case "$target_os" in linux) target_arch=amd64 ;; darwin) target_arch=arm64 ;; esac
  CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch go list -mod=readonly -deps -f '{{if .Module}}{{if not .Module.Main}}{{.Module.Path}} {{.Module.Version}} {{.Module.Dir}}{{end}}{{end}}' ./cmd/incus-for-devs >> "$stage/modules.raw"
done
sed '/^$/d' "$stage/modules.raw" | sort -u > "$stage/modules"
while read -r module module_version module_dir; do
  [ -n "$module_dir" ] || fail "missing module directory for $module"
  target_dir=$stage/licenses/$module@$module_version
  mkdir -p "$target_dir"
  found=0
  for license in "$module_dir"/LICENSE* "$module_dir"/COPYING* "$module_dir"/NOTICE* "$module_dir"/PATENTS; do
    [ -f "$license" ] && [ ! -L "$license" ] || continue
    cp "$license" "$target_dir/$(basename "$license")"
    case "$(basename "$license")" in LICENSE*|COPYING*) found=1 ;; esac
  done
  [ "$found" -eq 1 ] || fail "missing license text for $module"
  printf '%s %s\n' "$module" "$module_version" >> "$stage/licenses/MODULES.txt"
done < "$stage/modules"

package_target() {
  target_os=$1
  target_arch=$2
  target=$target_os-$target_arch
  bin=$stage/bin/$target/incus-for-devs
  pack=$stage/pack/$target
  archive=$stage/out/incus.ssh_${version}_${target_os}_${target_arch}.tar.gz

  printf 'release: building %s (CGO_ENABLED=0, GOOS=%s, GOARCH=%s)\n' "$target" "$target_os" "$target_arch"
  mkdir -p "$stage/bin/$target"
  CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch GOFLAGS= GOWORK=off \
    go build -mod=readonly -trimpath -buildvcs=false \
      -ldflags "-X incus-for-devs/internal/app.Version=$version" \
      -o "$bin" ./cmd/incus-for-devs

  [ "$(go version -m "$bin" | sed -n '1s/.*: //p')" = "$build_go_version" ] || fail "build used a different Go toolchain"

  if [ "$target" = linux-amd64 ]; then
    [ "$("$bin" version)" = "incus.ssh $version (incus-for-devs CLI)" ] \
      || fail "version $version was not embedded in the Linux binary"
  else
    go version -m "$bin" | grep -Fq 'GOOS=darwin' || fail "incorrect Darwin target"
    go version -m "$bin" | grep -Fq 'GOARCH=arm64' || fail "incorrect Darwin architecture"
  fi

  mkdir -p "$pack/bin" "$pack/deploy" "$pack/docs"
  cp "$bin" "$pack/bin/incus-for-devs"
  cp LICENSE "$pack/LICENSE"
  cp THIRD_PARTY_NOTICES.md "$pack/THIRD_PARTY_NOTICES.md"
  cp README.md "$pack/README.md"
  cp TESTING.md "$pack/TESTING.md"
  cp SECURITY.md "$pack/SECURITY.md"
  cp CHANGELOG.md "$pack/CHANGELOG.md"
  cp docs/phase3.md "$pack/docs/phase3.md"
  cp docs/api.md "$pack/docs/api.md"
  cp docs/ui.md "$pack/docs/ui.md"
  cp internal/controlapi/ui/pico.LICENSE.md "$pack/docs/PICO-LICENSE.md"
  cp docs/assets/fonts/LICENSE.txt "$pack/docs/FONT-LICENSE.txt"
  cp -R "$stage/licenses" "$pack/docs/licenses"
  find "$pack/docs/licenses" -type f -exec chmod 0644 {} \;
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
    "$pack/docs/phase3.md" \
    "$pack/docs/api.md" \
    "$pack/docs/ui.md" \
    "$pack/docs/PICO-LICENSE.md" \
    "$pack/docs/FONT-LICENSE.txt" \
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
      docs/phase3.md \
      docs/api.md \
      docs/ui.md \
      docs/PICO-LICENSE.md \
      docs/FONT-LICENSE.txt \
      docs/licenses \
      config.example.json \
      deploy/project.yaml \
      deploy/profile.yaml \
      deploy/incus-for-devs.service )

  tar -tzf "$archive" > "$stage/entries"
  while IFS= read -r entry; do
    case "$entry" in
      bin/|bin/incus-for-devs|LICENSE|THIRD_PARTY_NOTICES.md|README.md|TESTING.md|SECURITY.md|CHANGELOG.md|docs/|docs/phase3.md|docs/api.md|docs/ui.md|docs/PICO-LICENSE.md|docs/FONT-LICENSE.txt|docs/licenses/|docs/licenses/*|config.example.json|deploy/|deploy/project.yaml|deploy/profile.yaml|deploy/incus-for-devs.service) ;;
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
    docs/phase3.md \
    docs/api.md \
    docs/ui.md \
    docs/PICO-LICENSE.md \
    docs/FONT-LICENSE.txt \
    docs/licenses/MODULES.txt \
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
