# ADR-0012: Interactive Criteria Shell

- Status: Accepted (design record; committed to the repo by card 185)
- Wave: v0.6.0 (must-have; sequencing per D8)

## Context

The v0.6.0 hardening wave (ADR-0008 criteria-as-adapter, ADR-0009 subworkflow overrides, ADR-0010 Kintsugi split, card-121 sealed sink union) leaves strong run-control primitives in place: CRI-255's loopback LocalControlService (PauseRun / ResumeRun / ResolveResume) lands pauses exactly at checkpoint points (top of each runLoop iteration — pause points are checkpoint points; the in-flight step completes, checkpoints are durable), Engine.RequestPause + the boundaryPause latch, PauseRun / ResumeRun / AgentPrompt arms on the orchestrator Control stream with the CRI-254 no-silent-drop dispatch, castle run-control writes under the CRI-196 console allowlist, and run-viewer graph rendering (parapet-lite derivation rules; `criteria compile --format json` emits the compiled graph including recursive subworkflows).

The missing surface is INTERACTIVE: humans and AI harnesses both need to construct and adjust a workflow in a live session — fetch adapters, add break points, edit nodes and edges, recompile, render, validate, then continue. This is also the criteria-as-adapter payoff (ADR-0008): agent harnesses and a workflow's own agent adapters can drive a live session's control surface if the shell speaks one surface both client classes consume.

## Decisions

D1 — Grammar. `criteria shell` attaches over LocalControlService (loopback; apply prints the endpoint). Verbs: step, continue, break <node|--all>, clear <node|--all>, list, render, edit, lock, pull, exit. `step` = temporary all-nodes breakpoint + resume — breakpoint machinery powers stepping; no second pause path.

D2 — Edit semantics: FREE graph surgery under name-keyed linkage validation (dave ruling 2026-10-04). A replaced graph compiles through the SAME pipeline (parse → compile → validate) and passes a linkage check against the live run: current/pending node stays addressable, already-visited nodes may not be removed (hard error unless the caller explicitly overrides), visit counts and variable scope carry BY NAME, policy caps re-checked post-carry, and new adapter references complete the lock/pull/verify pipeline BEFORE the swap. Alternatives rejected: append-only edits (dishonest UX) and checkpoint-restart per edit (loses live sessions — the avoidance shape).

D3 — Breakpoints are run-scoped control-plane state (dave ruling). Set/cleared over the wire, recorded in run state; they die with the run and are NEVER persisted into the workflow HCL.

D4 — Recompile-every-edit + render-to-validate before continue (dave ruling). Each edit recompiles; the shell renders the candidate graph; the run continues only after explicit confirm. Confirmation is the only swap authorization — no partial or implicit swaps.

D5 — Two client classes, ONE surface. The human REPL (pretty text) and the AI-harness NDJSON mode (request/response lines on stdio) are both thin clients over the same LocalControlService verbs; no separate AI implementation. Castle/Kintsugi parity: the same verbs proxied over the wire drive the console panel (cards 194/195). Graph edits serialize per run — one confirmed swap at a time; simultaneous edits are rejected while a swap is pending (deterministic; no last-writer-wins on the graph).

D6 — Wire is ADDITIVE (consumes no breaking window; no SDK major bump): a new Envelope payload GraphReplaced (the card-121 sealed union consumes the generated payload type), new ControlMessage arms SetBreakpoints / ClearBreakpoints / GraphEdit (CRI-254 dispatch pattern — every arm dispatched, backpressure holds), and new ServerService + LocalControlService RPCs sharing request/response messages per CRI-255. Castle's proto copy lands in criteria-control-plane-proto after card 166.

D7 — AI exposure is in-series (dave ruling): the verbs register as adapter tool-resources with permission targets in the existing reference-form grammar (adapter.<type>.<name>.tools.<tool>; ONE-OWNER — grant = wiring + allow, no second gate). Tool-path verbs are forward-looking: breakpoints and edit requests apply at the next checkpoint while the calling step keeps running, and they run the same confirmed pipeline — an agent cannot swap a graph without validation + confirmation (confirmation rides the tool-call input_required response round trip, never a bypass knob).

D8 — Sequencing: filed in-wave as the release's must-have; additive, so it does not consume the v0.6.0 breaking window. Engine seam cards follow the 129/169 SessionManager spine; the castle card consumes card 166 (criteria-control-plane-proto); the Kintsugi panel follows cards 164/165; the validation run stays DISARMED until explicit fire-authorization.

## Cards

185 ADR commit · 186 linkage validation · 187 checkpoint gate (breakpoint latch + validated swap) · 188 additive wire vocabulary · 189 transport dispatch + ND-JSON · 190 LocalControlService verbs · 191 human shell · 192 edit protocol · 193 harness NDJSON mode · 194 castle proxy + persistence (after 166) · 195 Kintsugi panel (after 164/165) · 196 shell-as-tools · 197 docs · 198 validation run (DISARMED)
