# parallel-02 — Adapter `parallel_safe` capability gate

**Owner:** Workstream executor · **Depends on:** parallel-01 (for `Loader` in `Deps`) · **Coordinates with:** parallel-01 (independent changes, no merge conflicts expected)

## Context

`parallel = [...]` on an adapter step fans out goroutines that all call
`deps.Sessions.Execute(ctx, n.step.AdapterRef, ...)` with **the same session
ID**. A session carries adapter state (e.g. conversation history, auth
context). Concurrent `Execute` calls on one session are only safe when the
adapter explicitly guarantees thread-safety. Without such a guarantee,
goroutines race on session-internal state.

The Copilot adapter (`cmd/criteria-adapter-copilot/`) demonstrates the problem:
its `Execute` method acquires `s.execMu.Lock()` at the very first line,
serializing all callers — 3 parallel iterations × 1-hour turn = 3 hours of
wall-clock time with no concurrency benefit.

The fix is a **hard gate**: adapters must declare a well-known capability
string `"parallel_safe"` in their `InfoResponse.capabilities` proto field.
Without the declaration:
- At **compile time** (when the adapter binary is resolvable): emit a
  `DiagError` so the author learns immediately.
- At **runtime** (fallback for adapters not resolvable at compile time):
  return a descriptive error before any goroutine is launched.

Built-in adapters that are already goroutine-safe (`noop`, `shell`) declare
the capability. The Copilot adapter does **not** — its serializing mutex is the
proof it is not safe.

The proto field `InfoResponse.capabilities` already exists in
`sdk/pb/criteria/v1/adapter_plugin.pb.go`. No proto changes are needed.

## Prerequisites

- parallel-01 is merged (provides `Deps.Loader`).
- `make test` passes on the merge of parallel-01.

## In scope

### Step 1 — Add `Capabilities []string` to `workflow.AdapterInfo`

**File:** `workflow/schema.go`

Extend the `AdapterInfo` struct:

```go
// AdapterInfo describes an adapter's declared configuration schema.
// It is used during workflow compilation to validate adapter config blocks and
// step input blocks against the adapter's declared requirements.
// An empty (zero-value) AdapterInfo means "any keys accepted" (permissive).
type AdapterInfo struct {
    ConfigSchema map[string]ConfigField // schema for adapter-level `config { }` blocks
    InputSchema  map[string]ConfigField // schema for per-step `input { }` blocks
    OutputSchema map[string]ConfigField // declared outputs the adapter promises to populate (W04)
    Capabilities []string               // ← add: well-known capability strings (e.g. "parallel_safe")
}
```

---

### Step 2 — Add `adapterHasCapability` helper to the workflow package

**File:** `workflow/compile_adapters.go`

Add right after the existing `adapterInfo` function (line ~131):

```go
// adapterHasCapability reports whether the AdapterInfo declares cap in its
// Capabilities slice. Used to gate parallel = [...] at compile time.
func adapterHasCapability(info AdapterInfo, cap string) bool {
    for _, c := range info.Capabilities {
        if c == cap {
            return true
        }
    }
    return false
}
```

---

### Step 3 — Compile-time gate in `compileIteratingStep`

**File:** `workflow/compile_steps_iteration.go`

Inside the `else` branch (the adapter target path, starting after
`adapterType := adapterTypeFromRef(adapterRef)` at line ~70), add the
capability check after `maybeCopilotAliasWarnings`:

```go
} else {
    inputMap, inputExprs, d := decodeStepInput(g, sp, schemas, opts, adapterType)
    diags = append(diags, d...)
    // each.* references are valid inside iterating steps; no error emitted.
    node = newAdapterStepNode(sp, spec, adapterRef, effectiveOnCrash, envKey, timeout, inputMap, inputExprs)
    diags = append(diags, maybeCopilotAliasWarnings(sp.Name, adapterType, node.AllowTools)...)
    // parallel_safe capability gate: when the step uses parallel = [...] the
    // adapter must declare "parallel_safe". When the adapter is absent from the
    // schemas map (binary not found during schema collection), we skip the check
    // here and rely on the runtime gate in evaluateParallel instead.
    if parallelExpr != nil {
        if info, ok := adapterInfo(schemas, adapterType); ok {
            if !adapterHasCapability(info, "parallel_safe") {
                diags = append(diags, &hcl.Diagnostic{
                    Severity: hcl.DiagError,
                    Summary: fmt.Sprintf(
                        "step %q: adapter type %q does not declare the \"parallel_safe\" capability; "+
                            "parallel execution requires the adapter to be safe for concurrent Execute calls. "+
                            "Use for_each for sequential iteration or declare parallel_safe in the adapter's Info().",
                        sp.Name, adapterType),
                })
            }
        }
    }
}
```

---

### Step 4 — Populate `Capabilities` in `AdapterInfoFromProto`

**File:** `internal/plugin/loader.go`

`AdapterInfoFromProto` currently does not copy capabilities into
`workflow.AdapterInfo`. Add it:

```go
func AdapterInfoFromProto(resp *pb.InfoResponse) workflow.AdapterInfo {
    return workflow.AdapterInfo{
        ConfigSchema: protoToConfigSchema(resp.GetConfigSchema()),
        InputSchema:  protoToConfigSchema(resp.GetInputSchema()),
        Capabilities: append([]string(nil), resp.GetCapabilities()...),  // ← add
    }
}
```

This ensures that `collectSchemas` (which stores `info.AdapterInfo`) carries
capabilities into the compile-time schemas map automatically.

---

### Step 5 — Propagate capabilities in `builtinAdapterPlugin.Info`

**File:** `internal/plugin/builtin.go`

`builtinAdapterPlugin.Info` currently hardcodes `Capabilities: nil`. Update it
to propagate the capabilities declared in the adapter's own `Info()` return:

```go
func (p *builtinAdapterPlugin) Info(context.Context) (Info, error) {
    if p.adapter == nil {
        return Info{}, fmt.Errorf("builtin adapter implementation is nil")
    }
    adInfo := p.adapter.Info()
    return Info{
        Name:         p.adapter.Name(),
        Version:      "builtin",
        Capabilities: append([]string(nil), adInfo.Capabilities...),  // ← change from nil
        AdapterInfo:  adInfo,
    }, nil
}
```

---

### Step 6 — Cache capabilities in `SessionManager.Session` and `Open`

**File:** `internal/plugin/sessions.go`

**6a.** Add `Capabilities []string` to the `Session` struct:

```go
type Session struct {
    Name      string
    Adapter   string
    Config    map[string]string
    OnCrash   string
    plugin    Plugin
    respawned bool
    closing   atomic.Bool
    Capabilities []string  // ← add: cached from plug.Info() at Open time
}
```

**6b.** In `SessionManager.Open`, call `plug.Info(ctx)` after `Resolve` and
before `OpenSession`, and cache the returned capabilities:

```go
plug, err := m.loader.Resolve(ctx, adapterName)
if err != nil {
    return err
}

// Cache capabilities so HasCapability can be called without a separate Info RPC.
// On error, capabilities default to nil — the runtime gate rejects parallel use.
var caps []string
if info, infoErr := plug.Info(ctx); infoErr == nil {
    caps = append([]string(nil), info.Capabilities...)
}

if err := plug.OpenSession(ctx, name, config); err != nil {
    plug.Kill()
    return err
}
```

And update the `Session` construction at the end of `Open`:

```go
m.sessions[name] = &Session{
    Name:         name,
    Adapter:      adapterName,
    Config:       cloneConfig(config),
    OnCrash:      normalizeOnCrash(onCrash),
    plugin:       plug,
    Capabilities: caps,   // ← add
}
```

**6c.** Add `HasCapability` to `SessionManager`:

```go
// HasCapability reports whether the session identified by name has cap in its
// cached capabilities slice. Returns false if the session is unknown or has no
// capabilities cached. Thread-safe.
func (m *SessionManager) HasCapability(name, cap string) bool {
    m.mu.Lock()
    defer m.mu.Unlock()
    sess, ok := m.sessions[name]
    if !ok {
        return false
    }
    for _, c := range sess.Capabilities {
        if c == cap {
            return true
        }
    }
    return false
}
```

Place this after the `Execute` method in `sessions.go`.

---

### Step 7 — Runtime gate in `evaluateParallel`

**File:** `internal/engine/parallel_iteration.go`

Add the runtime gate in `evaluateParallel` (line ~515) immediately after the
`if keys != nil` map-rejection guard and before `OnForEachEntered`:

```go
// Reject map/object at runtime as a safety net.
if keys != nil {
    return "", fmt.Errorf("step %q: parallel must be a list [...]; map and object syntax are not supported", n.step.Name)
}

// Runtime parallel_safe gate. This catches adapters that were not resolvable
// at compile time (schema absent) and defends against schema-skipping paths.
// Sessions are already open at this point (initScopeAdapters runs at scope
// entry), so capabilities are available via HasCapability.
if n.step.TargetKind == workflow.StepTargetAdapter {
    if !deps.Sessions.HasCapability(n.step.AdapterRef, "parallel_safe") {
        return "", fmt.Errorf(
            "step %q: adapter session %q does not declare the \"parallel_safe\" capability; "+
                "parallel execution is not permitted. "+
                "Declare parallel_safe in the adapter's Info() capabilities or use for_each for sequential iteration",
            n.step.Name, n.step.AdapterRef)
    }
}

total := len(items)
deps.Sink.OnForEachEntered(n.step.Name, total)
```

---

### Step 8 — Declare `parallel_safe` in the `noop` adapter

**File:** `cmd/criteria-adapter-noop/main.go`

The noop adapter's `Execute` acquires `s.mu.Lock()` only around session map
access, not around the actual execute logic. It is safe for concurrent calls.
Declare the capability:

```go
func (s *noopService) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
    return &pb.InfoResponse{
        Name:         "noop",
        Version:      "0.1.0",
        Capabilities: []string{"parallel_safe"},  // ← add
    }, nil
}
```

---

### Step 9 — Declare `parallel_safe` in the `shell` adapter

**File:** `internal/adapters/shell/shell.go`

The shell adapter's `Execute` spawns an independent subprocess per call — it
holds no per-session state between calls. It is safe for concurrent calls from
multiple goroutines. Declare the capability:

```go
func (a *Adapter) Info() workflow.AdapterInfo {
    return workflow.AdapterInfo{
        Capabilities: []string{"parallel_safe"},  // ← add
        InputSchema: map[string]workflow.ConfigField{
            // ... existing fields unchanged ...
        },
        OutputSchema: map[string]workflow.ConfigField{
            // ... existing fields unchanged ...
        },
    }
}
```

---

### Step 10 — Document `parallel_safe` in `docs/plugins.md`

Add a "Parallel execution" section (or extend the existing concurrency section)
explaining:

- When a workflow step uses `parallel = [...]` targeting an adapter step,
  the engine calls `Execute` concurrently from multiple goroutines.
- To opt in, return `Capabilities: []string{"parallel_safe"}` from `Info()`.
- Without the declaration, the engine rejects `parallel = [...]` for that
  adapter type at compile time (when schemas are available) or at runtime
  (when not).
- `parallel_safe` means: `Execute` may be called concurrently on **the same
  session** from multiple goroutines. The adapter must not hold shared mutable
  state that is unprotected within a single session.
- If your adapter needs per-request state that cannot be shared, open a new
  session per call (model it as separate `agent { }` blocks in HCL) or do
  not declare `parallel_safe`.

---

### Step 11 — Tests

**File:** `workflow/compile_steps_iteration_test.go`

Add tests:

```
TestStep_Parallel_AdapterNotParallelSafe_CompileError
```
- Schema has the adapter type but its `Capabilities` does not include
  `"parallel_safe"` → compile returns `DiagError` with "parallel_safe" in
  the message.

```
TestStep_Parallel_AdapterParallelSafe_NoError
```
- Schema has `Capabilities: []string{"parallel_safe"}` → no error.

```
TestStep_Parallel_AdapterAbsentFromSchemas_NoCompileError
```
- `schemas` is nil or does not contain the adapter type → no compile error
  (runtime gate fires instead).

**File:** `internal/engine/parallel_iteration_test.go` (or nearby engine test file)

```
TestEvaluateParallel_AdapterNotParallelSafe_RuntimeError
```
- Adapter session open with empty capabilities → `evaluateParallel` returns
  error containing "parallel_safe" before any iteration runs.

```
TestEvaluateParallel_AdapterParallelSafe_Runs
```
- Adapter session with `Capabilities: []string{"parallel_safe"}` → iterations
  run normally.

**File:** `internal/plugin/sessions_test.go`

```
TestSessionManager_HasCapability_AfterOpen
```
- Open a session using a test Plugin that returns a known `Capabilities` list
  from `Info()` → `HasCapability(name, "parallel_safe")` returns true;
  `HasCapability(name, "unknown")` returns false.

```
TestSessionManager_HasCapability_UnknownSession
```
- Call `HasCapability` for a session that was never opened → returns false.

---

## Behavior change

**Yes.** Any workflow step using `parallel = [...]` against an adapter that
does not declare `"parallel_safe"` will fail at compile time (when the adapter
binary is resolvable) or at runtime (when not). Previously such steps compiled
and ran but silently serialized behind the adapter's internal mutex.

The `noop` and `shell` adapters gain `parallel_safe` — their existing parallel
tests continue to pass and now genuinely execute concurrently.

The Copilot adapter is unchanged — it does **not** declare `parallel_safe`,
so `parallel = [...]` on a `copilot.*` step becomes a compile error.

## Reuse

- `adapterInfo(schemas, adapterType)` — existing helper in
  `workflow/compile_adapters.go`; the new `adapterHasCapability` follows the
  same pattern.
- `SessionManager.Open` already calls `plug.Resolve` + `plug.OpenSession`;
  the `plug.Info` call follows the same error-handling pattern.
- `rpcPlugin.Info` (line ~195 of `loader.go`) already copies capabilities
  into `plugin.Info.Capabilities`; `AdapterInfoFromProto` just needs to
  mirror that into `workflow.AdapterInfo.Capabilities`.

## Out of scope

- Subworkflow-step parallel session isolation — that is parallel-01.
- Sink fan-in throughput — that is parallel-03.
- Shared variable write semantics — that is parallel-04.
- Adding `parallel_safe` to the Copilot adapter — the adapter is not safe;
  do not add the capability.
- Proto changes — `InfoResponse.capabilities` already exists; no `.proto` edits.
- Changes to `OutputSchema` pass-through in `compileOutcomeBlock` (existing
  behavior, not related to this workstream).

## Files this workstream may modify

- `workflow/schema.go`
- `workflow/compile_adapters.go`
- `workflow/compile_steps_iteration.go`
- `workflow/compile_steps_iteration_test.go`
- `internal/plugin/loader.go`
- `internal/plugin/builtin.go`
- `internal/plugin/sessions.go`
- `internal/plugin/sessions_test.go` (or whichever file holds session tests)
- `internal/engine/parallel_iteration.go`
- `internal/engine/parallel_iteration_test.go` (or nearby engine test file)
- `cmd/criteria-adapter-noop/main.go`
- `internal/adapters/shell/shell.go`
- `docs/plugins.md`

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, `sdk/CHANGELOG.md`,
`cmd/criteria-adapter-copilot/`, or any other workstream file.

## Tasks

- [ ] Add `Capabilities []string` to `AdapterInfo` in `workflow/schema.go`
- [ ] Add `adapterHasCapability` helper to `workflow/compile_adapters.go`
- [ ] Add parallel_safe compile-time gate in `compileIteratingStep` (adapter branch)
- [ ] Update `AdapterInfoFromProto` to populate `Capabilities` from proto
- [ ] Update `builtinAdapterPlugin.Info` to propagate capabilities from `p.adapter.Info()`
- [ ] Add `Capabilities []string` field to `plugin.Session` struct
- [ ] Update `SessionManager.Open` to call `plug.Info` and cache capabilities
- [ ] Add `HasCapability(name, cap string) bool` to `SessionManager`
- [ ] Add runtime gate at top of `evaluateParallel` for adapter steps
- [ ] Add `Capabilities: []string{"parallel_safe"}` to `noop` adapter `Info()`
- [ ] Add `Capabilities: []string{"parallel_safe"}` to `shell` adapter `Info()`
- [ ] Update `docs/plugins.md` with parallel_safe documentation
- [ ] Write compile-time tests (`TestStep_Parallel_AdapterNotParallelSafe_CompileError`, etc.)
- [ ] Write runtime gate tests (`TestEvaluateParallel_AdapterNotParallelSafe_RuntimeError`, etc.)
- [ ] Write `TestSessionManager_HasCapability_*` tests
- [ ] Run `go test -race -count=5 ./...` and confirm pass
- [ ] Run `make test && make validate` and confirm green

## Exit criteria

- `go test -race -count=5 ./...` passes with no races.
- `TestStep_Parallel_AdapterNotParallelSafe_CompileError`: a step with
  `parallel = [...]` against an adapter missing `parallel_safe` in schemas
  returns a `DiagError` containing `"parallel_safe"`.
- `TestStep_Parallel_AdapterParallelSafe_NoError`: same step with
  `Capabilities: []string{"parallel_safe"}` in schemas returns no errors.
- `TestEvaluateParallel_AdapterNotParallelSafe_RuntimeError`: `evaluateParallel`
  returns an error containing `"parallel_safe"` before launching goroutines.
- Existing parallel step tests (W19 suite) pass.
- `make validate` passes (all example workflows compile).
- The Copilot adapter does not declare `parallel_safe` and no change was made
  to `cmd/criteria-adapter-copilot/`.
