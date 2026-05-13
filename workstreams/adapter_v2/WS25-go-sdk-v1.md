# WS25 — Go adapter SDK v1.0 (new repo)

**Phase:** Adapter v2 · **Track:** SDK · **Owner:** Workstream executor (creates new repo `criteria-go-adapter-sdk`) · **Depends on:** [WS02](WS02-protocol-v2-proto.md). · **Unblocks:** [WS21](WS21-sdk-serveremote.md), [WS27](WS27-starter-repos.md), [WS31](WS31-migrate-shell.md), [WS42](WS42-extract-shell-adapter.md).

## Context

`README.md` D44 introduces a Go SDK alongside the existing TypeScript and Python ones. Same API shape, same protocol contract. Used by:

- The migrated `shell` builtin in WS31 (consumed as a local Go module while shell stays in-tree).
- The extracted `criteria-adapter-shell` in WS42 (consumes the published Go module).
- Any future Go adapters (community or first-party).

## Prerequisites

WS02 merged (Go bindings are essentially shared with the criteria monorepo's, but vendored for the SDK repo).

## In scope

### Step 1 — Repo bootstrap

Create `criteria-go-adapter-sdk` repo with standard Go module layout, Apache-2 license, MIT-style CONTRIBUTING.

### Step 2 — `Serve(...)` API

```go
package adapter

func Serve(cfg Config) error
func ServeRemote(cfg RemoteConfig) error  // WS21

type Config struct {
    Name        string
    Version     string
    Description string
    SourceURL   string
    Capabilities []string
    Platforms    []Platform
    ConfigSchema  Schema
    InputSchema   Schema
    OutputSchema  Schema
    Secrets       []SecretDecl
    Permissions   []string
    CompatibleEnvironments []string

    OnOpenSession  func(ctx context.Context, req *v2.OpenSessionRequest, h Helpers) (*v2.OpenSessionResponse, error)
    OnExecute      func(ctx context.Context, req *v2.ExecuteRequest, h Helpers) error
    OnCloseSession func(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error)
    OnSnapshot     func(ctx context.Context, sessionID string) ([]byte, error)
    OnRestore      func(ctx context.Context, sessionID string, data []byte) error
    OnInspect      func(ctx context.Context, sessionID string) (*v2.InspectResponse, error)
}
```

### Step 3 — Helpers interface

```go
type Helpers struct {
    Session     SessionStore
    Outcomes    OutcomeValidator
    Permissions PermissionCorrelator
    Log         RedactingLogger
    Secrets     Secrets
}

type Secrets interface {
    Get(ctx context.Context, name string) (string, error)
    // SpawnEnv returns an env map suitable for exec.Cmd.Env containing the
    // requested secrets. Refuses to expose a secret not in the adapter's
    // manifest. (D75)
    SpawnEnv(ctx context.Context, names ...string) ([]string, error)
}
```

### Step 4 — Schema generation from struct tags

```go
type Schema struct { Fields map[string]Field }

func SchemaFromStruct[T any]() Schema  // reflection over struct tags
```

Tags: `criteria:"required,sensitive,description=foo"`.

### Step 5 — `--emit-manifest` mode

When the binary is invoked with `--emit-manifest`, emit `adapter.yaml` to stdout and exit.

### Step 6 — TestHost

`testhost` subpackage with programmatic + CLI APIs (`criteria-go-adapter-test`).

### Step 7 — Library mode

Direct handler invocation for unit tests, parallel to TS/Python SDKs.

### Step 8 — Build matrix

`linux/amd64`, `linux/arm64`, `darwin/arm64` (native Go cross-compile via `GOOS`/`GOARCH`). Add `windows/amd64` ready.

### Step 9 — Docs

README opens with shelling-out guidance (D74), `SpawnEnv` example.

## Out of scope

- Adapter migrations consuming this SDK — WS31, WS42.
- Conformance harness — WS26.

## Behavior change

**N/A — new package.**

## Tests required

- Full SDK test suite green.
- Module published to a tagged release on the new repo; `go get github.com/brokenbots/criteria-go-adapter-sdk@v1.0.0-rc.N` resolves.

## Exit criteria

- Module exists, builds across the platform matrix, and the WS31 (shell migration) and WS30 (greeter equivalent for go — optional) compile against it.

## Files this workstream may modify

- Everything in `criteria-go-adapter-sdk/` (new repo).

## Files this workstream may NOT edit

- The criteria monorepo.
- Other workstream files.
