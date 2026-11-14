# ADR-0013: decision adapter — the System One decision-model family behind one adapter

**Status:** Accepted (at merge per the 2026-11-14 arming ruling; filed as Proposed,
series UN-ARMED, Kanboard cards 2xx, project 1)
**Date:** 2026-10-04
**Decided by:** dave (rulings in session; see D1-D8)

## Context

TypeSafe AI ships Jev, the first "System One" decision model (https://docs.typesafe.ai): the caller POSTs
`{model, state, questions}` to `POST /v1/systemone` where `questions` is a map of question-id →
`{type: choice|score|noul, instructions, criteria}` (choice: option map; score: ordered level list; noul:
optional yes/no gloss), and gets back typed answers keyed by question id — choice + probabilities +
confidence, score + legend + probabilities + confidence, or a bare noul 0..1. Every question is evaluated
independently in parallel against the same state; the response's `model` field reports the versioned model
id that answered (aliases like `jev-latest` drift by design).

Ollama v0.35.1 serves the identical wire format at `http://localhost:11434/v1/systemone` for Clef (27B) and
Clef Flash (9B) open-weights decision models, including multimodal state (`images` base64 array); decision
models now declare a `decision` capability so general chat clients do not offer them.

The wire format is therefore becoming the generic interface for decision models; the model field and base
URL are the only variables that differ between TypeSafe cloud and a local Ollama. Criteria has no adapter
that can make fast, typed, low-cost judgments inside workflows: copilot-class adapters spend a full agent
turn on what a decision model answers in one call with calibrated probabilities.

dave's framing: introduce this as the pre-v0.6.0 teaser, called the **decision adapter** to keep it generic.

## Decisions

D1 — NAME: `decision`. Adapter type = `decision`; repo = `criteria-adapter-decision`, Go, on
criteria-go-adapter-sdk (tier-2 Service interface for v0.1; D7). The generic name is deliberate: the
backing model is a config field, not the adapter's identity.

D2 — BACKENDS: cloud and local are the same adapter. `base_url` (e.g. `https://api.typesafe.ai` or
`http://localhost:11434`) and `model` (e.g. `jev-latest`, `clef-flash`) are REQUIRED config — no implicit
defaults, no backend enum, examples always write both out (strict-config posture, consistent with the
v0.6.0 config hardline). `api_key` is an OPTIONAL secret: present → Authorization Bearer header; absent →
no header (local Ollama needs none; no localhost heuristics).

D3 — OUTCOME POSTURE (dave ruling 1): strict. Default `outcome_pure` — the adapter always reports outcome
`success`; ALL routing is composed in the workflow via switch/match conditions over
`steps.<step>.answers.<id>.{choice,score,noul,confidence}`. Opt-in `outcome_question = "<question-id>"` on
the adapter block: names ONE choice question whose selected option becomes the step's outcome — validated
STRICTLY in two phases: at OpenSession the question id must exist and be type=choice (config error
otherwise), and at Execute the selected choice must be a member of that step's allowed_outcomes set or the
step fails closed (same posture as copilot's submit_outcome). Everything that is not a direct Choice
(choice thresholds, score bands, multi-factor weighting, confidence gating) stays graph-side in match
conditions — no adapter-side routing DSL.

D4 — QUESTION PLACEMENT (dave ruling 2, delegate): v0.1 ships PER-STEP questions — the `questions` object
and `state` are step inputs, so every step composes its own question set against the adapter instance's
locked backend. Instance-static question profiles are NOT in v0.1; revisit only when a real workflow
demonstrates the need (reusable question profiles on the adapter block).

D5 — OUTPUTS AND COMPOSITION: outputs are a verbatim passthrough of the upstream response — `answers`
exactly as returned keyed by question id, plus `usage`. No sugar projections, no re-encoding. Retries are
NOT an adapter concern: no retry DSL inside the adapter (the workflow's while/for_each composes retries);
one allowed exception is a small TRANSPORT-level retry knob (default OFF, honors Retry-After, 429/5xx
only) — a single POST /v1/systemone is idempotent. HTTP 429/5xx/auth/timeout → outcome `failure` with a
typed error payload in outputs; the workflow routes or retries from there.

D6 — EVENTS: one adapter event per decision call — the versioned model id that answered (for alias-drift
tracking), per-question answers, usage, and latency. State never rides events; secrets are redacted. The
adapter is STATELESS: no session state beyond the captured config/secrets/allowed_outcomes; concurrent
Execute calls are safe.

D7 — SDK SEQUENCING (dave ruling 3): build v0.1 NOW on the tier-2 Go Service interface, structured thin
(isolated decision-client package + thin Service impl), and fold the ADR-0011 migration INTO the v0.6.0
wave: the decision adapter becomes the first real non-test consumer of the unified spec-object tier when
cards 176-181 land, and its migration findings feed back into the SDK wave.

D8 — OUT OF SCOPE v0.1 (dave ruling 4): multimodal/images (Jev is text-only anyway; follow-up riding a
clef-specific need), tools-mode (`ask` tool over the adapter-tools surface — composable later because the
client layer is shared), instance-static question profiles (D4), any server/discover machinery.

## Input contract (wire)

- `state`: string, or a JSON object/array lowered to text per the TypeSafe state contract.
- `questions`: object of question-id (bareword) → question object; `type` ∈ {choice, score, noul};
  `instructions` required non-empty; `criteria` required for choice (option map) and score (ordered level
  list), optional for noul. Strict validation, fail-closed, unknown keys inside a question object are
  rejected. HCL authors write the questions object directly in the step `input` block.

## Consequences

- One adapter, two backends; workflows remain backend-agnostic in shape (same grammar against jev-latest
  in the cloud and clef-flash on localhost).
- The flagship composition is confidence-gated human-in-the-loop: noul(urgency) + choice(department) route
  a ticket, and a match condition on `answers.<id>.confidence` falls through to an `approval` node — all
  existing criteria primitives, no engine change. ZERO wire changes: the v2 adapter wire already carries
  config schema, input/output schemas, secrets, and allowed_outcomes.
- Lock/pin today: published as a signed OCI artifact (ghcr + keyless), locked in .criteria.lock.hcl like
  every other adapter — this lockfile-able block is the teaser deliverable.
- Migration debt is pre-declared (D7) instead of discovered later.

## Series

Cards 199-208 (project 1): 199 this record (decision-adapter doctrine, filed UN-ARMED) · 200 repo
scaffold + decisionclient core · 201 strict questions/state validation · 202 Service impl — Execute,
verbatim outputs, failure mapping, retry knob · 203 outcome_question strict Choice→outcome · 204 Info
schemas/secrets/manifest/publish · 205 workflow-example decision_demo_v1 (depends on 204's info surface;
repo brokenbots/workflow-example) · 206 docs sweep · 207 validation run (DISARMED — arm only with dave
fire-authorization after 200-206 + the ghcr `decision:v0.1.0` publish) · 208 migrate to the unified
spec-object tier (ADR-0011 fold-in; blocked after cards 176-181 + 182, after 207).

Cards 200-207 are built and closed: the adapter repo `criteria-adapter-decision` is live, tagged v0.1.0.
Until further docs land here, the consumer-facing contract stays in the adapter repo — its
[README](https://github.com/brokenbots/criteria-adapter-decision) (full adapter contract) and its
[backend guide](https://github.com/brokenbots/criteria-adapter-decision/blob/main/docs/backends.md).
