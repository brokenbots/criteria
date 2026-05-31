# Remote Adapter Deployment Guide

This guide explains how to deploy a Criteria adapter that runs outside the Criteria host process and "phones home" over a TLS-backed TCP connection.

## Concepts

### Phone-home model

In the remote deployment model the adapter is the **client** and the Criteria host is the **server**.

1. The Criteria host starts a **shim** listener on a TCP address (e.g. `0.0.0.0:7778`).
2. The adapter process dials the shim, completes an optional mTLS handshake, and sends a short JSON identity frame.
3. The shim verifies the adapter identity against the workflow lockfile and bearer token, then bridges the connection to a local Unix-domain socket.
4. The host's existing session layer talks to the adapter through this socket as if it were a local subprocess.

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

## Kubernetes deployment

The manifests below deploy a remote adapter into a Kubernetes cluster. They assume:

- A Criteria host is reachable from the cluster at `criteria.example.com:7778`.
- You have already built a container image for your adapter.
- You have generated mTLS certificates (see [Certificate generation](#certificate-generation)).

### Namespace and ConfigMap

```yaml
# docs/examples/k8s-remote-adapter/namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: criteria-remote
```

```yaml
# docs/examples/k8s-remote-adapter/configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: criteria-remote-config
  namespace: criteria-remote
data:
  host: "criteria.example.com:7778"
```

### Secret (bearer token)

```yaml
# docs/examples/k8s-remote-adapter/secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: criteria-remote-secret
  namespace: criteria-remote
type: Opaque
stringData:
  token: "REPLACE_ME_WITH_A_STRONG_TOKEN"
```

### Deployment

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

### Certificate generation

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
kubectl apply -f docs/examples/k8s-remote-adapter/
```

## Docker Compose deployment

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
  outcome "success" { next = "done" }
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

## Adapter entrypoint

Your adapter binary should use the SDK's remote entrypoint. In Go this looks like:

```go
package main

import (
    adapterhost "github.com/brokenbots/criteria/sdk/adapterhost"
)

func main() {
    adapterhost.ServeRemote(adapterhost.ServeRemoteOptions{
        Host:        os.Getenv("CRITERIA_REMOTE_HOST"),
        AcceptToken: os.Getenv("CRITERIA_REMOTE_TOKEN"),
        TLS: adapterhost.TLSConfig{
            CertPath: os.Getenv("CRITERIA_REMOTE_TLS_CERT"),
            KeyPath:  os.Getenv("CRITERIA_REMOTE_TLS_KEY"),
            CAPath:   os.Getenv("CRITERIA_REMOTE_CA"),
        },
        Identity: adapterhost.AdapterIdentity{
            Name:    "greeter",
            Version: "0.1.0",
        },
    }, &myAdapter{})
}
```

Equivalent entrypoints exist in the TypeScript and Python SDK packages.

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
- Check that the adapter has a reconnection loop with back-off. The SDK `ServeRemote` implementations include this by default.
- Ensure the adapter's Kubernetes liveness probe does not kill the pod faster than the engine's respawn timeout.

### Pod restarts but old session lingers

- The shim keeps one active session per adapter type. When a new connection arrives for the same type, the old session is closed and replaced. This is normal during rolling updates.
- If you need multiple replicas of the same adapter type, use distinct adapter types or run them against separate Criteria host instances.
