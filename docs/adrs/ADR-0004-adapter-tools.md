# ADR-0004 — Adapter-as-tool contract

**Status:** Accepted

**Date:** 2026-09-14

**Deciders:** Project lead (this repo); caller-side semantics ruling 2026-09-14

**Workstream:** M1.1 Adapter Tools (CRI-149). This ADR is the M1 contract of
the Adapter Tools project and blocks every other ticket in the project;
nothing lands before it merges.

---

## Context

Criteria adapters today are invoked in exactly one way: the engine routes a
step to a declared adapter instance, the adapter executes, and its outcome is
routed through the FSM. When one adapter needs work done by another adapter,
the callee has to be modelled as its own step — it enters the FSM, is
outcome-routed, and its result reaches the caller only through
step-expression/return plumbing. There is no way for an adapter to hand a
sub-task to another adapter *within* its own execution and get the result
back inline.

Protocol v2 already has the substrate such a mechanism needs:

- Adapters may emit a `permission.request` event on the Execute stream and
  block until the host decides (`awaitPermission`,
  `cmd/criteria-adapter-mcp/bridge.go`). The host intercepts the event,
  evaluates it against the step's `allow_tools` policy — glob matching with
  `path/filepath.Match` semantics
  ([internal/adapterhost/policy.go](../../internal/adapterhost/policy.go)) —
  writes an audit entry, and returns the decision to the adapter as a
  `PermissionEvent` on the per-session Permissions bidi stream.
- `PermissionEvent` flows **host-to-adapter** on that stream
  ([internal/adapterhost/permission_state.go](../../internal/adapterhost/permission_state.go),
  `sendEvent`): `request` for allow, `cancel` for deny. The reverse-direction
  `PermissionDecision` ACKs (adapter-to-host) are drained and discarded by
  the host
  ([internal/adapterhost/serve.go](../../internal/adapterhost/serve.go)).
- Adapters report capability strings in `InfoResponse.capabilities`
  (collected by the host loader).

What is missing is a contract that lets a declared adapter instance *present
tools* that other adapters can call, and defines how such a call travels on
the wire without the callee ever entering the FSM. The Adapter Tools project
(M1 onward) spans grammar, compiler, host runtime, SDK, and adapter work;
every ticket in it needs one citable contract. This ADR is that contract.

## Decision

The decisions are numbered for citation (`ADR-0004 §n`). All are binding;
later tickets implement them and do not re-open them.

### 1. Core concept — a tool call is not a step

Any declared adapter instance can present tools consumable by other adapters.
A tool call is **not a step**:

- the callee never enters the FSM;
- the callee is never outcome-routed;
- the call always returns to the calling adapter.

Adapter-to-adapter tool calls therefore bypass step-expression/return
plumbing entirely. The callee's result is delivered back into the caller's
in-flight Execute as a tool result, not as a step outcome in the workflow
graph.

### 2. Tool target naming

A tool target is one string:

```text
adapter.<type>.<name>.tools[.<tool>]
```

`<type>` and `<name>` identify a declared adapter instance, exactly as a step
`target` does today. This one string is, simultaneously:

1. the **HCL reference form** — what appears in a `tools = [...]` list (§4);
2. the **permission-surface target string** — what `allow_tools` patterns
   are matched against;
3. the **graph call-edge identity** — what the compiled call edge is keyed
   by.

- Bare `…tools` refers to **all tools** presented by that instance.
- `…tools.<tool>` refers to **one tool** by name.

`allow_tools` glob semantics apply to this string **unchanged**: matching
uses `path/filepath.Match` as the existing `allow_tools` evaluation does
today, so dots are literal characters (not wildcards), there is no `**`
recursive syntax, and `*` does not cross `/`. Because `*` matches any run of
non-`/` characters, it *may* span dots: the pattern `adapter.*.tools` covers
every instance's bare tool surface, and `adapter.shell.worker.tools.git_*`
covers every tool of that instance whose name starts with `git_`. Matching
remains anchored full-string matching with first-match-wins ordering, as
today.

| Reference / pattern | Meaning |
|---|---|
| `adapter.shell.worker.tools` | the instance's whole tool surface — grants all tools |
| `adapter.shell.worker.tools.git_status` | exactly the `git_status` tool |
| glob `adapter.*.tools` | every instance's bare tool surface |
| glob `adapter.shell.worker.tools.git_*` | every `git_*` tool of that instance |

Note that a bare `…tools` *pattern* matches only bare-form strings (matching
is anchored and exact), which is why bare-surface grants in `tools = [...]`
are taken as the whole surface rather than as glob patterns; a glob that
should cover every tool of an instance is spelled `…tools.*`.

### 3. Callee-side grammar

On an adapter **declaration**:

```hcl
adapter "shell" "worker" {
  # ... adapter config ...

  tool "git_status" {
    # static tool declaration
  }

  dynamic_tools = true
}
```

- `tool "<name>" { }` blocks are the **static declaration** form: the
  adapter declares the tools it presents, with stable names, at
  configuration time.
- `dynamic_tools = true` is the **runtime discovery** form: the adapter
  presents a tool surface discovered at run time, gated by `allow_tools` at
  call time (§7). This is the M7 extensibility hook — the grammar is not
  re-opened for dynamic tools later.
- An adapter may declare both: static blocks name the stable surface, and
  `dynamic_tools = true` additionally admits runtime-discovered tools.

### 4. Caller-side grammar

A caller grants tool calls with a step-level `tools` attribute — a list of
bare traversals in the target-naming form of §2:

```hcl
step "lint" {
  target = adapter.copilot.worker
  tools  = [adapter.shell.worker.tools.git_status]   # one tool
  # tools = [adapter.shell.worker.tools]             # all tools of the instance

  input { ... }

  outcome "success" { next = step.done }
  outcome "failure" { next = state.failed }
}
```

The same shape is available at adapter **configuration** level, applying to
every step that targets the instance; step-level lists union onto it.

Semantics ruling (project lead, 2026-09-14): step-level `tools` lists are
**determined by the adapter declarations**.

- A `tools` entry is meaningful only when the adapters involved actually
  support tools: the callee must present a tool surface (§3), and the caller
  must be able to issue calls.
- Every entry **grants the call**: entries are unioned into the step's
  effective allow set for the permission surface of §2.
- The compiler **flags** pointless `tools` entries using adapter-declared
  info — for example, naming a tool the callee does not statically declare,
  or granting tools on a step whose caller cannot issue calls. It **does not
  derive runtime behavior** from the list: runtime gating happens on the
  actual permission surface, not on the list alone.

### 5. Return semantics

The callee's `ExecuteResult` — its outcome plus typed outputs — flows back to
the caller **as the tool result**. The caller's own step outcome routing is
unaffected: the caller completes its step exactly as it would have without
the call, and its outcome is routed by the engine as normal.

The callee **never** routes through outcomes. Its `ExecuteResult` outcome is
data carried on the tool result, not an FSM transition; no `outcome` block of
any workflow node fires for a tool call.

### 6. Cycle policy

Cycles — A calls B calls A, directly or transitively — are **allowed**:

- **Compile time:** the compiler emits a **warning** when the call graph
  contains a cycle through tool edges. It is not an error.
- **Runtime:** enforcement is by depth. `policy.max_tool_depth` bounds the
  tool-call stack; default **8**, user-settable in the workflow `policy`
  block. Exceeding the depth, or runtime cycle detection, is a **typed
  failure returned to the calling adapter** as the tool result.

The run continues: a failed tool call is data for the caller, not a run
failure. An audit entry is written for the enforcement event, as for other
permission decisions.

### 7. Validation posture

- **Static tools** (§3 `tool` blocks): tool names in `tools = [...]` are
  checked at **compile time** against the callee's declared static tool
  blocks; unknown names are compile errors (per §4, the compiler works from
  adapter-declared info).
- **Dynamic tools** (`dynamic_tools = true`): the compile-time check is
  **lenient** — static names are not available — so enforcement happens at
  runtime, gated by `allow_tools` as for any permission surface.
- **Neither:** an adapter that declares no `tool` blocks and no
  `dynamic_tools = true` presents no tool surface; references to its tools
  are **rejected at compile time**.

### 8. Wire decision — Option A (message directions corrected against the live tree)

A tool call **is** a gated permission request with a payload.

**The call** travels as a `permission.request` AdapterEvent on the Execute
stream — the same mechanism the MCP bridge's `awaitPermission` uses today
(`cmd/criteria-adapter-mcp/bridge.go`). The payload carries:

| Field | Meaning |
|---|---|
| `request_id` | correlation id, minted by the calling adapter |
| `target` | the §2 target string of the callee tool surface |
| `tool` | the tool name |
| `args` | typed call arguments |
| `args_digest` | digest of the arguments, for audit and correlation |

**The result** returns on the Permissions bidi stream as a new
`PermissionEvent.tool_call_result` oneof member.

Message directions, corrected against the live tree — this is the part
earlier sketches got backwards:

- `PermissionEvent` flows **host-to-adapter** on the Permissions bidi
  stream (`internal/adapterhost/permission_state.go`, `sendEvent`) — which
  is why the tool result lands there.
- `PermissionDecision` is the adapter-to-host direction and is **drained and
  discarded** by the host (`internal/adapterhost/serve.go`); it is an ACK
  channel, not a result channel. The tool result must **not** be carried on
  `PermissionDecision`.

Message flow:

```text
caller adapter                host                          callee adapter
     |                         |                                |
     |  1. permission.request  |                                |
     | ---- Execute stream --> |                                |
     |    { request_id, target, tool, args, args_digest }       |
     |                         |                                |
     |                         |  2. allow_tools policy check   |
     |                         |     + audit entry              |
     |                         |                                |
     |                         |  3. nested Execute             |
     |                         | -----------------------------> |
     |                         |                                |
     |                         |  4. ExecuteResult              |
     |                         | <----------------------------- |
     |                         |    (outcome + typed outputs)   |
     |                         |                                |
     |  5. PermissionEvent     |                                |
     |     .tool_call_result   |                                |
     | <--- Permissions stream |                                |
     |    { request_id, result }                                |
```

Denied calls reuse `PermissionEvent.cancel`: the deny path is the existing
permission-deny path, unchanged.

Rationale:

- **One path for policy/audit/correlation.** Tool calls ride the existing
  permission machinery — the same `allow_tools` evaluation, the same
  decision-log entry shape (`request_id`, tool, `args_digest`, decision),
  the same audit writer. No parallel policy system to keep in sync.
- **Zero new streams.** Both directions already exist per session; the only
  wire addition is one `PermissionEvent` oneof member.
- **The capability string handles old-host determinism** (§9): a new adapter
  that initiates a call against an old host degrades deterministically
  instead of hanging or failing opaquely.

### 9. Versioning posture

The feature is advertised with an `adapter_tools` capability string in
`InfoResponse.capabilities`. Compatibility matrix:

| Host \ Adapter | no `adapter_tools` | declares `adapter_tools` |
|---|---|---|
| **old host** | unchanged — today's behavior | degrades deterministically: a granted call comes back as a bare allow-grant with no result signature, which the calling adapter surfaces as a typed `host_unsupported` failure; a denied call returns `cancel` as usual |
| **new host** | unaffected — unknown `PermissionEvent` oneof members are ignored, and old adapters never initiate tool calls | full feature |

The host does **not** error at session open for capability-less adapters.
The feature is **per-call**: the capability is consulted when a call is
attempted, not at session setup.

### 10. Self-call rule

A tool call whose callee is the calling adapter's own instance (`callee ==
caller`) is rejected with a typed `call_error` returned to the caller as the
tool result. Same-session reentry is out of scope for v1.

### 11. Pause/resume mid-call

The posture for pausing and resuming a run while a tool call is in flight is
decided in **M6.3 (CRI-169)**. This ADR deliberately does not fix it.
CRI-169 appends its resolution to this section when it lands; until then,
tickets must not assume either behavior for mid-call pause/resume.

## Consequences

### What this commits later tickets to build

- **Grammar / compiler.** Parse `tool` blocks and `dynamic_tools` on adapter
  declarations, `tools` lists at step and adapter config level (§3, §4);
  validate targets per §7; emit the cycle warning of §6; flag pointless
  `tools` entries per §4.
- **Host runtime.** Intercept tool-shaped `permission.request` events,
  evaluate `allow_tools` against the §2 string, run the nested Execute on
  the callee session, enforce `policy.max_tool_depth`, and dispatch
  `tool_call_result` on the caller's Permissions stream (§8). Deny reuses
  `cancel` (§8).
- **Wire / SDK.** One `PermissionEvent` oneof member (`tool_call_result`) in
  the external [`criteria-adapter-proto`](https://github.com/brokenbots/criteria-adapter-proto)
  package, plus the `adapter_tools` capability string (§8, §9).
- **M6.3.** Appends the pause/resume-mid-call resolution to §11.

### What stays unchanged

- The FSM and step machinery: tool calls create no nodes, need no new node
  kinds, and add no outcome plumbing (§1, §5).
- The existing permission flow for adapter-internal tools (e.g. the MCP
  bridge): the same `permission.request` → policy → `PermissionEvent` path,
  now carrying tool-call payloads.
- Step-expression/return plumbing: untouched by tool calls; only the
  existing step mechanism uses it.
- Adapters that never declare tools: zero behavioral change, old wire shape,
  no capability string needed.

### Risks accepted

- A caller that waits on `tool_call_result` against an old host blocks until
  it resolves the missing result signature into the typed `host_unsupported`
  failure (§9) — bounded degradation, no hang.
- Cycle depth is a runtime, not compile-time, guard (§6); pathological
  graphs are stopped by depth, not by static analysis, and each enforcement
  is auditable.

## Alternatives considered

1. **Carry the tool result on `PermissionDecision`.** Rejected: that message
   flows adapter-to-host and is drained and discarded by the host
   (`internal/adapterhost/serve.go`); it is an ACK channel. Using it would
   require the host to start interpreting a channel it deliberately
   discards, and the host-to-adapter result direction would still be
   missing.
2. **Dedicated tool-call RPC pair (a new bidi stream).** Rejected: it adds a
   session-lifetime stream and a parallel policy/audit path to obtain
   exactly what the permission path already provides (§8 rationale: one
   path, zero new streams).
3. **Model adapter-to-adapter calls as real steps.** Rejected: it puts the
   callee in the FSM, outcome-routes it, and couples the caller's control
   flow to callee outcomes through step-expression/return plumbing —
   precisely what §1 rules out.

## Related

- [ADR-0002](ADR-0002-while-step-iteration.md) — `while` step iteration
  modifier; tool-call targets reuse the existing `adapter.<type>.<name>`
  reference shape.
- [ADR-0003](ADR-0003-conformance-scope.md) — conformance scope (host +
  imported SDK): tool-call wire behavior is exercised by host conformance
  once the implementing tickets land.
- CRI-169 (M6.3) — pause/resume-mid-call posture; appends its resolution to
  §11 of this ADR.
- `awaitPermission` in
  [cmd/criteria-adapter-mcp/bridge.go](../../cmd/criteria-adapter-mcp/bridge.go)
  — the existing `permission.request` mechanism §8 reuses.
- [`criteria-adapter-proto`](https://github.com/brokenbots/criteria-adapter-proto)
  — external home of the v2 wire protocol (`PermissionEvent` oneof,
  `InfoResponse.capabilities`).

## Sign-off

| Role | Reviewer | Status | Date |
|---|---|---|---|
| Project lead (this repo) | Dave Sanderson (semantics ruling) | Accepted | 2026-09-14 |