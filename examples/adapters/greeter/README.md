# criteria-adapter-greeter

A minimal example of a third-party [Criteria](https://github.com/brokenbots/criteria) adapter. It demonstrates the full path from writing an adapter to running it with `criteria apply`.

The greeter accepts one input key (`name`) and returns:

- **outcome** `success`
- **output** `greeting = "hello, <name>"`

## Prerequisites

- Go 1.27+ (matches the `go` directive in `go.mod`)
- The `criteria` binary (built with `make build` from the criteria repo root)

## Build and install

```bash
# From this directory:
go build -o bin/criteria-adapter-greeter .

# Install into your personal adapters directory:
mkdir -p ~/.local/criteria/adapters
cp bin/criteria-adapter-greeter ~/.local/criteria/adapters/
chmod +x ~/.local/criteria/adapters/criteria-adapter-greeter
```

Or use a temporary directory to avoid touching your home directory:

```bash
tmpdir=$(mktemp -d)
go build -o "$tmpdir/criteria-adapter-greeter" .
chmod +x "$tmpdir/criteria-adapter-greeter"
CRITERIA_ADAPTERS="$tmpdir" criteria apply example.hcl
```

## Run the example workflow

```bash
criteria apply example.hcl
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

`main.go` implements the v2 adapter contract from the published
[`criteria-go-adapter-sdk`](https://github.com/brokenbots/criteria-go-adapter-sdk)
(`adapterhost.Service`, protocol v2 in
[`criteria-adapter-proto`](https://github.com/brokenbots/criteria-adapter-proto))
and calls `adapterhost.Serve` from `main()`:

```go
type Service interface {
    Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error)
    OpenSession(context.Context, *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error)
    Execute(context.Context, *v2.ExecuteRequest, ExecuteEventSender) error
    Log(context.Context, *v2.LogRequest, LogEventSender) error
    CloseSession(context.Context, *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error)
    Permissions(context.Context, PermissionsStream) error
}
```

(greeter embeds `UnimplementedPermissions`, so the permission stream auto-allows.)

Criteria discovers the binary as `criteria-adapter-<name>` in the adapter
roots (`$CRITERIA_ADAPTERS`, else `~/.local/criteria/adapters`) and manages the
adapter subprocess lifecycle itself.

## SDK version note

The `go.mod` in this directory builds against the **published** external SDK
and proto modules (`github.com/brokenbots/criteria-go-adapter-sdk`,
`github.com/brokenbots/criteria-adapter-proto`), with no `replace` directives —
the same way an external adapter author would.
