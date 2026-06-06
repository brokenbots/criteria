# Migrating to the v2 adapter system

The v2 adapter system is a clean break from v0.3. There is no host-side v1 shim:
v1 adapters do not load, and a few HCL surfaces changed. This guide covers what
workflow authors and adapter authors need to do.

For the full reference, see [adapters.md](adapters.md).

## What changed

| Area | v0.3 | v2 |
|---|---|---|
| Distribution | Build from source, copy the binary into `~/.criteria/adapters/` | Signed **OCI artifacts** pulled from any registry |
| Reference | Discovery by binary name (`criteria-adapter-<name>`) | `source` + `version` on the `adapter` block, resolved at lock time |
| Reproducibility | None | Per-workflow `.criteria.lock.hcl` pinning digests + signer |
| Protocol | v1 (5 RPCs) | v2 (output schema, log stream, permission stream, pause/resume/snapshot/restore/inspect) |
| Environments | `shell` only | `shell`, `sandbox`, `container`, `remote` with policy fields |
| Secrets | Plain `config` map | Dedicated secret channel, taint propagation, log redaction |

## For workflow authors

1. **Add `source` + `version` to each adapter block.** Replace name-only
   discovery with the adapter's OCI location:

   ```hcl
   adapter "claude" "assistant" {
     source  = "ghcr.io/your-org/criteria-adapter-claude"
     version = "0.5.0"
     config { max_turns = 4 }
   }
   ```

   The in-tree `copilot` and `shell` adapters were extracted to their own repos
   and are now pulled like any other adapter
   ([`criteria-adapter-copilot`](https://github.com/brokenbots/criteria-adapter-copilot),
   [`criteria-adapter-shell`](https://github.com/brokenbots/criteria-adapter-shell)).

2. **Generate the lockfile.** Run `criteria adapter lock` and commit the
   resulting `.criteria.lock.hcl`. Compilation fails with a pointer to this
   command if a workflow references OCI adapters without a lockfile.

3. **Move secrets out of `config`.** Values the adapter needs as secrets now flow
   through a `secrets { … }` block (or a step's `secret_input { … }`), not
   `config`. The compiler rejects interpolating a secret-tagged value into
   `config`/`input` with a hint. See
   [adapters.md → Secrets](adapters.md#secrets).

4. **Adopt environments as needed (optional).** `shell` behaves as before. To add
   isolation, declare a `sandbox`/`container`/`remote` environment and bind it via
   `environment = <type>.<name>`. See
   [adapters.md → Environments](adapters.md#environments).

5. **Signature verification defaults to `strict`.** If you pull adapters that are
   not yet signed, set `verification = "warn"` (or `"off"`) during the transition,
   or pull with `--allow-unsigned`. Keep CI at `strict`.

## For adapter authors

1. **Rebuild against the v2 SDK** for your language and re-publish. Each SDK lives
   in its own repo with its own CHANGELOG:
   - TypeScript — [`@criteria/adapter-sdk`](https://github.com/brokenbots/criteria-typescript-adapter-sdk)
   - Python — [`criteria-python-adapter-sdk`](https://github.com/brokenbots/criteria-python-adapter-sdk)
   - Go — [`criteria-go-adapter-sdk`](https://github.com/brokenbots/criteria-go-adapter-sdk)

2. **Start from a starter template** if rewriting is easier than porting — it ships
   the publish workflow, manifest emission, and remote/Dockerfile examples. See
   [adapters.md → Authoring](adapters.md#authoring-an-adapter).

3. **Declare what you used to read implicitly.** Secrets move from `config` to a
   declared `secrets` list in the manifest, resolved by the host and delivered
   over the secret channel (`sdk.secrets.get(...)`), never `process.env`. If you
   shell out to a child that needs a secret in its environment, forward it
   explicitly with the SDK's `secrets.spawnEnv([...])` helper
   ([adapters.md → Secrets](adapters.md#secrets)).

4. **Publish a signed artifact.** Tag your repo; the starter's `publish.yml` (or
   `.gitlab-ci.yml.example`, or `make publish`) builds, **keyless-signs**, and
   pushes. To ship a runnable container image too, build + push it and record it
   with `criteria adapter publish … --image <ref>`.
