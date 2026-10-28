# Contributing

## Before you start

Open an issue for anything larger than a small fix. This gateway deliberately exposes a narrow interface. Features that widen the trust boundary need a design discussion first.

The current scope is a single trusted app user, one restricted Incus project, fixed image and profile, and the documented command subset. Do not add arbitrary Incus flags, client-selected projects, or raw host access.

## Development

Use Go 1.26 or newer. Edit locally; build and test where your project permits it.

```sh
gofmt -w cmd internal integration
go mod tidy
go test ./...
go test -race ./...
go vet ./...
```

Unit and transport tests use temporary keys and fake backends. Do not point tests at a shared Incus server. Real lifecycle tests must use a dedicated project, uniquely named disposable containers, and a confirmed cleanup path.

## Code expectations

- Keep changes small and within the existing package boundaries.
- Validate all client input; never pass client text to a host shell.
- Preserve strict host-key verification and public-key username binding.
- Preserve project ownership and unrelated containers.
- Do not log credentials, private keys or customer data.
- Add or update a regression test for every behavior change.

## Pull requests

Describe what changed, why, and how you tested it. Note any new limitation or behavior change. Keep unrelated refactors out of the same pull request.

By contributing, you agree that your contribution is licensed under the project's MIT license.
