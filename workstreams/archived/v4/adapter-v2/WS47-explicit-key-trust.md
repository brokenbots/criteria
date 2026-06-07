# WS47 — Explicit-key signing + lockfile trust anchor (enterprise)

**Phase:** Adapter v2 · **Track:** Signing completion (WS06 follow-up) · **Owner:** Workstream executor · **Depends on:** WS46, WS06, WS07. · **Unblocks:** WS48 (shares the lock→policy wiring); enterprise strict-verify. · **Base branch:** `adapter-v2`

## Context

Two of the three signing layers WS06 intended are present but not connected end-to-end:

- **Signer (publish):** `internal/adapter/publish/sign.go` has a working `KeySigner` (Ed25519) and the publish CLI/action expose `--sign-key`. `TestSign_KeyMode_RoundTripVerifies` passes.
- **Verifier:** `internal/adapter/signing/verify.go` `verifyKeyBased` verifies a signature against `Policy.TrustedKeys` (match by fingerprint, then `VerifySignature`). The lockfile schema (`workflow/lockfile/types.go` `LockedSignature.Key{Algorithm,Fingerprint}`) already has a slot for the pinned key, and `lock` records it via `lockfile.BuildEntry`.

**The missing link:** nothing populates `Policy.TrustedKeys` at verify time, and the engine's `lockfileDigestVerifier` (`internal/engine/engine.go` ~line 772/804) only checks the **digest** — it never feeds the lockfile's pinned **signer** into the verify policy. So a key-signed artifact cannot actually be verified by `lock`/`apply` today.

Goal (enterprise track): strong validation with **known keys**. Establish the model **"the lockfile is the trust anchor"** — `lock` pins the signer; `apply`/`pull` enforce it. This WS implements that wiring for key mode; WS48 reuses it for keyless identity.

## Prerequisites

WS46 merged (override resolver + modes). WS06/WS07 present.

## In scope

### Step 1 — Trusted-keys configuration surface

Enterprises declare which public keys they trust. Add a trusted-keys source loaded into `signing.Policy.TrustedKeys`:

- A `trusted_keys` list (PEM public keys, or paths) — choose the home: a workflow-level block and/or a global `~/.criteria/trust.hcl`. Recommend **both**, global taking union with workflow.
- Loader populates `Policy.TrustedKeys` with `{RawKey, Fingerprint}` (the verifier computes/matches fingerprints already).

### Step 2 — Lock pins the key

- On `criteria adapter lock`, for a key-signed artifact, verify against the configured trusted keys and record `LockedSignature.Key{Algorithm, Fingerprint}` (already supported by `BuildEntry`; confirm it is populated for key mode).
- Drift: if the pinned fingerprint no longer matches on re-lock, surface a `SignerChanged` lockfile diff (the `lockfile.ChangeKind` already enumerates this).

### Step 3 — Runtime enforces the pin (the wiring)

- Extend the engine's verification (`internal/engine/engine.go` `lockfileDigestVerifier`) so that, in addition to the digest check, it constructs the verify `Policy` from the lockfile entry's `LockedSignature` + the configured trusted keys and calls `signing.Verify`. For key mode: confirm the artifact's signature verifies against the pinned fingerprint's key.
- Same wiring for the `apply`/`pull` standalone paths (whatever does not go through the engine verifier).
- Respect the WS46 override (off/warn/strict, `--allow-unsigned`).

### Step 4 — Key management ergonomics

- `--sign-key` already exists for publish; document key generation (Ed25519) and distribution.
- Optional: `criteria adapter trust add/list <pubkey>` to manage `trusted.hcl`, and a `--trusted-key` flag for ad-hoc `pull`/`lock`.
- Document the enterprise flow in `docs/adapters.md` → Secrets/Signing.

## Out of scope

- Keyless / Fulcio / Rekor (WS48).
- The override mechanics themselves (WS46).

## Behavior change

Key-signed adapters can be verified end-to-end: `lock` pins the key fingerprint; `apply`/`pull` verify the artifact's signature against the configured trusted key and the pinned fingerprint. Strict mode now has a working, offline, reproducible trust path.

## Tests required

- e2e (local registry): publish with `--sign-key`, `lock` (pins fingerprint), `apply`/`pull` verify; assert success.
- Wrong/rotated key → fail closed in strict; `SignerChanged` diff on re-lock.
- `--allow-unsigned`/`verification=off` bypasses (WS46 interaction).
- Unit: trusted-keys loader; engine policy construction from a `LockedSignature.Key`.

## Exit criteria

- A key-signed adapter verifies through `lock` + `apply` against configured trusted keys, fully offline (no network, no TUF).
- Lockfile pins the fingerprint; drift is detected.
- Docs cover key generation, trust config, and the enterprise flow.

## Files this workstream may modify

- `internal/adapter/signing/verify.go` (key path only — populate/consume `TrustedKeys`), `internal/adapter/signing/policy.go`
- `internal/engine/engine.go` (feed lockfile signer → verify policy)
- `internal/cli/adapter_lock.go`, `internal/cli/adapter_pull.go`, `internal/cli/apply*.go`, optional `internal/cli/adapter_trust.go` *(new)*
- `workflow/lockfile/*` *(only if key fields need adjustment)*, trust-config schema (`workflow/schema.go` and/or a global config loader)
- `docs/adapters.md`

## Files this workstream may NOT edit

- `internal/adapter/publish/sign.go` keyless paths (WS48).
- The WS46 override resolver semantics (consume it, don't change it).
