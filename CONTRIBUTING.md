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
