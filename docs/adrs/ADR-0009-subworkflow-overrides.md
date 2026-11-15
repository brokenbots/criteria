# ADR-0009 — Subworkflow environment and adapter overrides (pass-in/pinning model)

**Status:** Proposed

**Date:** 2026-10-02

**Deciders:** Project lead; design session 2026-10-02 (engine trace + team review).

---

## Context

Subworkflows are self-contained modules: an `environment "<type>" "<name>"` block
compiles into the declaring workflow's graph only, and each module's adapters bind
against their own declarations (adapter ref, else that graph's `DefaultEnvironment`;
`resolveAdapterEnv`, `workflow/compile_adapters.go`). `mergeCalleeGraph` merges only
PinSet + FileCache — environment maps and policies deliberately never cross graphs,
which is what keeps module isolation (CRI-145/CRI-233) true.

Two forces push against pure module isolation:

1. **Composition.** A caller composing a callee (optionally a third-party, ref-pinned
   callee per ADR-0005) needs a way to hand the callee its own environment identity —
   the environment is the isolation boundary (one container per environment hosts all
   of that environment's adapters; v0.6.0 peer model, ADR-0007), so letting a callee
   run with an environment the caller does not control fragments pod topology, secret
   stacks, and policy enforcement across module seams.
2. **Session lifetime under iteration.** Provisioning today is scope-keyed
   (`initScopeAdapters`; `runWorkflowBody` defers `tearDownScopeAdapters` per body
   execution), so a callee inside a `for_each` loop cycles its adapter session/pod per
   iteration. Identity-based sharing (the parent's environment or the parent's adapter
   instance) is what makes session reuse a property of the topology instead of an
   artifact of skip timing (CRI-145).

Engine-trace findings that constrain the design:
- The subworkflow declaration's `environment` attribute parses and is stored on
  `SubworkflowNode.Environment` (compile test exists) but has **no runtime reader** —
  any pass-in semantics is a wire-up, not a new attribute.
- That reference is shape-checked (2-segment bare traversal) but **not validated to
  exist** in the parent graph, and is outside secret-taint validation today.
- Post-compile, an adapter whose reference was inherited from the graph default and
  one explicitly bound are **indistinguishable** on `AdapterNode.Environment` —
  substitution must happen during callee compilation, before binding resolution and
  policy caching (`cacheResolvedPolicy` keys `adapter:env`), not after.
- Adapter instance identity is `type.name`; environment identity is `<type>.<name>`;
  a callee may declare `shell.prod` and `container.prod` simultaneously, and
  `default` is a **legal environment name** (`environmentNamePattern`).

## Decision

### D1 — Grammar (v0.6.0, additive)

On the `subworkflow` block, three attributes alongside the existing `source`/`ref`:

```hcl
subworkflow "render" {
  source      = "./modules/render"        # + ref pin per ADR-0005, unchanged
  ref         = "sha256:..."

  # (a) default slot + same-key shadow pin. Bare 2-segment traversal.
  #     MUST exist in the declaring (parent) graph.
  environment = shell.ci

  # (b) environment overrides: callee env KEY -> parent env node.
  environment_overrides = {
    "container.render" = container.render_pool
  }

  # (c) adapter overrides: callee adapter slot "type.name" -> parent adapter instance.
  adapter_overrides = {
    "shell.renderer" = adapter.shell.pool_renderer
  }

  input { doc = "report.md" }
}
```

- Keys are **quoted `<type>.<name>` strings**; values are **bare traversals** —
  environment values `container.render_pool`, adapter values `adapter.shell.x`.
  Quoted-string values are rejected with the existing migration hint.
- Slot key in (c) is the callee adapter's **slot name** `type.name`; the value is the
  parent adapter **instance** `adapter.type.name` (instance identity may differ).
- No expressions in values for v0.6.0 (literals only — bare-traversal resolution has
  no conditional support and semantic stabilization refuses conditionals here).

### D2 — Resolution semantics

- Environments and adapters resolve **per declaring module** (callee envs are
  namespaced to the callee). Nothing about a callee's inner bindings is visible to the
  parent beyond these three attributes.
- **Substitution, not re-binding.** Pass-in replaces nodes in the callee's namespace:
  (a) and (b) replace the callee env node for the named key (and the callee's default
  slot when (a) is set and no same-key declaration exists, plus shadow any same-key
  declaration); (c) replaces the named adapter slot's effective instance. Then the
  callee compile proceeds **exactly as it would have**: bindings resolve against the
  substituted graph.
- **Precedence**: callee adapter's explicit env key binding > pass-in pin > callee's
  own default (header or auto-promoted single declaration). Explicit env bindings
  keep their KEY but run the substituted node's contents (policy, secrets provider,
  filesystem/network, working directory).
- **Consent is per hop, written in the hop's own vocabulary.** Overrides never flow
  through a module that did not write a line. A middle module forwards with its own
  `adapter_overrides` on ITS subworkflow block (`"shell.renderer" = adapter.shell.renderer`
  — "my child uses my slot, whatever backs it"); substitution composes mechanically
  and the middle module never learns what its parent pinned. No parent-adapter
  injection points in the callee, no conditional blends, no automatic deep
  propagation (non-lexical, unauditable).
- **Name-aligned sharing needs no line**: a callee re-declaring the parent adapter's
  original `type.name` shares the session via the existing CRI-145 re-declaration
  reuse, unchanged by this ADR. Overrides exist for name-mismatched reuse.

### D3 — Validation (compile-time, fail-closed)

- Key shape: quoted 2-segment `<type>.<name>`, segments matching the identifier
  patterns; otherwise a diagnostic naming the expected form.
- Slot existence: every key must be declared in the callee (env key in the callee's
  environments — except the default slot pin (a), which binds the default, not a
  declared key; adapter slot in the callee's adapters). Unknown keys error listing
  what the callee does declare.
- Value existence: values resolve in the **immediate parent graph only** (no
  grandparent reach). Env values must exist; adapter values must be declared parent
  adapter instances.
- **Type-label equality**: a key's type must equal its value's type (`shell` binds
  `shell` only). Outcome vocabularies and env-policy shape travel with the type label.
- Overlap: one slot, one binding — a key pinned by (a) may not reappear in (b).
- Conformance of the **effective** instance, post-substitution, inside the callee
  compile: manifest shape, `compatible_environments` type compatibility, input/output
  schema vs the callee's step expectations. **Resources availability and semantic
  misalignment are the caller author's problem** — this is an advanced feature and the
  residual behavioral risk is owned where it is created.

### D4 — Compile mechanics

- Substitution threads through `buildChildOpts` into the callee compile and lands
  **before** `resolveAdapterEnv` resolves bindings and before
  `cacheResolvedPolicy`/`ValidateEnvironmentTaint` run — so policy caching, secret
  stack resolution, and taint validation cover passed-in environment nodes with zero
  special cases.
- Pin provenance: `mergeCalleeGraph`'s pin merge records **parent-wins** on
  `type+name` conflicts, stated explicitly rather than whichever merged first.

### D5 — Runtime: borrow-not-owned session binding

- An override pins the callee slot to the parent's instance; the callee scope
  **borrows** the session: `initScopeAdapters`' session-open reuse (CRI-145) extends
  to alias-bound slots, and borrowed instances are excluded from the borrower's
  teardown list — **only the provisioning scope tears the session down**.
- Under iteration this makes the topology explicit: a `for_each` iteration's exit
  provably cannot kill a borrowed session; reuse-vs-fresh is decided by declared
  identity, and with the ADR-0007 peer model becomes an explicit host-to-peer
  control-plane signal rather than emergent skip timing.
- Remote per-scope machinery (token rotation, scope-instance reuse/adoption —
  CRI-137/CRI-304) keys by the slot identity; an alias registration records borrower
  scopes so release/teardown remains owned by the provisioning scope.
- Environment substitution into a `remote`-type parent env points the callee's session at the
  parent's host-of-record (guaranteed running — shim/peer startup walks the root
  graph), retiring the callee-only-remote-env sharp edge.

### D6 — Sibling decision

Step-level `environment` is removed in v0.6.0 (KB-73). After removal, the only
binding sites are: adapter declaration reference, subworkflow-block pins (this ADR),
and the per-graph default. No new binding surfaces beyond these.

## Consequences

- `SubworkflowNode.Environment` becomes live (D2/a) and gains existence validation in
  the parent graph; `environment_overrides` and `adapter_overrides` are new grammar
  (schema, parse, validate, substitute, engine binding, docs).
- No wire/SDK change. Conformance suite unaffected; HCL grammar and engine surface
  only.
- Breaking surface is additive for v0.5.x authors; removal of step-level environment
  ships its own migration (KB-73).
- Rejected alternatives: reserved `default` key inside the override maps (collides
  with a legal env name); name-only map keys (ambiguous with two-label identity when
  a callee declares `shell.x` and `container.x`); a nested `overrides {}` block
  (regroups freshly ratified top-level syntax for no gain); expression-valued slots
  (no resolution story; refuses to fake conditionals); callee-side "supplied by
  parent" injection points (layering violation); automatic deep propagation of
  overrides across intermediate modules (non-lexical consent).
- Validation waves: ADR + spec/docs first, then parser/validator, then substitution
  in compile, then engine borrow-binding, then docs/examples. Each lands green on
  main with tests (ticket series on the intake board).
