# eval-varscope-restore — Implement var-value overlay in RestoreVarScope

**Phase:** Pre-Phase-4 (independent) · **Track:** C (test buffer / correctness) · **Owner:** Workstream executor · **Depends on:** [test-02-hcl-parsing-eval-coverage.md](test-02-hcl-parsing-eval-coverage.md) (test skeleton and `t.Skip` anchors are in place). · **Unblocks:** The `for_each` var-override crash-resume gap documented as `[ARCH-REVIEW]` in `test-02`.

## Context

`RestoreVarScope` ([workflow/eval.go](../workflow/eval.go)) rebuilds the runtime variable scope after a crash or pause. Step outputs are correctly restored from the JSON scope snapshot. However, **primitive variable values in the `"var"` JSON section are silently ignored**: when the engine resumes, all `var.*` values are re-seeded from the `FSMGraph` defaults rather than from the serialized runtime values.

This means that any CLI var overrides (`criteria apply --var name=value`) and any var values that were mutated during execution are **silently lost on crash-resume**. The round-trip contract is broken for primitive vars.

The `test-02` workstream added focused tests that prove this gap:
- `TestRestoreVarScope_VarValues_RestoredFromJSON` — skipped, pending this workstream
- `TestRestoreVarScope_VarTypeMismatch_ReturnsError` — skipped, pending this workstream
- `TestVarScope_RoundTrip_PrimitiveTypes` — currently asserts FSMGraph-default behavior with a comment pointing here

The fix requires:
1. Implementing the var-overlay path in `RestoreVarScope` — for each primitive var (string, number, bool) in the JSON `"var"` section, overwrite the FSMGraph default with the serialized value.
2. Using **strict full-string numeric parsing** (`strconv.ParseFloat`, not `fmt.Sscanf`) so that prefix-valid garbage like `"1oops"` is rejected with an error rather than silently truncated to `1`.
3. Un-skipping the two deferred tests and updating `TestVarScope_RoundTrip_PrimitiveTypes` to assert runtime values survive.

Non-primitive vars (list, map, object) continue to fall back to FSMGraph defaults because `CtyValueToString` is lossy for these types. This is documented as a separate `[ARCH-REVIEW]` in `test-02` and is **not in scope here**.

## Prerequisites

- `make ci` green on the base branch.
- `test-02-hcl-parsing-eval-coverage.md` is merged; the `t.Skip` tests and the `restoreStepsFromJSON` refactoring are in place.
- Familiarity with `workflow/eval.go`: `SeedVarsFromGraph`, `RestoreVarScope`, `CtyValueToString`, and the `FSMGraph.Variables` map.

## In scope

### Step 1 — Implement `restoreVarFromString` helper

Add a private helper in `workflow/eval.go` (place it before `RestoreVarScope`):

```go
// restoreVarFromString converts a string scope value (as written by
// CtyValueToString) back to a cty.Value of the given type. Only primitive
// types (string, number, bool) are supported; callers skip non-primitive types
// to fall back to FSMGraph defaults.
//
// Numeric strings are parsed with strconv.ParseFloat (full-string, strict) so
// that prefix-valid garbage like "1oops" is rejected rather than silently
// truncated to 1.
func restoreVarFromString(s string, t cty.Type) (cty.Value, error) {
    switch t {
    case cty.String:
        return cty.StringVal(s), nil
    case cty.Number:
        f, err := strconv.ParseFloat(s, 64)
        if err != nil {
            return cty.NilVal, fmt.Errorf("parse number %q: %w", s, err)
        }
        return cty.NumberFloatVal(f), nil
    case cty.Bool:
        switch s {
        case "true", "1":
            return cty.True, nil
        case "false", "0":
            return cty.False, nil
        default:
            return cty.NilVal, fmt.Errorf("parse bool %q: expected 'true' or 'false'", s)
        }
    default:
        return cty.NilVal, fmt.Errorf("unsupported type %s for primitive var restoration", t.FriendlyName())
    }
}
```

### Step 2 — Implement `overlayVarsFromJSON` helper

Add a second helper immediately after `restoreVarFromString`:

```go
// overlayVarsFromJSON merges the "var" section of a JSON scope snapshot onto
// the pre-seeded vars map. Empty strings are skipped (null/empty ambiguity).
// Non-primitive types fall back to FSMGraph defaults because CtyValueToString
// is lossy for list/map/object.
func overlayVarsFromJSON(vars map[string]cty.Value, varData map[string]interface{}, g *FSMGraph) error {
    varObj := vars["var"]
    if varObj == cty.NilVal || !varObj.Type().IsObjectType() {
        return nil
    }
    updated := make(map[string]cty.Value, len(varObj.Type().AttributeTypes()))
    for k := range varObj.Type().AttributeTypes() {
        updated[k] = varObj.GetAttr(k)
    }
    for k, rawVal := range varData {
        sv, ok := rawVal.(string)
        if !ok || sv == "" {
            continue
        }
        node, ok := g.Variables[k]
        if !ok {
            continue
        }
        if node.Type != cty.String && node.Type != cty.Number && node.Type != cty.Bool {
            continue
        }
        v, err := restoreVarFromString(sv, node.Type)
        if err != nil {
            return fmt.Errorf("restore var %q: %w", k, err)
        }
        if _, exists := updated[k]; exists {
            updated[k] = v
        }
    }
    if len(updated) > 0 {
        vars["var"] = cty.ObjectVal(updated)
    }
    return nil
}
```

### Step 3 — Wire `overlayVarsFromJSON` into `RestoreVarScope`

In `RestoreVarScope`, after parsing the raw JSON and before restoring step outputs, add:

```go
// Overlay serialized primitive var values onto FSMGraph defaults.
if varData, ok := raw["var"].(map[string]interface{}); ok {
    if err := overlayVarsFromJSON(vars, varData, g); err != nil {
        return vars, nil, err
    }
}
```

Update the function's doc-comment to reflect that primitive var values are now restored from JSON.

### Step 4 — Un-skip and strengthen the deferred tests

In `workflow/eval_varscope_roundtrip_test.go`:

1. **`TestRestoreVarScope_VarValues_RestoredFromJSON`** — remove the `t.Skip`. The test should verify that a var serialized with a runtime value comes back as that runtime value (not the FSMGraph default). Use distinct runtime vs FSMGraph defaults to prove JSON wins over graph seeding.

2. **`TestRestoreVarScope_VarTypeMismatch_ReturnsError`** — remove the `t.Skip`. The test should verify that a JSON scope with a type-mismatched var value (e.g. `"not-a-number"` for a `cty.Number` var) returns a non-nil error naming the offending variable.

3. **Add `TestRestoreVarScope_NumericPrefixGarbage_ReturnsError`** — a regression test specifically for the prefix-valid garbage case: JSON `{"var":{"count":"1oops"}}` with a `cty.Number` var named `count` must return an error (not silently restore `count=1`).

4. **`TestVarScope_RoundTrip_PrimitiveTypes`** — update to assert that runtime values (not FSMGraph defaults) are restored. The FSMGraph defaults should be distinct from the runtime values; the test should verify the JSON values win.

5. **`TestVarScope_RoundTrip_LargeScope_HandlesLengthEfficiently`** — add back the spot-check that runtime values survive (not just that the count is correct).

## Out of scope

- Non-primitive var types (list, map, object) — these remain behind the `[ARCH-REVIEW]` in `test-02` and require a schema change or alternate serialization strategy.
- `IterCursor.Items`/`Keys` serialization — re-evaluated from workflow expression on re-entry by design.
- Any changes to the `SerializeVarScope` wire format — the format is stable; this workstream only changes the restore side.

## Allowed files

- `workflow/eval.go`
- `workflow/eval_varscope_roundtrip_test.go`
- `workflow/go.mod` / `workflow/go.sum` if a new dependency is needed (unlikely; `strconv` is stdlib)

## Exit criteria

- [ ] `TestRestoreVarScope_VarValues_RestoredFromJSON` passes (not skipped).
- [ ] `TestRestoreVarScope_VarTypeMismatch_ReturnsError` passes (not skipped).
- [ ] `TestRestoreVarScope_NumericPrefixGarbage_ReturnsError` passes: `"1oops"` is rejected with an error.
- [ ] `TestVarScope_RoundTrip_PrimitiveTypes` asserts runtime values (not FSMGraph defaults).
- [ ] `go test -race -count=2 ./workflow/...` passes.
- [ ] `make ci` passes.
- [ ] Net new production lines in `workflow/eval.go`: `restoreVarFromString` + `overlayVarsFromJSON` + overlay call ≤ 70 lines combined.
