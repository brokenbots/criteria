# Criteria Python Adapter SDK

SDK for building Criteria adapters in Python (protocol v2).

## Quick start — local adapter (`serve`)

```python
from criteria_adapter_sdk import serve, ServeConfig, pydantic_to_schema
from pydantic import BaseModel

class MyConfig(BaseModel):
    model: str = "gpt-4"
    temperature: float = 0.7

class MyInput(BaseModel):
    prompt: str

class MyOutput(BaseModel):
    text: str

async def execute(req, helpers):
    api_key = await helpers.secrets.get("ANTHROPIC_API_KEY")
    # ... run the step ...
    return {"outcome": "success", "output": {"text": "Hello"}}

serve(ServeConfig(
    name="claude",
    version="1.2.3",
    source_url="https://github.com/criteria-adapters/claude",
    capabilities=["multi_turn", "tool_calling"],
    platforms=["linux/amd64", "linux/arm64", "darwin/arm64"],
    config_schema=pydantic_to_schema(MyConfig),
    input_schema=pydantic_to_schema(MyInput),
    output_schema=pydantic_to_schema(MyOutput),
    secrets=[{"name": "ANTHROPIC_API_KEY", "required": True}],
    permissions=["read_file"],
    execute=execute,
))
```

## Running as a remote adapter

The `serve_remote` entry point dials a criteria host, performs the identity handshake, and bridges the local gRPC service over the resulting TCP connection.

```python
from criteria_adapter_sdk import serve_remote, RemoteIdentity, ServeRemoteOptions

identity = RemoteIdentity(
    name="my-adapter",
    version="1.0.0",
    digest="sha256:abc123...",
)

opts = ServeRemoteOptions(
    host="criteria.example.com:7778",
    identity=identity,
    accept_token=os.environ.get("CRITERIA_REMOTE_TOKEN"),
)

serve_remote(my_service, opts)
```

## Helper APIs

Every handler receives a `Helpers` object with:

| Property | Purpose |
|----------|---------|
| `helpers.session` | Per-session keyed get/set |
| `helpers.outcomes` | Validate outcome strings |
| `helpers.permission` | Request/deny permission correlation |
| `helpers.log` | Send structured log events |
| `helpers.secrets` | Secret-channel-only access |
| `helpers.timestamps` | Monotonic timestamps |

### Shelling out safely

Use `helpers.secrets.spawn_env([...])` to build an `env` dict that contains **only** the requested secrets. No ambient environment variables leak to the child process.

```python
env = helpers.secrets.spawn_env(["ANTHROPIC_API_KEY"])
subprocess.run(["my-tool"], env=env)
```

## `--emit-manifest`

Print the adapter manifest as YAML without starting the server:

```bash
python my_adapter.py --emit-manifest
```

## Library mode

Call an adapter directly from Python without spawning a process:

```python
from criteria_adapter_sdk.library_mode import run_in_process

result = run_in_process(config, step_name="step-1", step_config={"model": "gpt-4"})
print(result.outcome, result.outputs_json)
```

## TestHost harness

Unit-test adapters in-process:

```python
from criteria_adapter_sdk.testing import TestHost

host = TestHost(config)
host.open_session(session_id="test-1")
result = host.execute(session_id="test-1", step_name="step-1")
assert result.outcome == "success"
```

CLI:

```bash
python -m criteria_adapter_sdk.testing my_adapter:config --step-name step-1
```

### Proto generation

The vendored `.proto` files live in `protos/criteria/v2/`. To regenerate Python bindings after a proto change:

```bash
make proto
```

This requires `grpcio-tools` (`pip install grpcio-tools`).

### Binary builds

The Makefile includes a `build-binaries` target that produces Nuitka onefile executables:

```bash
make build-binaries
```

Supported targets:
- `linux-x64` — native Linux x86_64
- `linux-arm64` — cross-compile or build on arm64 host
- `darwin-arm64` — build on Apple Silicon macOS host
- `windows-x64` — future-ready (requires Windows host or cross toolchain)

### Noop adapter CLI

The package includes a minimal noop adapter runnable as a module:

```bash
python -m criteria_adapter_sdk
```

### Environment variables

- `CRITERIA_REMOTE_HOST` — host address to dial for remote mode
- `CRITERIA_REMOTE_TOKEN` — optional pre-shared token for the handshake
- `CRITERIA_REMOTE_TOKEN_FILE` — path to a file containing the token

### Docker

```bash
docker build -f examples/docker/Dockerfile -t my-adapter .
```

### Systemd

See `examples/systemd/criteria-adapter.service`.
