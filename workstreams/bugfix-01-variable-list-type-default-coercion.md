# Bugfix Workstream BF-01 — Variable `list(string)` default rejects `["a", "b"]` literal

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** BF-02 (independent).

## Context

A variable declared with `type = "list(string)"` could not accept a `["a", "b"]` literal as
its `default` value, even though that is the expected and natural syntax. HCL evaluates `[...]`
expressions as `cty.Tuple`, not `cty.List`, because the two types share the same construction
syntax. The compile-time validator in `convertCtyValue`
([workflow/compile_variables.go:120](../workflow/compile_variables.go#L120)) previously used a
strict `Type().Equals(typ)` check with no fallback, so any attempt to write:

```hcl
variable "tags" {
  type    = "list(string)"
  default = ["foo", "bar"]
}
```

produced the compile error `default value is tuple(string, string) but variable is declared as
list(string)`, forcing users to the non-idiomatic workaround of `tolist(["foo", "bar"])` or
simply omitting the default entirely.

The runtime counterparts — `SharedVarStore.Set` and `SharedVarStore.SetBatch`
([internal/engine/shared_var_store.go:62](../internal/engine/shared_var_store.go#L62)) — already
handled this case correctly via `go-cty`'s `convert.Convert` package. The bug was only at
compile-time default validation.

## Prerequisites

- `make test` green on `main`.
- Familiarity with [workflow/compile_variables.go](../workflow/compile_variables.go) and
  [github.com/zclconf/go-cty/cty/convert](https://pkg.go.dev/github.com/zclconf/go-cty/cty/convert).

## In scope

### Step 1 — Fix `convertCtyValue` to use `convert.Convert` as fallback

Edit [workflow/compile_variables.go:120](../workflow/compile_variables.go#L120).

Replace the strict equality-only implementation with one that attempts `convert.Convert` when
types differ. Add `"github.com/zclconf/go-cty/cty/convert"` to the import block.

```go
func convertCtyValue(v cty.Value, typ cty.Type) (cty.Value, error) {
    if v.Type().Equals(typ) {
        return v, nil
    }
    converted, err := convert.Convert(v, typ)
    if err != nil {
        return cty.NilVal, fmt.Errorf("default value is %s but variable is declared as %s",
            v.Type().FriendlyName(), typ.FriendlyName())
    }
    return converted, nil
}
```

Semantics preserved: a `number` literal on a `string` variable is still rejected. Only
conversions that `go-cty` considers safe and lossless are accepted — in practice the only
newly-passing case is tuple-of-T → list(T).

### Step 2 — Tests

Add to [workflow/compile_variables_test.go](../workflow/compile_variables_test.go):

- `TestVariableCompile_ListDefaultTupleLiteral` — `type = "list(string)"` with
  `default = ["foo", "bar"]` must compile without error; the compiled `VariableNode.Default` must
  have type `list(string)` (not tuple) and element values `["foo", "bar"]`.
- Existing `TestVariableCompile_DefaultTypeMismatch` must continue to pass.
- Existing `TestVariableCompile_DefaultBoolMismatch` must continue to pass.

## Behavior change

**Yes — previously-rejected workflows now compile.**

- `variable` blocks with a `list(string)`, `list(number)`, or `list(bool)` type and a tuple
  literal default now compile successfully. The default is coerced to the declared list type.
- Incompatible types (e.g. `number` default on a `string` variable) continue to be errors.
- No change to runtime behavior. No change to the wire contract.

## Reuse

- `github.com/zclconf/go-cty/cty/convert` — already used by `SharedVarStore.Set/SetBatch` and
  `evalRunOutputs`. Do not hand-roll type coercion.

## Out of scope

- Coercion of tuple literals in any context other than `variable` block `default` values.
- Any change to `parseVariableType`, `TypeToString`, or the accepted type-string set.
- Any change to `isListStringValue` or input-block validation.
- Any change to the wire contract or event types.

## Files this workstream may modify

- `workflow/compile_variables.go` — add `convert` import; replace `convertCtyValue` body.
- `workflow/compile_variables_test.go` — add `TestVariableCompile_ListDefaultTupleLiteral`.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [x] Add `"github.com/zclconf/go-cty/cty/convert"` import to `workflow/compile_variables.go`.
- [x] Replace `convertCtyValue` body with `convert.Convert`-based fallback.
- [x] Add `TestVariableCompile_ListDefaultTupleLiteral` to `workflow/compile_variables_test.go`.
- [x] `go test ./workflow/ -run TestVariableCompile` passes.
- [x] `make test` clean.

## Exit criteria

- `variable "x" { type = "list(string)"; default = ["a", "b"] }` compiles without diagnostics.
- `VariableNode.Default.Type()` equals `cty.List(cty.String)`.
- `variable "x" { type = "string"; default = 42 }` continues to produce a compile error.
- `make test` clean.
