# Criteria Go Adapter SDK

Go package `github.com/brokenbots/criteria/sdk/adapterhost` for building Criteria adapters.

## Running as a remote adapter

`ServeRemote` dials a criteria host, sends the v2 identity handshake, and bridges the local gRPC service over the resulting TCP connection.

```go
import (
    "github.com/brokenbots/criteria/sdk/adapterhost"
    v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
)

opts := &adapterhost.ServeRemoteOptions{
    Host: "criteria.example.com:7778",
    Identity: adapterhost.RemoteIdentity{
        Name:    "my-adapter",
        Version: "1.0.0",
        Digest:  "sha256:abc123...",
    },
    AcceptToken: os.Getenv("CRITERIA_REMOTE_TOKEN"),
}

if err := adapterhost.ServeRemote(myService, opts); err != nil {
    log.Fatal(err)
}
```

### Environment variables

- `CRITERIA_REMOTE_HOST` — host address to dial
- `CRITERIA_REMOTE_TOKEN` — optional pre-shared token for the handshake
- `CRITERIA_REMOTE_TOKEN_FILE` — path to a file containing the token

### Docker

See `examples/docker/Dockerfile`.

### Systemd

See `examples/systemd/criteria-adapter.service`.
