# Criteria Python Adapter SDK

SDK for building Criteria adapters in Python.

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

### Noop adapter CLI

The package includes a minimal noop adapter runnable as a module:

```bash
python -m criteria_adapter_sdk
```

### Environment variables

- `CRITERIA_REMOTE_HOST` — host address to dial (required)
- `CRITERIA_REMOTE_TOKEN` — optional pre-shared token for the handshake
- `CRITERIA_REMOTE_TOKEN_FILE` — path to a file containing the token

### Docker

```bash
docker build -f examples/docker/Dockerfile -t my-adapter .
```

### Systemd

See `examples/systemd/criteria-adapter.service`.
