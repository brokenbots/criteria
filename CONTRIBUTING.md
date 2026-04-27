# Contributing

## Prerequisites

- Go 1.23+
- [buf](https://buf.build/docs/installation) (for proto regeneration)
- git

## Setup

```bash
git clone https://github.com/brokenbots/overseer.git
cd overseer
make bootstrap
```

## Workflow

1. Fork the repo and create a feature branch.
2. Make your changes. Add or update tests as needed.
3. Run `make test` to verify all tests pass.
4. Run `make validate` to verify example workflows parse correctly.
5. If you changed proto files, run `make proto` and commit the generated code.
6. Open a pull request against `main`.

## Conformance and integration tests

`make test-conformance` runs the SDK conformance suite against the in-memory stub Subject:

```bash
make test-conformance   # fast; no external dependencies
```

The conformance suite is the authoritative proof that the `OverseerService` contract is implementable by any compliant orchestrator. If you build your own orchestrator, validate compliance by implementing `conformance.Subject` and running:

```go
import "github.com/brokenbots/overseer/sdk/conformance"

func TestMyOverseer(t *testing.T) {
    conformance.Run(t, &mySubject{})
}
```

See [`sdk/conformance/`](sdk/conformance/) for the interface and the in-memory reference implementation.

## Published SDK contract

`sdk/` is a published Go sub-module at `github.com/brokenbots/overseer/sdk`. The following are **breaking SDK changes** requiring a version bump:

- Any change to the `conformance.Subject` interface.
- Any change to `ServiceHandler` or `ServiceClient` method signatures.
- Any change to event proto field numbers in `proto/v1/events.proto` (field numbers are permanent).
- Removal or rename of exported SDK functions or types.

Additive changes (new fields, new events, new conformance test cases) are non-breaking at minor or patch level.

## Proto changes

Proto source files live in `proto/v1/`. After editing them:

```bash
make proto       # regenerate Go bindings
make proto-lint  # lint proto files
```

Commit both the `.proto` changes and the regenerated `sdk/pb/` files together.

## Adapter plugins

Plugin binaries are named `overlord-adapter-<name>` and must be placed in
`${OVERLORD_PLUGINS}/` or `~/.overlord/plugins/`. See [docs/plugins.md](docs/plugins.md).

## Code conventions

- Backend: structured logging with `slog` (JSON in production entrypoints).
- Avoid CGO — use pure-Go packages (e.g. `modernc.org/sqlite` if storage is needed).
- Keep plugin adapter code in `cmd/overlord-adapter-*/` and internal adapter
  loader code in `internal/adapter*/`.
