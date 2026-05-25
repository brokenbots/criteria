# WS01 — Mechanical schema cleanup

**Phase:** Language Cleanup · **Track:** Language · **Owner:** Workstream executor · **Depends on:** none · **Unblocks:** [WS02](WS02-data-and-outcome-semantics.md) (lands on the cleaned schema). · **Base branch:** `main`

## Context

The workflow HCL has accumulated several small divergences from Terraform's conventions that are visible the moment a user opens a workflow file in the VSCode extension:

- `workflow "name" { ... }` uses a label where Terraform would use a `name` attribute. Only one workflow block is allowed per module, so the label adds no information.
- `policy { ... }` is a stand-alone top-level block, but semantically it's the workflow's policy — Terraform would nest it inside the workflow block.
- `type = "string"` (string literal) does not match Terraform's `type = string` (type expression). Users coming from Terraform routinely write the unquoted form first.
- `default_outcome = "success"` is a magic-string attribute pointing at an outcome by name; more naturally expressed as an `outcome "default" {}` block that carries its own `next`/`output`/writes the same way every other outcome does.
- `environment = "shell.ci"` (quoted string) on workflow/adapter/subworkflow contradicts the bare-traversal form already used at the step level (`environment = shell.ci`).
- A short, hand-rolled function set (`file`, `fileexists`, `fileset`, `templatefile`, `trimfrontmatter`, `jsonencode`, `base64encode`, hash funcs) — but no `startswith`, `endswith`, `substr`, `lower`, `upper`, `replace`, `format`, `length`, etc. Users currently shell out to bash for trivial string work.

All of these are **mechanical** — they touch [workflow/schema.go](../../workflow/schema.go), [workflow/parser.go](../../workflow/parser.go), [workflow/parse_legacy_reject.go](../../workflow/parse_legacy_reject.go), a handful of compile_* files, and the .hcl example/internal-workflow files — but none of them touch the engine runtime or the runtime evaluation context. Bundling them avoids merge-conflict churn on `schema.go` (which they all edit) and lets WS02 land on a clean base.

Migration strategy: **hard break with helpful errors**. Each legacy form gets a one-line migration message via the existing pattern in [`parse_legacy_reject.go`](../../workflow/parse_legacy_reject.go).

## Prerequisites

- Working tree on `main`. The branch for this workstream cuts from `main`; PR targets `main`. After the language cleanup phase closes, `main` merges into `adapter-v2`.
- No external dependencies. `github.com/hashicorp/hcl/v2/ext/typeexpr` and `github.com/zclconf/go-cty/cty/function/stdlib` are both already transitively available through `hcl v2.24.0` / `go-cty`.

## In scope

### Step 1 — `workflow {}` header reshape

**Today:**
```hcl
workflow "demo" {
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}
policy { max_total_steps = 100 }
```

**Target:**
```hcl
workflow {
  name          = "demo"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
  policy { max_total_steps = 100 }
}
```

- [workflow/schema.go](../../workflow/schema.go) `WorkflowHeaderSpec`: drop `Name string hcl:"name,label"`; add `Name string hcl:"name"`. Add `Policy *PolicySpec hcl:"policy,block"`.
- [workflow/schema.go](../../workflow/schema.go) `Spec`: remove the top-level `Policy *PolicySpec hcl:"policy,block"`; compile reads from the header's nested block instead.
- [workflow/compile.go](../../workflow/compile.go): adjust the policy compile call site to read from `spec.Header.Policy`.
- Legacy rejection in [parse_legacy_reject.go](../../workflow/parse_legacy_reject.go):
  - Detect a labeled `workflow "x" {}` block and emit `workflow no longer takes a label; use workflow { name = "x" ... }`.
  - Detect a top-level `policy {}` block and emit `policy is now nested inside workflow { policy { ... } }`.

### Step 2 — Type expressions (`type = string`)

**Today:** `type = "string"`, `type = "list(string)"`, `type = "map(string)"`
**Target:** `type = string`, `type = list(string)`, `type = map(string)`, `type = object({ field = string })`, `type = any`

- [workflow/schema.go](../../workflow/schema.go) `VariableSpec.TypeStr string` → `Type hcl.Expression hcl:"type,optional"`. Same for `SharedVariableSpec` (kept as-is in this WS; it's renamed in WS02).
- [workflow/compile_variables.go](../../workflow/compile_variables.go): replace [`parseVariableType`](../../workflow/compile_variables.go#L64-L83) with a call to `typeexpr.Type(vs.Type)`. Delete the hand-rolled switch *and* delete `TypeToString` and any callers that round-tripped the string form (no cruft retained).
- The substitution unlocks `object({...})`, `tuple([...])`, `set(...)`, `any`, and `optional(string)` "for free."
- Legacy rejection: detect `type = "<string-literal>"` in `variable`/`shared_variable` bodies and emit `type is now a type expression: write type = string (no quotes), type = list(string), type = object({ field = string }), etc.`

### Step 3 — `default_outcome` → `outcome "default" {}`

**Today:** `default_outcome = "success"` (attribute on step).
**Target:** an `outcome "default" {}` block whose `next`/`output`/`write` fields apply when the adapter returns an unknown outcome name.

- [workflow/schema.go](../../workflow/schema.go) `StepSpec.DefaultOutcome` (line 178): remove.
- [workflow/compile_steps_graph.go](../../workflow/compile_steps_graph.go) `compileOutcomeBlock`: when an outcome literally named `default` is declared, attach the compiled outcome to `StepNode.DefaultOutcome` (change the field from `string` to `*CompiledOutcome` so the default carries its own `next`, projected `output`, and writes — matching every other outcome).
- The default outcome routes when the adapter returns a name not in the declared outcome set (existing behavior, just re-shaped).
- Legacy rejection: detect `default_outcome = "..."` on a step and emit `default_outcome has been replaced by an outcome "default" {} block; move the next target inside it.`

### Step 4 — Environment refs as traversals

**Today:** quoted-string form on workflow/adapter/subworkflow: `environment = "shell.ci"`. Step-level already uses the bare-traversal form.
**Target:** bare traversal everywhere: `environment = environment.shell.ci`.

- [workflow/schema.go](../../workflow/schema.go): change `WorkflowHeaderSpec.DefaultEnvironment` (line 93), `AdapterDeclSpec.Environment` (line 153), `SubworkflowSpec.Environment` (line 254) from `string` to `hcl.Expression`.
- Add a small helper (in [workflow/compile_environments.go](../../workflow/compile_environments.go) or a new file) that accepts a three-segment traversal `environment.<type>.<name>` and returns the canonical `"<type>.<name>"` string used downstream. Reuse the bare-traversal extraction logic already present for per-step `environment` overrides.
- Legacy rejection: detect quoted-string `environment = "..."` on these three blocks and emit `environment is now a reference: environment = environment.<type>.<name>`.

### Step 5 — Register `cty/function/stdlib`

- [workflow/eval_functions.go](../../workflow/eval_functions.go) `workflowFunctions` (line 102): import `github.com/zclconf/go-cty/cty/function/stdlib` and register all its functions into the returned map. Goes first so our handful of Criteria-specific functions (`file`, `fileexists`, `fileset`, `templatefile`, `trimfrontmatter`) can override if needed — but the policy is to **not** override; rely on community implementations wherever they exist.
- Drop hand-rolled duplicates: [`registerEncodingFunctions`](../../workflow/eval_functions_encoding.go) exports `jsonencode`/`jsondecode`/`base64encode`/`base64decode` — all of which live in stdlib. Remove them (and any tests asserting Criteria-specific behavior that doesn't match stdlib). Same review for [`registerHashFunctions`](../../workflow/eval_functions_hash.go) — these are *not* in stdlib (cty provides them via a separate optional `crypto` subpackage), so keep them only if no community equivalent exists.
- Adds (from stdlib): `substr`, `startswith`, `endswith`, `lower`, `upper`, `title`, `replace`, `format`, `formatlist`, `join`, `split`, `trim`, `trimspace`, `trimprefix`, `trimsuffix`, `length`, `regex`, `regexall`, `regexreplace`, `contains`, `keys`, `values`, `lookup`, `merge`, `concat`, `coalesce`, `coalescelist`, `compact`, `distinct`, `flatten`, `reverse`, `sort`, `range`, `slice`, `chunklist`, `abs`, `ceil`, `floor`, `max`, `min`, `pow`, `signum`, `parseint`, `chomp`, `indent`, `strrev`, etc.

### Step 6 — VSCode grammar updates

Coordinated single update to [criteria-vscode-extension-v1/syntaxes/criteria-hcl.tmLanguage.json](../../../criteria-vscode-extension-v1/syntaxes/criteria-hcl.tmLanguage.json):

- `workflow` block: drop the label match `^(workflow)\s+("[^"]*")\s*\{`; recognize `^(workflow)\s*\{` as the singleton form. Add `name` to the workflow body attribute list.
- `policy` block: remove the top-level matcher; recognize `policy` as a nested block inside `workflow`.
- `variable`/`shared_variable` body: drop the string-form `type = "..."` highlighting; highlight type expressions (`string`, `number`, `bool`, `list(...)`, `map(...)`, `object({...})`, `tuple([...])`, `any`) as type keywords.
- Step body: remove `default_outcome` from the attribute keyword list (it's no longer an attribute).
- `environment` attribute: highlight as a reference, not a string.
- Function-name highlighting: extend the function name pattern to include the new stdlib names so autocomplete-like coloring works.

### Step 7 — Migration rewrites

Rewrite all `.hcl` files (sed-friendly for most; quick visual sweep for the unusual ones):

- `examples/hello/hello.hcl`
- `examples/phase3-environment/phase3.hcl`
- `examples/phase3-multi-file/*.hcl`
- `examples/phase3-fold/fold-demo.hcl`
- `.criteria/workflows/develop/main.hcl`
- `.criteria/workflows/pr_review/main.hcl`
- `.criteria/workflows/bootstrap/main.hcl`
- `proposed_hcl.hcl` (design doc — update so it stays accurate)

Note: workflows that use `shared_variable` are left intact in this WS — WS02 will rewrite them.

### Step 8 — Tests

- [workflow/parse_legacy_reject_test.go](../../workflow/parse_legacy_reject_test.go) (or equivalent): add one case per new legacy-rejection rule; assert that the migration hint appears in the diagnostic.
- New positive tests for the new forms:
  - `workflow { name = ... }` with `policy { ... }` nested.
  - `type = string`, `type = list(string)`, `type = object({ a = string, b = number })`, `type = any`.
  - `outcome "default" { next = ... }` falls back when adapter returns an unknown outcome.
  - `environment = environment.shell.ci` traversal resolves to the expected `<type>.<name>`.
  - End-to-end smoke: a workflow exercising `startswith`, `substr`, `replace`, `format`, `join`, `length` in step `input { }` expressions and in switch `match` conditions.
- Update affected existing tests: anywhere a test fixture passes a label to `workflow`, a top-level `policy {}`, `type = "string"`, `default_outcome`, or `environment = "x.y"`, rewrite to the new form. There should be no remaining legacy forms in `workflow/*_test.go` fixtures after this WS.

## Out of scope

- `next` traversals in outcomes — WS02.
- `shared_variable` rename to `data` — WS02.
- `shared_writes` → `write` blocks — WS02.
- New language features (loop primitives, error handlers, etc.).
- Editor tooling beyond the TextMate grammar update.

## Reuse pointers

- [`github.com/hashicorp/hcl/v2/ext/typeexpr`](https://pkg.go.dev/github.com/hashicorp/hcl/v2/ext/typeexpr) — `typeexpr.Type()` and `typeexpr.TypeConstraint()` produce `cty.Type` from a HCL type expression. Already transitively available.
- [`github.com/zclconf/go-cty/cty/function/stdlib`](https://pkg.go.dev/github.com/zclconf/go-cty/cty/function/stdlib) — the full Terraform-equivalent function set. Already transitively available.
- Existing legacy-rejection pattern in [workflow/parse_legacy_reject.go](../../workflow/parse_legacy_reject.go) — clone this for each new rejection.
- Existing bare-traversal extraction for step-level `environment` overrides (in [workflow/compile_environments.go](../../workflow/compile_environments.go) or the step-iteration files) — generalize for workflow/adapter/subworkflow.

## Behavior change

**User-facing surface:** every workflow file changes shape (see migration rewrites in Step 7). Behavior of running a workflow is unchanged — same compile output, same engine semantics.

**Function set:** ~50 new functions become available. Existing function names (`file`, `fileexists`, `fileset`, `templatefile`, `trimfrontmatter`, hash family) keep working identically. Removed: `jsonencode`/`jsondecode`/`base64encode`/`base64decode` are now provided by stdlib instead of our `registerEncodingFunctions` — output should be byte-identical for normal inputs; verify in tests before deleting.

## Tests required

- All existing `workflow/*_test.go` pass after fixture migration.
- New tests in Step 8 pass.
- `go vet ./...` clean.
- Manual: open a migrated workflow in the VSCode extension; confirm correct highlighting for the new forms.
- Manual: `criteria run examples/phase3-environment/phase3.hcl` (after migration) executes successfully end-to-end.
