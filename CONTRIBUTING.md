# Contributing

## Before you start

Open an issue for anything larger than a small fix. This gateway deliberately exposes a narrow interface. Features that widen the trust boundary need a design discussion first.

Phase 1 scope is a single trusted app user, one restricted Incus project, fixed image and profile, and this command set: `help`, `whoami`, `new`, `ls`, `ssh`, `start`, `stop`, `restart`, `resume` (alias of `start`) and `rm`. Resource values are per-container maxima with defaults of 1 CPU, 2GiB memory and 256 processes. Do not add arbitrary Incus flags, client-selected projects, or raw host access; do not turn `resume` into a RAM snapshot promise.

`pause`, SFTP/SCP, web previews, sharing, Docker/nested containers, OpenTunnel-style exposure and multiuser admission are outside Phase 1. Do not implement them as an unannounced side effect of another change.

## Development

Use Go 1.26 or newer. Edit locally; build and test where your project permits it. Build and test on Linux, not on the target host.

```sh
gofmt -w cmd internal integration
go mod tidy
go test ./...
go test -race ./...
go vet ./...
go build -trimpath -o bin/incus-for-devs ./cmd/incus-for-devs
```

Unit and transport tests use temporary keys and fake backends. Do not point tests at a shared Incus server. Real lifecycle tests must use a dedicated project, uniquely named disposable containers, and a confirmed cleanup path.

If you build a client for another platform, use a different output name. Do not overwrite `bin/incus-for-devs` with a non-Linux build or install that build on a Linux host.

## Code expectations

- Keep changes small and within the existing package boundaries.
- Validate all client input; never pass client text to a host shell.
- Preserve strict host-key verification and public-key username binding.
- Preserve project ownership and unrelated containers.
- Keep memory admission conservative and fail closed when host capacity cannot be verified; it applies to create, start and restart. CPU maxima are caps, not a scheduler.
- Keep setup fail-closed: the plan must not mutate; apply may create only missing project and profile resources, reject unknown YAML keys, and never edit, delete, download or create pools or networks.
- Do not log credentials, private keys or customer data.
- Add or update a regression test for every behavior change.

## Documentation

The README is an overview; `docs/` is the published site and the full guide. Keep placeholders (`GATEWAY`, `you`, `ifd-dev`, `devpool`) generic. Never commit real host addresses, private network details, credentials or local machine paths.

Record user-visible changes in `CHANGELOG.md` under `Unreleased`. Do not label a version as released before the maintainer publishes it.

## Pull requests

Describe what changed, why, and how you tested it. Note any new limitation or behavior change. Keep unrelated refactors out of the same pull request.

By contributing, you agree that your contribution is licensed under the project's MIT license.
