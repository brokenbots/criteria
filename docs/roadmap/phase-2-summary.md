# Phase 2 — Maintainability + unattended MVP + Copilot tool-call finalization

> **This is a closed-phase record.** Active planning lives in
> `docs/roadmap/phase-3.md` (created by the Phase 3 cleanup gate).

**Status:** Closed 2026-05-02 at `v0.2.0`.
**Active workstream files:** [workstreams/archived/v2/](../../workstreams/archived/v2/)

## Goal

Phase 2 targeted three interlocking improvements on top of the Phase 1
stabilization base: (1) **maintainability lift** — burn down the mechanical
lint baseline debt, cap it in CI, and reduce bus-factor risk through a proper
contributor on-ramp; (2) **unattended MVP** — land local-mode approval and
signal waits, per-step `max_visits`, structured adapter lifecycle logging, and
state-directory permission hardening so that a pipeline can run end-to-end
without a server-side orchestrator; and (3) **Copilot tool-call finalization**
— replace brittle Copilot prose parsing with a typed `submit_outcome`
tool-call contract (`allowed_outcomes` on the wire, `SubmitOutcome` handler in
the adapter) and split the overgrown `copilot.go` file to make the adapter
maintainable. A Docker runtime image and release-candidate artifact upload
rounded out the phase.

## Workstreams

- **W01** — [Lint baseline mechanical burn-down](../../workstreams/archived/v2/01-lint-baseline-mechanical-burn-down.md): reduce W04/W06 mechanical entries; annotate proto-generated suppressions.
- **W02** — [Lint CI gate](../../workstreams/archived/v2/02-lint-ci-gate.md): enforce a hard cap in CI so the baseline cannot grow silently.
- **W03** — [copilot.go file split + permission-kind alias](../../workstreams/archived/v2/03-copilot-file-split-and-permission-alias.md): split oversized source file; add `permission_kind` alias (UF#02).
- **W04** — [State directory permissions hardening](../../workstreams/archived/v2/04-state-dir-permissions.md): create `~/.criteria/` and run subdirs at mode `0700`.
- **W05** — [SubWorkflowResolver wiring](../../workstreams/archived/v2/05-subworkflow-resolver-wiring.md): **cancelled 2026-04-30.** Deferred to Phase 3 language surface rework.
- **W06** — [Local-mode approval and signal wait](../../workstreams/archived/v2/06-local-mode-approval.md): stdin / file / env / auto-approve modes for approval nodes without an orchestrator.
- **W07** — [Per-step `max_visits`](../../workstreams/archived/v2/07-per-step-max-visits.md): compile-time and runtime enforcement of visit limits on back-edge loops.
- **W08** — [Contributor on-ramp](../../workstreams/archived/v2/08-contributor-on-ramp.md): first-PR guide, good-first-issue labels, bus-factor mitigation.
- **W09** — [Docker dev container and runtime image](../../workstreams/archived/v2/09-docker-dev-container-and-runtime-image.md): `Dockerfile.runtime` and `make docker-runtime-smoke` target.
- **W10** — [Remove `CRITERIA_SHELL_LEGACY=1` escape hatch](../../workstreams/archived/v2/10-remove-shell-legacy-escape-hatch.md): hard delete of the legacy shell-adapter bypass.
- **W11** — [Reviewer outcome aliasing](../../workstreams/archived/v2/11-reviewer-outcome-aliasing.md): **cancelled 2026-04-30.** Superseded by W14/W15 Copilot tool-call finalization (UF#03).
- **W12** — [Adapter lifecycle log clarity](../../workstreams/archived/v2/12-lifecycle-log-clarity.md): `[adapter: <name>]` tag in concise output (UF#06).
- **W13** — [Release-candidate artifact upload](../../workstreams/archived/v2/13-rc-artifact-upload.md): CI job to publish per-PR RC bundles.
- **W14** — [Copilot tool-call wire contract](../../workstreams/archived/v2/14-copilot-tool-call-wire-contract.md): `AllowedOutcomes` field in `pb.ExecuteRequest`; host populates on every Execute.
- **W15** — [Copilot `submit_outcome` adapter](../../workstreams/archived/v2/15-copilot-submit-outcome-adapter.md): `SubmitOutcome` tool-call handler in the Copilot adapter; full structured outcome finalization.
- **W16** — [Phase 2 cleanup gate](../../workstreams/archived/v2/16-phase2-cleanup-gate.md): archive, coordination-set updates, `v0.2.0` tag, phase close.

## Outcomes

- Lint baseline mechanical debt burned down; CI gate enforces the cap.
- Unattended local-mode approval/signal waits delivered (W06).
- Per-step `max_visits` compiled and enforced (W07).
- State directory and approval subdirectory hardened to mode `0700` (W04).
- `CRITERIA_SHELL_LEGACY=1` removed from all source (W10).
- Docker runtime image and smoke target operational (W09).
- Copilot `submit_outcome` structured tool-call contract shipped on the wire (W14) and in the adapter (W15).
- RC artifact upload job in CI (W13).
- Contributor on-ramp docs and first-PR guide in place (W08).
- Maintainability and Tech Debt both at **C+** at Phase 2 close
  (per [TECH_EVALUATION-20260501-01.md](../../tech_evaluations/TECH_EVALUATION-20260501-01.md));
  the ≥ B target was not reached in this phase and is carried into Phase 3.

## Source plan

The Phase 2 implementation plan was authored interactively and lives in the
architecture team's planning workspace. This file is the durable in-repo
summary; the original plan file is not preserved verbatim.
