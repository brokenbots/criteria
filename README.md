# Overseer

Overseer is the standalone execution engine for [Overlord](https://github.com/brokenbots/overlord) workflows. It compiles HCL workflow definitions into finite-state machines, drives execution locally or in coordination with a Castle server, and exposes a plugin interface for swappable AI adapters.

## Packages

| Path | Description |
|------|-------------|
| `cmd/overseer` | CLI entrypoint (`compile`, `plan`, `apply`, `validate`) |
| `sdk/` | Go client SDK, event helpers, and conformance test suite |
| `workflow/` | HCL parser and FSM compiler |
| `proto/overseer/v1/` | Proto source files (`overseer.v1` package) |
| `sdk/pb/overseer/v1/` | Generated Go bindings |
| `events/` | Shared event helpers |

## Quickstart

```bash
# Install (requires Go 1.26+)
go install github.com/brokenbots/overseer/cmd/overseer@latest

# Or build from source
make build

# Run a workflow locally (no Castle required)
overseer apply examples/hello.hcl

# With event output captured
overseer apply examples/hello.hcl --events-file /tmp/events.ndjson
```

## Development

```bash
# Sync workspace dependencies
make bootstrap

# Run all tests
make test

# Regenerate proto bindings (requires buf)
make proto
```

## Adapter plugins

Adapter plugins are discovered as `overseer-adapter-<name>` binaries from `${OVERSEER_PLUGINS}/` or `~/.overseer/plugins/`. Build the bundled adapters with:

```bash
make plugins
```

See [docs/plugins.md](docs/plugins.md) for the plugin wire contract.

## Workflow syntax

See [docs/workflow.md](docs/workflow.md) for the full HCL workflow reference and examples.

## SDK conformance

Any orchestrator implementing the `OverseerService` contract can validate compliance:

```go
import "github.com/brokenbots/overseer/sdk/conformance"

func TestMyOverseer(t *testing.T) {
    conformance.Run(t, &mySubject{})
}
```

See [`sdk/conformance/`](sdk/conformance/) for the interface and in-memory reference implementation.

## License

See [LICENSE](LICENSE).
