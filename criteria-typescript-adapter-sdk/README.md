# Criteria TypeScript Adapter SDK

SDK for building Criteria adapters in TypeScript.

## Running as a remote adapter

The `serveRemote` entry point dials a criteria host, performs the identity handshake, and bridges the local gRPC service over the resulting TCP connection.

```typescript
import { serveRemote, RemoteIdentity, ServeRemoteOptions } from "criteria-typescript-adapter-sdk";

const identity: RemoteIdentity = {
  name: "my-adapter",
  version: "1.0.0",
  digest: "sha256:abc123...",
};

const opts: ServeRemoteOptions = {
  host: "criteria.example.com:7778",
  identity,
  acceptToken: process.env.CRITERIA_REMOTE_TOKEN,
};

await serveRemote(myService, opts);
```

### Environment variables

- `CRITERIA_REMOTE_HOST` — host address to dial (e.g. `criteria.example.com:7778`)
- `CRITERIA_REMOTE_TOKEN` — optional pre-shared token for the handshake
- `CRITERIA_REMOTE_TOKEN_FILE` — path to a file containing the token

### Docker

```bash
docker build -f examples/docker/Dockerfile -t my-adapter .
```

### Systemd

See `examples/systemd/criteria-adapter.service`.
