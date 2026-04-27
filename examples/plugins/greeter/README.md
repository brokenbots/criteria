# overseer-adapter-greeter

A minimal example of a third-party [Overseer](https://github.com/brokenbots/overseer) adapter plugin. It demonstrates the full path from writing a plugin to running it with `overseer apply`.

The greeter accepts one input key (`name`) and returns:

- **outcome** `success`
- **output** `greeting = "hello, <name>"`

## Prerequisites

- Go 1.26+ (matches the `go` directive in `go.mod`)
- The `overseer` binary (built with `make build` from the overseer repo root)

## Build and install

```bash
# From this directory:
go build -o bin/overseer-adapter-greeter .

# Install into your personal plugin directory:
mkdir -p ~/.overseer/plugins
cp bin/overseer-adapter-greeter ~/.overseer/plugins/
chmod +x ~/.overseer/plugins/overseer-adapter-greeter
```

Or use a temporary directory to avoid touching your home directory:

```bash
tmpdir=$(mktemp -d)
go build -o "$tmpdir/overseer-adapter-greeter" .
chmod +x "$tmpdir/overseer-adapter-greeter"
OVERSEER_PLUGINS="$tmpdir" overseer apply example.hcl
```

## Run the example workflow

```bash
overseer apply example.hcl
```

Expected output (concise mode):

```
▶ greeter_example  steps=1
[1/1] greet  (greeter)
  hello, world
  ✓ success in <Xms>
  · outputs: greeting
  → done
✔ run completed in <Xms>
```

The `greeting` output is available to any downstream step as `steps.greet.greeting`.

## How it works

`main.go` implements the [`pluginhost.Service`](https://pkg.go.dev/github.com/brokenbots/overseer/sdk/pluginhost) interface and calls `pluginhost.Serve` from `main()`. That is the entire plugin contract:

```go
type Service interface {
    Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error)
    OpenSession(context.Context, *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error)
    Execute(context.Context, *pb.ExecuteRequest, ExecuteEventSender) error
    Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error)
    CloseSession(context.Context, *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error)
}
```

Overseer discovers the binary as `overseer-adapter-<name>` and manages the subprocess lifecycle via [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin).

## SDK version note

The `go.mod` in this directory currently uses a `replace` directive that points to the in-tree `sdk/` module. This is a **temporary workaround** until the first `github.com/brokenbots/overseer/sdk` tag is published (tracked in [W09](../../../workstreams/09-phase0-cleanup-gate.md)). Once a tag exists, remove the `replace` directive and update the `require` line to the published version.

For local development against an unreleased SDK, add a `go.work` file (gitignored) that includes the SDK module. This lets you test changes without modifying `go.mod`.
