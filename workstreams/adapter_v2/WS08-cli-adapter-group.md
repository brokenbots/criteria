# WS08 — `criteria adapter` CLI command group + compile-time auto-pull

**Phase:** Adapter v2 · **Track:** Distribution · **Owner:** Workstream executor · **Depends on:** [WS04](WS04-oci-cache-layout.md), [WS05](WS05-adapter-manifest.md), [WS06](WS06-cosign-signing.md), [WS07](WS07-lockfile.md). · **Unblocks:** every workstream that needs adapters to be installable by users; the migration WSes can finally use the new path.

## Context

`README.md` D49–D52: adapter-specific commands live under `criteria adapter <verb>` because the workflow team's `criteria pull <workflow_ref>` is the primary user entry point and pulls adapters transitively. This WS wires the OCI cache (WS04), manifest (WS05), signing (WS06), and lockfile (WS07) into user-facing verbs.

## Prerequisites

WS04, WS05, WS06, WS07 merged.

## In scope

### Step 1 — Cobra subcommand tree

Add `internal/cli/adapter.go` registering the `adapter` parent command and wiring children:

```
criteria adapter pull <ref> [--allow-unsigned] [--registry <alias>]
criteria adapter lock [--upgrade]
criteria adapter publish <path> [--registry <ref>] [--with-image]   # dev convenience; CI uses WS28 action
criteria adapter list [--installed | --referenced]
criteria adapter info <ref-or-name>
criteria adapter where <ref-or-name>
criteria adapter remove <ref-or-name>
criteria adapter prune [--older-than <duration>] [--max-size <bytes>]
criteria adapter dev <local-binary-path> [--as <type>.<name>]
```

### Step 2 — Reference resolution + alias config

`internal/cli/adapter_resolve.go`:

```go
// Resolve turns a user-supplied string into a fully-qualified oci.Reference.
//   - "ghcr.io/org/name:1.2.3"      -> as-is
//   - "name:1.2.3"                  -> looks up "name" alias in config; errors if absent
//   - "@sha256:..."                 -> requires --resolve flag (rare; for repair scenarios)
func Resolve(ctx ResolveContext, raw string) (oci.Reference, error)
```

Aliases live in `~/.criteria/config.hcl` (global) and as `registry "<alias>" { source = "ghcr.io/org" }` blocks in the workflow HCL (per-workflow). Workflow aliases override global. Add config parsing in this WS — it's small.

### Step 3 — `pull` verb

`internal/cli/adapter_pull.go`:

1. Resolve input → `oci.Reference`.
2. Build the `signing.Policy` from CLI flags + workflow/global config.
3. Call `oci.Puller.Pull(ctx, ref)` → digest.
4. Open the artifact with `oci.Layout.Open(digest)`; read `adapter.yaml`.
5. Validate manifest (`manifest.Manifest.Validate()`).
6. Verify signature against policy (`signing.Verify(...)`); fail per mode.
7. **Platform check** (per `README.md` D12c-alt): fail closed if host's `GOOS/GOARCH` is not in `manifest.Platforms`, with the publisher-pointing error message.
8. **Container-image fetch** if `manifest.ContainerImage != nil` and the active environment is `container`-mode (D12c.1): pull the additional image blob.
9. Update the lockfile via `lockfile.BuildEntry(...)` + `lockfile.Write(...)`.
10. Print a summary of what was pulled, the resolved digest, and the signer identity.

### Step 4 — `lock` verb

`internal/cli/adapter_lock.go`:

1. Parse the workflow(s) in the current directory.
2. Collect every `adapter "<type>" "<name>"` reference (the parser already produces `FSMGraph.Adapters`).
3. For each adapter that already has a lockfile entry: optionally re-resolve (with `--upgrade` flag) or keep the pinned digest.
4. For each adapter without an entry: call `Resolve(...)` and `Pull(...)` to populate.
5. Detect stale entries (in lockfile, not in workflow) and prune them.
6. Print the `lockfile.Diff(old, new)` summary.
7. Write the new lockfile via canonical writer.

### Step 5 — `publish` verb (dev only)

`internal/cli/adapter_publish.go`:

1. Take a local path to a built adapter binary (one platform).
2. Run the binary with `--emit-manifest` to extract `adapter.yaml`.
3. Construct an OCI artifact (per `README.md` D10–D11; reuse the WS28 publish-action's logic via a shared library function — extract it here in `internal/adapter/publish/`).
4. Optionally build + sign a runnable container image when `--with-image` is set (D12d).
5. Push to the configured registry (using `oras-go/v2`'s push API).

This verb is a developer convenience for "build locally, test against a workflow on the same machine" loops. CI publish uses WS28's composite action (which calls the same `internal/adapter/publish/` helpers).

### Step 6 — `list` / `info` / `where` / `remove` / `prune`

Read-only / cache-management verbs. Mostly thin wrappers over `oci.Layout` and `lockfile`:

- `list --installed`: enumerate `index.json` entries.
- `list --referenced`: enumerate workflow's lockfile entries.
- `info <ref>`: print the cached `adapter.yaml` + signer info.
- `where <ref>`: print the on-disk binary path for the host platform (useful for debugging, IDE jump-to-binary).
- `remove <ref>`: remove an entry from `index.json` and rely on `prune` to reclaim blob space, OR remove directly (config flag).
- `prune --older-than 30d --max-size 5GiB`: invoke `oci.Layout.GC(...)`.

### Step 7 — `dev` verb

`internal/cli/adapter_dev.go`: register a local binary path as `<type>.<name>` for development. Bypasses lockfile and signature verification. Errors out when the workflow has `verification = "strict"`. Stores a sentinel in the layout's index pointing at the local path (not copied into blobs — this is a dev-mode link). Sets a process-wide flag so `criteria apply` honors the dev binding.

### Step 8 — Compile-time auto-pull

In the workflow compiler (modified in WS09 for environment work; we coordinate here), on `compile`:

1. Read `.criteria.lock.hcl` from the workflow directory.
2. Validate against the parsed workflow (`lockfile.ValidateAgainstWorkflow`).
3. For each adapter reference in the workflow:
   - If the lockfile pins it and the binary is in cache: continue.
   - If the lockfile pins it but the binary isn't cached: pull silently (with progress bar on TTY).
   - If the lockfile doesn't pin it: fail with a hint to run `criteria adapter lock`.

### Step 9 — Tests

- Unit tests for each verb's argument parsing and error paths.
- An e2e test fixture (in `internal/cli/adapter_e2e_test.go`) that uses a local OCI registry container + a fake signed adapter, runs `criteria adapter pull` / `lock` / `info` and asserts results.
- Verb help text + man-page-equivalent rendered via cobra's built-in mechanism (no separate effort).

## Out of scope

- The publish-action (WS28) which is GH-Actions-specific.
- Workflow team's `criteria pull <workflow_ref>` — separate team.
- Workflow HCL changes (registry alias blocks etc.) — touched here for parsing only; full HCL extensions land in WS09.

## Reuse pointers

- All of WS04/WS05/WS06/WS07 packages.
- `internal/adapter/publish/` extracted as part of Step 5 — shared with WS28's composite action.
- Cobra command-tree patterns already used in `internal/cli/` (e.g., the `apply`/`run`/`plan` triad).

## Behavior change

**Yes — major user-visible additions.**

- New `criteria adapter ...` commands.
- `criteria compile` now requires `.criteria.lock.hcl` to be present and complete (or fails with a hint).
- Adapters are pulled into `~/.criteria/cache/oci/` instead of being expected to live at `~/.criteria/plugins/`. The legacy path still works for `criteria adapter dev` only.

## Tests required

- Verb-level tests + an e2e test against a local registry.
- All existing `criteria compile` tests updated to either (a) ship a fixture lockfile, or (b) declare `verification = "off"` and use `criteria adapter dev`.

## Exit criteria

- `criteria adapter ...` verbs all functional.
- `criteria compile` auto-pulls per the lockfile.
- e2e test green in CI.
- Help text reviewed.

## Files this workstream may modify

- `internal/cli/adapter*.go` *(new)*
- `internal/cli/root.go` registering the new parent.
- `internal/adapter/publish/*.go` *(new package shared with WS28)*
- Test fixtures.

## Files this workstream may NOT edit

- `workflow/schema.go` and `compile_*.go` — owned by WS09; this WS only consumes the compile output.
- The OCI / manifest / signing / lockfile packages — owned by WS04–WS07.
- Other workstream files.
