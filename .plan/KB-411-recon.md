# KB-411 recon notes (workstream plan — will be removed before final review)

## Defect
templatefile()-rendered step inputs re-evaluated at criteria_restart lose the
pre-restart var value context: `shellquote(jsonencode(var.repos))` (var.repos
list(string), CLI/launch override) fails post-restart with
`invalid value; expected string` at <shellquote> (run 19bed9ec).

## Confirmed mechanics (code-read + cty/text-template probes)
- text/template fires exactly `at <shellquote>: invalid value; expected string`
  when a PIPELINE operand is Go nil (`{{ .x | shellquote }}` with x == nil).
  Go nil in the template ctxMap comes from ctyToGoValue(NULL cty value).
- jsonencode (cty stdlib) ALWAYS returns a string for known input ("null" for
  null input), so the nil can only enter via a value that reached the template
  vars map WITHOUT jsonencode, or a DIFFERENT var whose type was string.
- RestoreVarScope (workflow/eval.go) restores step outputs (typed) + iter
  cursors but IGNORES the serialized "var" section — vars are re-seeded from
  graph defaults only. Multiple SKIPPED tests in
  workflow/eval_varscope_roundtrip_test.go document this known bug.
- SerializeVarScope writes vars as LOSSY strings (CtyValueToString — list
  vars become comma-joined text; nulls omitted). Step outputs use typed
  cty-JSON ("steps_typed" + "steps_typed_type") — the established lossless
  pattern.
- Server-connected restart (internal/cli/reattach.go
  buildResumedActiveEngine) uses RestoreVarScope → override values lost.
  Local resume (internal/cli/apply_local.go) re-applies CLI var overrides;
  serve-adapter restore (internal/cli/serveadapter_snapshot.go restoreRunScope)
  re-applies the LOSSY string map via ApplyVarOverrides/parseOverrideString —
  which for list vars parses "a,b" as HCL and fails.

## Fix design (spec "restore/honor the variable type context")
1. SerializeVarScope: additionally write "vars_typed" + "vars_typed_type"
   (cty-JSON of vars["var"] + its type), keeping the legacy "var" string map
   for older readers (server stores blob verbatim).
2. RestoreVarScope: prefer typed vars overlay when present; fall back to
   graph defaults (current behavior) for legacy blobs.
3. serveadapter restoreRunScope: prefer typed vars; legacy string overlay
   only for old blobs.
4. Un-skip/uplift the round-trip tests for the typed path.
5. Regression test (engine): templatefile step + restart at step boundary →
   re-evaluation byte-identical; shellquote(jsonencode(var.list)) intact.

## Validation
- go test ./workflow/... (roundtrip + templatefile suites)
- go test ./internal/engine/ -run focused
- go test ./internal/cli/ -run (snapshot/restore focused)
