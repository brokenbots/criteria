# Remote Adapter Deployment Guide

> **Status: Untested.** The `remote` environment is implemented but has had
> minimal real-world testing (see [README → Component status](../README.md#component-status)).
> Treat this guide as a design reference, not a hardened deployment runbook.

This guide explains how to deploy a Criteria adapter that runs outside the Criteria host process and "phones home" over a TLS-backed TCP connection.

There are two ways to package a remote adapter:

- **Peer mode (recommended)** — the container runs the `criteria` engine
  itself as the entrypoint (`criteria peer`). The peer supervisor launches
  and supervises the adapter child and streams **first-class supervision
  facts** back to the host. New deployments should use this mode.
- **Runner mode (legacy)** — the container runs the thin
  `criteria-adapter-remote-runner` binary, which serves the adapter's
  contract through a byte-bridge but reports no process-lifecycle truth.
  Supported for one minor release after peer parity; see the
  [deprecation plan](#deprecation-plan).

## Concepts

### Phone-home model

In the remote deployment model the adapter side is the **client** and the Criteria host is the **server**.

1. The Criteria host starts a **shim** listener on a TCP address (e.g. `0.0.0.0:7778`).
2. The adapter-side process dials the shim, completes an optional mTLS handshake, and sends a short JSON identity frame.
3. The shim verifies the identity against the workflow lockfile and the bearer token.
4. In peer mode the verified connection *is* the session transport, and the host additionally opens a supervision journal on it (see [Peer mode](#peer-mode-recommended)). In runner mode the shim bridges the connection to a local Unix-domain socket instead.

This design means:

- **No ingress required** for the adapter. Firewalls only need to allow an *outbound* connection from adapter → host.
- **No container orchestrator logic lives in Criteria**. The host is a plain TCP listener; Kubernetes, ECS, or bare-metal placement are external concerns.
- **Identity is verified at connection time**, not at pod-startup time. A restarted adapter pod can reconnect and resume in-flight steps if the workflow configures `on_crash = "respawn"`.

### The shim

The shim is created automatically when a workflow references an `environment "remote"` block. Its behaviour is controlled by the environment block:

```hcl
environment "remote" "production" {
  listen_address = "0.0.0.0:7778"
  accept_token   = env("CRITERIA_REMOTE_TOKEN")

  mtls {
    server_cert = "/etc/criteria/certs/server.pem"
    server_key  = "/etc/criteria/certs/server-key.pem"
    client_ca   = "/etc/criteria/certs/adapter-ca.pem"
    client_identity_pattern = "CN=criteria-adapter-.*"
  }
}
```

| Attribute | Purpose |
|-----------|---------|
| `listen_address` | TCP or Unix socket where the shim listens for adapter connections. |
| `accept_token` | Optional bearer token. The adapter must send the same value in its identity handshake. |
| `mtls` | Optional mutual-TLS block. When present the shim requires a client certificate signed by `client_ca` and optionally matches the certificate subject against `client_identity_pattern`. |

### Identity verification

When an adapter connects it sends a single JSON line:

```json
{"name":"greeter","version":"0.1.0","digest":"sha256:abc123...","token":"smoke-token"}
```

The shim validates, in order:

1. **mTLS** (if configured) — TLS handshake must present a client certificate signed by the configured CA.
2. **Client identity pattern** (if configured) — the certificate subject must match the regex.
3. **Lockfile digest** — the `digest` field must match the lockfile entry for the adapter type being requested.
4. **Accept token** (if configured) — the `token` field must match `accept_token`.

If any check fails the connection is closed immediately and an error is logged.

After the four gates pass, the identity frame's `role` decides the routing:

- `role: "peer"` → the connection is handed to the host's **peer acceptor**
  (peer mode; see below).
- any other value (or no `role`) → **runner mode**: the shim creates a local
  Unix-domain socket and bridges the connection byte-for-byte, so the host's
  session layer talks to the adapter through the socket as if it were a local
  subprocess.

## Peer mode (recommended)

### Overview

Peer mode packages the **engine-side peer supervisor** into the adapter container. The container entrypoint is the `criteria` binary itself running `criteria peer`:

- The peer **resolves and launches the adapter child** itself (manifest →
  `CRITERIA_ADAPTER_BINARY` → conventional `/usr/local/bin` path → `PATH`),
  then supervises it directly as a local go-plugin child.
- The peer serves the **full v2 AdapterService contract plus the PeerService
  supervision protocol** on the phone-home connection. There is no separate
  shim-side Unix-domain socket and no byte-bridge: the verified connection is
  the transport for both.
- The child **survives host disconnects**. Every reconnect re-dials,
  re-handshakes (same token, scope, and digest), and re-serves both services.
- Process-lifecycle facts cross the wire as **typed supervision events**
  instead of being inferred from gRPC transport errors, with a bounded
  in-peer journal that replays crash evidence after reconnects.

### Phone-home topology

```
adapter container (pod)                                criteria host
──────────────────────                                 ─────────────
┌───────────────────────────────────┐
│ ENTRYPOINT: criteria peer         │
│                                   │
│  resolves adapter child:          │
│  manifest → CRITERIA_ADAPTER_     │
│  BINARY → conventional path →     │
│  PATH; launches it as a local     │
│  go-plugin child and supervises   │
│  it (crash taxonomy, journal)     │
└───────────────┬───────────────────┘
                │
                │ 1. outbound dial to shim listen_address
                │    (10s dial timeout, 15s TCP keepalive),
                │    optional mTLS (TLS 1.2+)
                │ 2. one newline-terminated JSON identity
                │    frame, ≤ 16 KiB (role: "peer")
                ▼
                                    ┌───────────────────────────────────────┐
                                    │ shim (environment "remote")           │
                                    │  mTLS → identity pattern → lockfile   │
                                    │  digest → accept/scope token          │
                                    │  (constant-time compares)             │
                                    │                                       │
                                    │  role == "peer" ⇒ hand the verified   │
                                    │  connection to the peer acceptor:     │
                                    │  NO UDS, NO byte-bridge — the         │
                                    │  connection itself is the transport   │
                                    └───────────────┬───────────────────────┘
                                                    │
                                                    │ 3. host ⇄ peer gRPC on the held
                                                    │    connection (the PEER is the
                                                    │    gRPC server):
                                                    │      Supervise(since_event_seq)
                                                    │        → typed journal stream
                                                    │      Control(KillChild)
                                                    │    plus the v2 AdapterService the
                                                    │    host calls for session work
                                                    ▼
```

### Handshake schema

The peer writes a single newline-terminated JSON line immediately after the transport connection is established, before any gRPC traffic. The frame is capped at 16 KiB. Older shims tolerate unknown fields, so newer peers stay compatible.

```json
{
  "name": "shell",
  "version": "0.5.2",
  "digest": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "token": "REPLACE_ME_WITH_A_STRONG_TOKEN",
  "scope": "engagements/acme",
  "sdk_protocol_version": 2,
  "role": "peer",
  "peer": {
    "criteria_version": "0.5.30",
    "capabilities": ["adapter.v2.full", "supervision.v1"]
  }
}
```

| Field | Meaning |
|-------|---------|
| `name` / `version` / `digest` | Adapter identity. Same lockfile + token verification as runner mode. |
| `token` | The bearer token for the shim's accept gate (or the per-scope token when `per_scope_sessions = true`). |
| `scope` | `"scopeName/scopeInstanceID"` the peer reports on supervision events; empty in run-wide mode. |
| `sdk_protocol_version` | Wire protocol version; `2` for the v2 adapter protocol. |
| `role` | `"peer"` routes the dial to the peer acceptor (any other value takes the legacy runner path). |
| `peer.criteria_version` | The peer's engine version, for host-side compatibility checks. |
| `peer.capabilities` | `adapter.v2.full` (full v2 contract served on the connection) and `supervision.v1` (PeerService journal). Protocol evolution is negotiated with these strings, not proto churn. |

### Supervision protocol summary

PeerService is defined in `proto/criteria/v1/peer.proto`. The peer is the gRPC server on the phone-home connection; the host opens the stream and reads lifecycle truth.

| RPC | Direction | Purpose |
|-----|-----------|---------|
| `Supervise(SupervisionRequest) → stream SupervisionEvent` | host → peer | Open/reopen the supervision journal. `since_event_seq` replays strictly after that value (exclusive); `0` replays the journal from the beginning. |
| `Control(ControlRequest) → ControlResponse` | host → peer | Control plane. Stage A carries `KillChild` with a grace period; the peer acknowledges with `accepted` + detail. |

Each `SupervisionEvent` is a typed process-lifecycle fact with a **gapless, globally monotonic `event_seq` per peer process**:

| Event | Carries | Notes |
|-------|---------|-------|
| `spawned` | `binary`, `digest`, `version`, `pid` | The child was launched. |
| `exited` | `exit_code`, `signal`, `idle_ms`, `graceful` | Clean exit or explicit kill (`exit_code = -1` when signalled). |
| `crash` | `reason`, `detail` | Peer's classification. `reason` is one of the shared crash-reason strings below. |
| `flushed` | `channel` (`"log"`), `up_to_seq` | A stream channel drained; monotonic per channel. |
| `heartbeat` | `last_event_seq` | Emitted every 30s while idle; a journal-lag probe. |

Delivery semantics:

- **At-least-once**: consumers dedup on (peer connection identity, `event_seq`).
- **Terminal events survive reconnects**: `exited` and `crash` records persist to the bounded in-peer journal and are replayed **first**, so crash evidence survives the RPC stream that died.
- **Classification outranks heuristics**: when the journal delivers a `crash` reason, the host consumes it verbatim instead of guessing from transport errors.

### Crash-reason vocabulary

`CrashClassified.reason` is one of the shared taxonomy constants (single source of truth: `internal/adapterhost/crashreason.go`; both host-side crash mapping and peer emission consume the same set):

| Reason | Diagnosis |
|--------|-----------|
| `adapter process exited before the call completed` | The adapter process died before the in-flight call completed — the most precise diagnosis. |
| `adapter process terminated` | The adapter process was terminated (e.g. OOM kill, external signal). |
| `log-stream heartbeat stall (adapter stopped streaming)` | The adapter stopped streaming its log stream and the stall watchdog tripped. |
| `gRPC client transport closed (adapter or shim closed the connection)` | The transport closed underneath a live session. |
| `gRPC endpoint unavailable (adapter process gone)` | The adapter's gRPC endpoint is gone. |
| `plugin stdio pipe broken (adapter process died)` | The plugin stdio pipe broke because the adapter process died. |
| `plugin stdio EOF (adapter process exited or closed its stream)` | The stdio stream hit EOF. |
| `unknown adapter error` | An error matching no known transport or process signature. |
| `unknown` | Neither process state nor an error was available to classify. |

### Backoff, keepalive, and lifecycle policy

| Knob | Value | Notes / override |
|------|-------|------------------|
| Dial timeout | 10s | Fixed; bounds a single phone-home dial attempt. |
| TCP keepalive | 15s | Fixed keepalive period on phone-home dials. |
| Reconnect backoff | 1s floor → 30s ceiling, **full jitter** | The ceiling doubles per attempt until the max, so restart storms never lockstep. Override with `CRITERIA_PEER_BACKOFF_MIN` / `CRITERIA_PEER_BACKOFF_MAX`. |
| Supervision heartbeat | every 30s while idle | Complements, never replaces, transport keepalive. |
| Handshake frame cap | 16 KiB | Larger frames are rejected on both sides. |
| Journal limit | 4096 events | Bounded replay buffer. Override with `CRITERIA_PEER_JOURNAL_LIMIT`. |
| Child keepalive across host disconnects | on | `CRITERIA_PEER_CHILD_KEEPALIVE=false` is reserved for host-driven teardown. |
| Peer shutdown budget | 30s | Bounds the whole shutdown sequence (session drain + child stop). |

### Container image

Build the peer image with [`images/remote-adapters/Dockerfile.peer`](../images/remote-adapters/Dockerfile.peer):

```bash
docker buildx build \
  --build-arg ADAPTER_NAME=shell \
  --build-arg ADAPTER_VERSION=0.5.2 \
  --build-arg CRITERIA_VERSION=v0.5.30 \
  -f images/remote-adapters/Dockerfile.peer \
  -t ghcr.io/your-org/criteria-adapter-shell-peer:0.5.2 .
```

The image builds `criteria` from this repo (so the peer and the host share one engine version) and pulls the adapter binary from its public module. It requires an explicit `CRITERIA_VERSION` (fail-closed, same policy as `Dockerfile.runtime`) because the peer handshake advertises `criteria_version`. The runtime keeps the alpine/debian package set of the legacy runner image and runs as the non-root `criteria` user (uid 10001).

### Peer environment variables

Set by the manifest; consumed by the `criteria peer` entrypoint.

| Variable | Purpose |
|----------|---------|
| `CRITERIA_REMOTE_HOST` | **Required.** `host:port` (or unix path) of the orchestrator's peer stream. The peer exits non-zero without it. |
| `CRITERIA_REMOTE_TOKEN` | The bearer token for the peer stream. |
| `CRITERIA_REMOTE_SCOPE` | Scope reported on supervision events (`scopeName/scopeInstanceID`). |
| `CRITERIA_REMOTE_DIGEST` | Pinned OCI digest; a digest-addressed artifact in the local adapter cache is preferred over `PATH` resolution. |
| `CRITERIA_REMOTE_TLS_CERT` / `CRITERIA_REMOTE_TLS_KEY` / `CRITERIA_REMOTE_CA` | Client cert, key, and CA bundle paths. All-or-nothing; TLS 1.2+. Partial settings are rejected. |
| `CRITERIA_ADAPTER_NAME` | Adapter name (a manifest wins if also set). |
| `CRITERIA_ADAPTER_VERSION` | Adapter version (default `"0.0.0"` unless manifest or child reports one). |
| `CRITERIA_ADAPTER_BINARY` | Explicit child binary path (beats `PATH`). |
| `CRITERIA_ADAPTER_MANIFEST` | Manifest file; fills name/version and the conventional binary path. |
| `CRITERIA_PEER_CHILD_KEEPALIVE` | `"true"` (default): the child survives host disconnects. |
| `CRITERIA_PEER_JOURNAL_LIMIT` | Bounded supervision journal size (default 4096). |
| `CRITERIA_PEER_BACKOFF_MIN` / `CRITERIA_PEER_BACKOFF_MAX` | Reconnect backoff floor/ceiling (default 1s / 30s). |
| `CRITERIA_LOG_LEVEL` | `debug`, `info` (default), `warn`, or `error`. |

Every `CRITERIA_REMOTE_*` variable is scrubbed from the adapter child's environment (prefix-based, including `CRITERIA_REMOTE` itself), so the child cannot sniff into phone-home mode.

### Kubernetes deployment (peer mode)

The manifests below deploy a peer-mode adapter into a Kubernetes cluster. They assume:

- A Criteria host is reachable from the cluster at `criteria.example.com:7778`.
- You have built a peer image for your adapter (see [Container image](#container-image)).
- You have generated mTLS certificates (see [Certificate generation](#certificate-generation)).

The namespace and ConfigMap manifests are shared with runner mode; the Secret carries the stream token.

```yaml
# namespace + configmap: docs/examples/k8s-remote-adapter/ (runner-mode
# example files; identical shapes work for peer mode)
apiVersion: v1
kind: Namespace
metadata:
  name: criteria-remote
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: criteria-remote-config
  namespace: criteria-remote
data:
  host: "criteria.example.com:7778"
```

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: criteria-remote-secret
  namespace: criteria-remote
type: Opaque
stringData:
  token: "REPLACE_ME_WITH_A_STRONG_TOKEN"
```

The Deployment runs the peer image. The image's `ENTRYPOINT` is already `/usr/local/bin/criteria peer`, so no `command` override is needed; the env block is the whole contract:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: greeter-adapter
  namespace: criteria-remote
spec:
  replicas: 1
  selector:
    matchLabels:
      app: greeter-adapter
  template:
    metadata:
      labels:
        app: greeter-adapter
    spec:
      containers:
        - name: peer
          image: ghcr.io/your-org/criteria-adapter-greeter-peer:v0.1.0
          # ENTRYPOINT /usr/local/bin/criteria peer — no command override.
          resources:
            requests:
              memory: "64Mi"
              cpu: "100m"
            limits:
              memory: "256Mi"
              cpu: "500m"
          env:
            # Orchestrator endpoint (required).
            - name: CRITERIA_REMOTE_HOST
              valueFrom:
                configMapKeyRef:
                  name: criteria-remote-config
                  key: host
            # Stream auth + identity.
            - name: CRITERIA_REMOTE_TOKEN
              valueFrom:
                secretKeyRef:
                  name: criteria-remote-secret
                  key: token
            # Set only with per_scope_sessions = true (the operator injects
            # the scope_name/scope_instance_id from provision_wanted).
            - name: CRITERIA_REMOTE_SCOPE
              value: "engagements/acme"
            # Optional; the operator fills this from the pinned digest on
            # provision_wanted. Prefers a digest-addressed cache artifact.
            - name: CRITERIA_REMOTE_DIGEST
              value: "sha256:REPLACE_WITH_LOCKFILE_PINNED_DIGEST"
            # mTLS (all three are all-or-nothing).
            - name: CRITERIA_REMOTE_TLS_CERT
              value: "/etc/criteria/certs/tls.crt"
            - name: CRITERIA_REMOTE_TLS_KEY
              value: "/etc/criteria/certs/tls.key"
            - name: CRITERIA_REMOTE_CA
              value: "/etc/criteria/certs/ca.crt"
            # Adapter identity (baked into the image, but overridable).
            - name: CRITERIA_ADAPTER_NAME
              value: "greeter"
          volumeMounts:
            - name: certs
              mountPath: /etc/criteria/certs
              readOnly: true
      volumes:
        - name: certs
          secret:
            secretName: greeter-adapter-tls
```

Certificate generation is identical for both modes — see [Certificate generation](#certificate-generation).

### Docker Compose (peer mode)

```yaml
services:
  criteria:
    image: ghcr.io/brokenbots/criteria:latest
    command: ["apply", "/workspace/workflow.hcl"]
    ports:
      - "7778:7778"
    volumes:
      - ./workflow.hcl:/workspace/workflow.hcl:ro
      - ./certs:/etc/criteria/certs:ro
    environment:
      - CRITERIA_REMOTE_TOKEN=${CRITERIA_REMOTE_TOKEN:-smoke-token}

  adapter:
    image: ghcr.io/your-org/criteria-adapter-greeter-peer:v0.1.0
    environment:
      - CRITERIA_REMOTE_HOST=criteria:7778
      - CRITERIA_REMOTE_TOKEN=${CRITERIA_REMOTE_TOKEN:-smoke-token}
      - CRITERIA_ADAPTER_NAME=greeter
    depends_on:
      - criteria
```

The peer retries its connection with full-jitter backoff (1s → 30s) until the shim is ready, so startup ordering does not need a health gate.

### The operator pattern: reconcile on `provision_wanted`

With `per_scope_sessions = true` on the remote environment, the host emits **`adapter.lifecycle` events** (visible on the ND-JSON event stream and the server event stream) that are the whole contract an external operator needs:

- **`provision_wanted`** — emitted when a scope needs an adapter pod. It carries `run_id`, `scope_name`, `scope_instance_id`, `adapter_name`/`adapter_type`, the lockfile-pinned `digest`, the lockfile-pinned `image_reference`, the shim's `shim_listen_address`, a `token_ref` (path to the rotated accept-token file under the run data directory) plus the wire-delivered `token`, and the `environment_type`/`environment_name` of the declaring environment block.
- **`released`** — emitted at teardown before the scope token is unregistered, so a torn-down pod cannot reconnect with the old token. `token_ref` is carried; the `token` field is empty.

The operator reconciles on these events:

1. **`provision_wanted`** → create (or keep) a pod for the adapter type, injecting `shim_listen_address` as `CRITERIA_REMOTE_HOST`, the delivered (or file-read) token as `CRITERIA_REMOTE_TOKEN`, the `scope_name/scope_instance_id` as `CRITERIA_REMOTE_SCOPE`, and `CRITERIA_REMOTE_DIGEST` from the pinned digest. `image_reference` lets the operator pull the exact pinned image instead of guessing from the adapter kind.
2. **`released`** → delete the pod (or scale the owning workload to zero). The token is unregistered immediately after, so late reconnects fail closed.

**Pods are the adapter isolation units**: one pod per adapter type per scope instance. The host stays a plain TCP listener; all placement, scaling, and credential delivery decisions are the operator's, driven by the lifecycle events above. Reconciliation is idempotent — a repeated `provision_wanted` for the same `scope_instance_id` should be a no-op.

### Troubleshooting peer deployments

The supervision journal turns "the adapter misbehaved" into concrete facts. Diagnose by reading the `SupervisionEvent` stream (each row: the observable symptom, the supervision fact, the diagnosis):

| Symptom | Supervision fact | Diagnosis |
|---------|------------------|-----------|
| Step fails, host never saw a crash | Only `exited` with `graceful = true`, no `crash` | Clean child exit mid-session: check the adapter's own exit path (bad input handling, deliberate `os.Exit`). `idle_ms` tells you how long the child sat still before exiting. |
| Step fails mid-call, host reports adapter died | `crash` with `reason = "adapter process exited before the call completed"` | The child died under load — inspect the child's stderr/log channel up to the last `flushed` event. |
| Child vanishes without an adapter log trail | `crash` with `reason = "adapter process terminated"` | External signal: OOM kill, `docker/podman` stop, or node eviction. Check the container runtime's OOM/exit records. |
| Sessions hang, then fail all at once | `crash` with `reason = "log-stream heartbeat stall (adapter stopped streaming)"` | The child stopped streaming; the stall watchdog tripped. Look for deadlock or a blocked output pipe in the adapter. |
| Crash evidence missing after a reconnect | `heartbeat` shows `last_event_seq` < the seq you expected; no terminal event replayed | The journal limit (default 4096) evicted the record, or the peer restarted (fresh `event_seq` starts a new journal). Raise `CRITERIA_PEER_JOURNAL_LIMIT` or dedup on the peer's connection identity. |
| The same supervision event appears twice | Duplicate `event_seq` on one journal stream | Expected under at-least-once delivery: dedup on `(peer connection identity, event_seq)`. |
| Host refuses the dial entirely, no journal opens | No supervision events at all; shim logs an identity-gate rejection | One of the four identity gates failed (mTLS → pattern → digest → token) before the role branch. See the connection-level [troubleshooting](#troubleshooting) below. |
| `peer role dial rejected: no peer acceptor configured` in host logs | Dial authenticated but no journal ever opens | The host's shim has no peer acceptor installed — the host binary is older than the peer. Upgrade the host. |
| Child killed by a host-issued kill | `exited` following a `Control(KillChild)`; `graceful = true` when it exited within `grace_ms`, `graceful = false` when it had to be force-terminated after the grace window | The kill came from the control plane, not a crash — cross-check `ControlResponse.accepted` in the host logs and the requested `grace_ms`. |
| Peer exits at startup, nothing listens | No events; peer log says `CRITERIA_REMOTE_HOST` missing or config unresolved | Manifest problem: `CRITERIA_REMOTE_HOST` is required, and partial TLS settings (`CRITERIA_REMOTE_TLS_CERT`/`KEY`/`CA`) are rejected — set all three or none. |

## Runner mode (legacy)

The runner image packages the generic remote runner binary with the requested adapter binary. At runtime the runner starts the local adapter via hashicorp/go-plugin and forwards the v2 contract to the remote host using the SDK's `ServeRemote`; the host shim bridges the connection to a local Unix-domain socket. Lifecycle RPCs (pause/resume/snapshot/restore/inspect) work in this mode too, but the host learns about adapter crashes only indirectly — there is no supervision journal.

**This mode is deprecated** on the timeline documented in the [deprecation plan](#deprecation-plan). New deployments should use [peer mode](#peer-mode-recommended); the manifests below remain valid for existing deployments.

### Kubernetes deployment (runner)

```yaml
# docs/examples/k8s-remote-adapter/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: greeter-adapter
  namespace: criteria-remote
spec:
  replicas: 1
  selector:
    matchLabels:
      app: greeter-adapter
  template:
    metadata:
      labels:
        app: greeter-adapter
    spec:
      containers:
        - name: adapter
          image: ghcr.io/your-org/criteria-adapter-greeter:v0.1.0
          resources:
            requests:
              memory: "64Mi"
              cpu: "100m"
            limits:
              memory: "256Mi"
              cpu: "500m"
          env:
            - name: CRITERIA_REMOTE_HOST
              valueFrom:
                configMapKeyRef:
                  name: criteria-remote-config
                  key: host
            - name: CRITERIA_REMOTE_TOKEN
              valueFrom:
                secretKeyRef:
                  name: criteria-remote-secret
                  key: token
            - name: CRITERIA_REMOTE_TLS_CERT
              value: "/etc/criteria/certs/tls.crt"
            - name: CRITERIA_REMOTE_TLS_KEY
              value: "/etc/criteria/certs/tls.key"
            - name: CRITERIA_REMOTE_CA
              value: "/etc/criteria/certs/ca.crt"
          volumeMounts:
            - name: certs
              mountPath: /etc/criteria/certs
              readOnly: true
      volumes:
        - name: certs
          secret:
            secretName: greeter-adapter-tls
```

### Docker Compose (runner)

For local trial without a Kubernetes cluster, use Docker Compose to run both Criteria and the adapter side-by-side.

```yaml
# docs/examples/compose-remote-adapter/docker-compose.yml
services:
  criteria:
    image: ghcr.io/brokenbots/criteria:latest
    command: ["apply", "/workspace/workflow.hcl"]
    ports:
      - "7778:7778"
    volumes:
      - ./workflow.hcl:/workspace/workflow.hcl:ro
      - ./certs:/etc/criteria/certs:ro
    environment:
      - CRITERIA_REMOTE_TOKEN=${CRITERIA_REMOTE_TOKEN:-smoke-token}
    healthcheck:
      test: ["CMD", "criteria", "version"]
      interval: 5s
      timeout: 3s
      retries: 5

  adapter:
    image: ghcr.io/your-org/criteria-adapter-greeter:v0.1.0
    environment:
      - CRITERIA_REMOTE_HOST=criteria:7778
      - CRITERIA_REMOTE_TOKEN=${CRITERIA_REMOTE_TOKEN:-smoke-token}
    depends_on:
      criteria:
        condition: service_healthy
```

Example workflow (`workflow.hcl`):

```hcl
workflow {
  name = "compose-remote-demo"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "default" {
  listen_address = "0.0.0.0:7778"
  accept_token   = "smoke-token"
}

adapter "greeter" "default" {
  environment = remote.default
}

step "run" {
  target = adapter.greeter.default
  input {
    name = "world"
  }
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
```

Run it:

```bash
cd docs/examples/compose-remote-adapter
docker compose up --build
```

The adapter container will retry its connection every few seconds until the Criteria shim is ready.

### Adapter entrypoint

Your adapter binary should use the SDK's remote entrypoint. In Go this looks like:

```go
package main

import (
    "os"

    "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

func main() {
    tlsConf, err := adapterhost.LoadClientTLS(
        os.Getenv("CRITERIA_REMOTE_TLS_CERT"),
        os.Getenv("CRITERIA_REMOTE_TLS_KEY"),
        os.Getenv("CRITERIA_REMOTE_CA"),
    )
    if err != nil {
        panic(err)
    }
    if err := adapterhost.ServeRemote(&myAdapter{}, &adapterhost.ServeRemoteOptions{
        Host:        os.Getenv("CRITERIA_REMOTE_HOST"),
        TLSConfig:   tlsConf,
        AcceptToken: os.Getenv("CRITERIA_REMOTE_TOKEN"),
        Identity: adapterhost.RemoteIdentity{
            Name:    "greeter",
            Version: "0.1.0",
            Digest:  os.Getenv("CRITERIA_REMOTE_DIGEST"),
        },
        Reconnect: true,
    }); err != nil {
        panic(err)
    }
}
```

Equivalent entrypoints exist in the TypeScript (`serveRemote`) and Python
(`serve_remote`) SDK packages. These remain **functional but deprioritized**:
they still work against the runner path, while the peer path (recommended)
supervises the adapter child directly and needs no in-SDK remote loop. See
[docs/adapter-v2-migration.md](adapter-v2-migration.md).

## Certificate generation

#### Option A — cert-manager

If your cluster runs [cert-manager](https://cert-manager.io), create an Issuer (or use a cluster-wide one) and a Certificate:

```yaml
# docs/examples/k8s-remote-adapter/cert-manager.yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: greeter-adapter-tls
  namespace: criteria-remote
spec:
  secretName: greeter-adapter-tls
  issuerRef:
    name: criteria-ca-issuer
    kind: ClusterIssuer
  commonName: criteria-adapter-greeter
  dnsNames:
    - greeter-adapter.criteria-remote.svc.cluster.local
  usages:
    - client auth
```

The host shim must trust the CA referenced by `criteria-ca-issuer`.

#### Option B — cfssl

For ad-hoc or local clusters, generate certificates with [cfssl](https://github.com/cloudflare/cfssl):

```bash
# 1. Create a CA
cat > ca-csr.json <<EOF
{
  "CN": "Criteria Remote CA",
  "key": { "algo": "ecdsa", "size": 256 },
  "names": [{ "O": "Criteria" }]
}
EOF
cfssl gencert -initca ca-csr.json | cfssljson -bare ca

# 2. Create a client certificate for the adapter
cat > adapter-csr.json <<EOF
{
  "CN": "criteria-adapter-greeter",
  "key": { "algo": "ecdsa", "size": 256 },
  "names": [{ "O": "Criteria" }]
}
EOF
cfssl gencert \
  -ca=ca.pem -ca-key=ca-key.pem \
  -config=ca-config.json \
  -profile=client \
  adapter-csr.json | cfssljson -bare adapter

# 3. Create a server certificate for the host shim
cat > server-csr.json <<EOF
{
  "CN": "criteria-host",
  "hosts": ["criteria.example.com", "localhost"],
  "key": { "algo": "ecdsa", "size": 256 }
}
EOF
cfssl gencert \
  -ca=ca.pem -ca-key=ca-key.pem \
  -config=ca-config.json \
  -profile=server \
  server-csr.json | cfssljson -bare server
```

Create the Kubernetes Secret manually:

```bash
kubectl create secret tls greeter-adapter-tls \
  --cert=adapter.pem --key=adapter-key.pem \
  -n criteria-remote
```

The host shim mounts `server.pem`, `server-key.pem`, and `ca.pem`.

### Apply the manifests

```bash
# Runner-mode example (legacy):
kubectl apply -f docs/examples/k8s-remote-adapter/

# Peer mode: apply the Deployment from the peer section above.
```

## Troubleshooting

### Connection refused / timeout from adapter to host

- Verify the Criteria host is listening on the configured `listen_address`.
- Check firewall rules between the adapter and the host. The adapter needs **outbound** TCP to the host address.
- If running Criteria inside a container or VM, ensure the shim port is published or forwarded to an address reachable by the adapter.
- From inside the adapter pod, test reachability with `nc -zv <host> <port>`.

### Certificate / mTLS errors

- Ensure the adapter's client certificate is signed by the CA configured in the host's `client_ca`.
- Check certificate expiry.
- Verify `client_identity_pattern` (if set) matches the certificate subject. The shim logs the extracted subject on mismatch.
- Ensure the full certificate chain is sent by the adapter. Some TLS libraries require explicit chain configuration.

### Identity-mismatch / digest verification failed

- The adapter's `digest` in the handshake must match the lockfile entry for its type. Run `criteria compile` to generate an updated lockfile if the adapter binary changed.
- The adapter's `name` in the handshake must match the adapter type referenced in the workflow (`adapter.greeter.default` → name must be `greeter`).
- If `accept_token` is configured, both sides must use the exact same value.

### Adapter crash-loops but workflow does not resume

- Verify the workflow step sets `on_crash = "respawn"`. Without this the engine treats a disconnect as a fatal step failure.
- Check that the adapter has a reconnection loop with back-off. The SDK `ServeRemote` implementations include this by default; the peer supervisor includes it always.
- Ensure the adapter's Kubernetes liveness probe does not kill the pod faster than the engine's respawn timeout.

### Pod restarts but old session lingers

- The shim keeps one active session per adapter type. When a new connection arrives for the same type, the old session is closed and replaced. This is normal during rolling updates.
- If you need multiple replicas of the same adapter type, use distinct adapter types or run them against separate Criteria host instances. With `per_scope_sessions = true`, peer-mode pods are keyed by scope instead — see the [operator pattern](#the-operator-pattern-reconcile-on-provision_wanted).

## Deprecation plan

The runner mode — the `criteria-adapter-remote-runner` binary plus the host shim's UDS/byte-bridge path — is **deprecated**:

- **Today**: peer mode is the recommended deployment; runner mode remains fully supported and all documented manifests keep working.
- **Runner + byte-bridge supported through one minor release after peer parity.** Runner-mode fixes are limited to crash/security issues; new remote features land in peer mode only.
- **Then**: a removal ticket deletes the legacy path. The ticket must remove these exact symbols:

| Symbol | Location | Role in the legacy path |
|--------|----------|-------------------------|
| `Shim.setupUDS` | `internal/adapter/environment/remote/shim.go` | Creates the shim-side Unix-domain socket for the byte-bridge. |
| `Shim.bridgeAndDial` | `internal/adapter/environment/remote/shim.go` | Byte-copies between the phone-home connection and the UDS and dials the local endpoint. |
| `adapterhost.LocalSocketDialer` (remote `LocalSocketDialer` path) | `internal/adapterhost/loader_reattach.go` (called from `shim.go`) | Reattach-mode dialer the bridge hands its socket to; only the remote call site goes away with runner mode. |
| `noopAttachedRunner` | `internal/adapterhost/loader_reattach.go` | AttachedRunner placeholder used because the real runner is out of process in the remote path. |
| Runner proxy (`proxyService` / `grpcAdapterServer` surface) | `cmd/criteria-adapter-remote-runner/serve_remote.go` | The runner's gRPC adapter-service proxy server: the `grpc.NewServer` + adapter-service registration that serves the child's contract on the phone-home connection, together with its dial/handshake helpers (`serveRemoteOnce`, `dialRemote`, `sendRemoteHandshake`, `singleConnListener`). |

The TypeScript and Python `ServeRemote` entrypoints remain functional but deprioritized (see [docs/adapter-v2-migration.md](adapter-v2-migration.md)); their removal is a separate SDK-side decision, not part of this engine-side plan.
