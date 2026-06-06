# WS48 — Keyless signing with transparency-log bundle (public)

**Phase:** Adapter v2 · **Track:** Signing completion (WS06 follow-up) · **Owner:** Workstream executor · **Depends on:** WS46, WS47 (reuses the lockfile→policy wiring). · **Unblocks:** verifiable public adapters; flip default back to `strict`. · **Base branch:** `adapter-v2`

## Context

Keyless signing is **published but not verifiable after ~10 minutes**, the central remaining gap.

- **Signer:** `internal/adapter/publish/sign.go` `KeylessSigner` requests a Fulcio leaf cert and `Sign` returns `(sig, certPEM, "", nil)` — **no Rekor (transparency-log) entry, no signed timestamp, no Sigstore bundle**. `buildSignatureManifest` records only `dev.sigstore.cosign/certificate` (+ chain) + the signature annotation; it never writes `dev.sigstore.cosign/bundle`.
- **Verifier:** `internal/adapter/signing/verify.go` `verifyKeyless` takes the bundle path only when `rec.bundleJSON != ""`; since no bundle is ever produced it always falls to `verifyKeylessLegacy`, whose core is `verify.VerifyLeafCertificate(time.Now(), cert, tm)`.

Fulcio leaf certs are **ephemeral (~10 min)**. Verifying at `time.Now()` fails for any pull/lock after the cert expires → `leaf certificate verification failed`. Keyless is *designed* to be verified at **signing time**, proven by a Rekor inclusion entry (or an RFC3161 timestamp), which `verifyKeylessLegacy` lacks.

The good news: the **correct verify path already exists and is unused** — `verifyKeylessBundle` builds a `sigstore-go` verifier with `WithTransparencyLog(1)` + `WithObserverTimestamps(1)` + `WithSignedCertificateTimestamps(1)`, which validate the cert at the log/TSA timestamp. The work is almost entirely on the **signer**: produce a bundle so that path runs.

Discovered while completing the signing chain; the two adjacent fixes already landed: signature-manifest push shape (#241) and pull-side referrer discovery (#242). With both merged, verification now *runs* and surfaces exactly this gap.

Goal (public track): keyless verification that works for any consumer/developer with no key management — "an adapter signed by its own repo's CI verifies out of the box."

## Prerequisites

WS46 (override + transition default), WS47 (lockfile→policy wiring, reused here for identity pinning).

## In scope

### Step 1 — Emit a Sigstore bundle at sign time

- Extend `KeylessSigner` (`internal/adapter/publish/sign.go`) to, after obtaining the Fulcio cert, **submit the signature to Rekor** and assemble a `sigstore-go` protobundle (cert + inclusion proof + signed entry timestamp). `sigstore-go/pkg/sign` already underpins the current Fulcio request; extend it to the full bundle flow.
- Enrich the `Signer` interface (`Sign(payload) (sig, certPEM, chainPEM, err)`) to also return the bundle bytes (e.g. a `SignResult` struct, or an optional `Bundle()` accessor). `KeySigner` returns no bundle (unchanged).
- `buildSignatureManifest` writes the bundle into the `dev.sigstore.cosign/bundle` annotation (the verifier already reads this key in `recordFromManifest`).

### Step 2 — Make verification require the bundle path

- `verifyKeyless`: prefer `verifyKeylessBundle`; turn `verifyKeylessLegacy` into a **fail-closed** with a clear message (e.g. "keyless signature has no transparency-log proof; cannot verify after certificate expiry — use --allow-unsigned for development") instead of the misleading "leaf certificate verification failed". Optionally still accept legacy *within* the cert's `NotAfter` for the ~10-min self-test window, gated behind a flag.
- Keep `verifyKeylessBundle` as-is; confirm `trustedMaterial(ctx)` (TUF) is the trust-root source.

### Step 3 — Identity trust, anchored in the lockfile

- Reuse WS47's lockfile→policy wiring for the keyless case: `lock` pins `LockedSignature.Keyless{Issuer, Subject}`; `apply`/`pull` confirm the verified bundle's identity matches the pin.
- Default `Policy.TrustedIssuers` = the GitHub Actions OIDC issuer; default `SubjectPatterns` such that **"an adapter signed by its own repo's CI" verifies** without per-consumer config (decision D-WS48-1; record the exact pattern). Enterprises can tighten.

### Step 4 — TUF trust root policy

- Resolve the WS06 open question ("Cosign keyless TUF root refresh policy: pinned vs auto-refresh"). Recommend a **pinned, cached** root for reproducibility with an explicit refresh command; document offline/air-gapped behavior (keyless verify needs the TUF root + was-online-at-sign; air-gapped consumers use WS47 key mode or `--allow-unsigned`).

### Step 5 — Restore secure default

- Flip the WS46 transition default from `warn` back to `strict` now that keyless is verifiable.

> **Decision D-WS48-1 (2026-06-06):** Default keyless policy trusts the well-known
> CI OIDC issuers (`signing.DefaultTrustedIssuers`, incl. the GitHub Actions
> issuer `https://token.actions.githubusercontent.com`) and accepts any subject
> (`*`) at first `lock`. The concrete identity is pinned into the lockfile
> (`LockedSignature.Keyless{Issuer,Subject}`) and enforced on every subsequent
> pull/apply via `cli.policyForPin` (narrows issuer+subject to the pin) +
> `cli.assertSignerMatchesPin`. Net effect: an adapter signed by its own repo's CI
> verifies with no per-consumer config, and the lockfile is the trust anchor.
> Enterprises tighten via the trust config / workflow `verification = "strict"`.
>
> **Decision D-WS48-TUF (2026-06-06):** The Sigstore TUF root is fetched via TUF
> and **cached** at `~/.criteria/cache/sigstore/` (honoring `CRITERIA_STATE_DIR`);
> once cached it is reused for reproducibility. Refresh = clear that directory (an
> explicit `criteria adapter trust refresh` command is future work). Air-gapped
> consumers cannot keyless-verify (TUF root + was-online-at-sign Rekor entry
> required) and use WS47 key mode or `--allow-unsigned`.
>
> **Verifier config:** `verifyBundleEntity` requires `WithTransparencyLog(1)` +
> `WithObserverTimestamps(1)` — the Rekor inclusion proof fixes the certificate at
> log time, so a keyless signature stays verifiable after the ~10-min Fulcio cert
> expires. (CT-log SCTs are not required; Rekor is the anchor.)
>
> **Step 5 status:** deferred to a follow-up PR. Per the release decision, the
> transition default stays `warn` (D-WS46-1) until the real-OIDC CI integration
> job is green on `adapter-v2`; the flip is the one-line change in
> `internal/cli/verification.go` (`transitionDefaultMode` → `signing.ModeStrict`).

## Out of scope

- Explicit-key mode (WS47).
- RFC3161-only (no-Rekor) timestamping — note as a future alternative if Rekor dependency is undesirable.

## Behavior change

Keyless-signed adapters are verifiable indefinitely (cert checked at log time, not `time.Now()`). Public consumers verify adapters built by their own repos' CI with no key setup. Default posture returns to `strict`.

## Tests required

- Unit: `verifyKeylessBundle` against a fixture bundle (cert + inclusion proof) — passes; expired-cert fixture still passes via log timestamp.
- Unit: `verifyKeylessLegacy` now fails closed with the documented message.
- Integration (CI, real OIDC): publish keyless → `lock` pins identity → strict `pull`/`apply` verifies; tamper → fail.
- Identity-pattern tests: self-repo subject verifies; foreign subject rejected under default policy.

## Exit criteria

- A keyless-signed adapter published by CI verifies under `strict` days later on a clean machine.
- `verifyKeylessLegacy` no longer yields a misleading error.
- Identity defaults documented; TUF root policy decided + documented.
- WS46 default returned to `strict`.

## Files this workstream may modify

- `internal/adapter/publish/sign.go` (keyless signer + `Signer` interface + bundle emission), `internal/adapter/publish/keyless*.go`, `buildSignatureManifest`
- `internal/adapter/signing/verify.go` (`verifyKeyless` dispatch, `verifyKeylessLegacy` fail-closed, identity defaults), `internal/adapter/signing/policy.go` (default issuers/subject patterns, TUF root policy)
- `internal/engine/engine.go` (keyless identity enforcement via the WS47 wiring)
- `internal/cli/verification.go` (flip transition default), `docs/adapters.md`

## Files this workstream may NOT edit

- `internal/adapter/publish/sign.go` `KeySigner` (WS47).
- The WS46 override resolver semantics.
