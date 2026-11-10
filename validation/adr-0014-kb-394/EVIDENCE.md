# ADR-0014 M8 validation evidence (KB-394) — container hosting

Status: the container-mode canary tree is committed; the live run is armed at
ops level (KB-398 published the container-capable shell image and verified the
runner-side lane, 2026-11-10 — see run 718095c6-f403-40f8-841f-6da9c269e328).
The dev pod has no container runtime and no LAN registry reach, so the live
run below is executed from a docker-capable host with reach to
`localhost:5000`; its seq citations complete this file.

## Gate provenance (KB-398 close-out, PR #597)

- Adapter artifact `localhost:5000/criteria-adapter-shell:v0.5.6`
  (digest `sha256:a86fbdeb57e79623a70eeed9e36dc6c7b1e60258286b2bb48842cc226daec75a`).
- Container image `localhost:5000/criteria-adapter-shell:0.5.5-image`
  (digest `sha256:b6c44272fcc911db6225df77bdd22a85e0228914934d7a2f200fca5762087bc0`,
  built from criteria-adapter-shell v0.5.5, entrypoint
  `/usr/local/bin/criteria-adapter-shell`).
- Provenance note: the pinned artifact is v0.5.6 while the container image is
  the v0.5.5-era build, so the container arm's observed behavior proves the
  in-image v0.5.5 adapter build — not the v0.5.6 artifact. Cite the image
  build (0.5.5-image) when reporting the container-arm evidence; the artifact
  pin (`:v0.5.6`) governs the pull/handshake identity the engine records.
- Both pins are committed in `canary-container/.criteria.lock.hcl`. Before
  running, the harness re-locks with the real CLI and compares semantic pins
  (schema version, reference, version, digest, protocol, platforms, container
  image ref/digest) against the committed pin — comments and blank lines are
  ignored (the canonical emitter writes neither), and any semantic difference
  fails closed: the run does not start on a substituted pin.

## Live-run procedure

From a docker-capable host with the `criteria` CLI installed:

```
./validation/adr-0014-kb-394/run-canary.sh
```

Environment knobs: `CANARY_CRITERIA` (CLI override), `CANARY_OUT` (evidence
archive dir), `CANARY_KILL_LANE` (`executor` default, `none` skips injection),
`CANARY_ARTIFACT` / `CANARY_IMAGE` / `CANARY_RUNTIME` (defaults pinned in the
lockfile). The harness archives `events.ndjson`, `apply-stdout.log`,
`apply-stderr.log`, `injector.log`, plus the lock drift diff, under
`$CANARY_OUT`.

## Pre-declared evidence shape

One green run whose stream + operator logs carry, for the killed CONTAINER
child, the IN-PROCESS supervisor journal classification (not a peer process):

1. **Typed crash classification, container host.** Mid-`crash_demo` the
   harness kills the container hosting the crasher child (located by its live
   run cidfile plus the in-image `sleep 12` process; injector.log records the
   cid). Expected operator-log markers from the apply process:

   - **1a** the in-process supervisor's classification
     (`internal/peer/child.go:777-790`): `{"msg":"adapter child exited
     unexpectedly","adapter":"shell.crasher","reason":"adapter process
     terminated","exit_code":…,"signal":…}` and the journaled
     `CrashClassified` detail `adapter child exited while supervised
     (exit code …, signal …)`;
   - **1b** the engine consuming the journal's typed reason verbatim
     (`internal/adapterhost/sessions.go:2609`): `WARN adapter session crashed
     session=shell.crasher … crash_reason="adapter process terminated"`;
   - the declared `on_crash = "respawn"` policy cycling the session from the
     journal (a container session has no pod recreate arm, so the declared
     policy owns the outcome — the classification reason in the stream is
     consumed, never string-matched);
   - **1c** event stream: `AdapterEvent{kind:"session.respawned", …,
     crash_reason:"…"}` carried by seq (emitted at
     `internal/adapterhost/sessions.go:2652`), replacement container child
     spawned (**1d**: a fresh cidfile appears in the run TMPDIR, recorded in
     the injector log), `crash_demo` retried green.

2. **Declared fate at subworkflow re-entry, container host.**

   - **2a** Fresh pair (`shell.freshp`, `continuation = "fresh"` on both
     sides): every `child-fresh` re-entry logs `declared-fresh re-entry
     released an inherited session scope=sw-fresh-N
     adapter_instance=shell.freshp` (`internal/engine/lifecycle.go:1455`)
     and emits the paired stream event `OnAdapterLifecycle("closed")` for
     that instance, then a NEW conversation opens for the body scope (the
     inherited engagement closed; the fresh fall-through provisions that
     scope's own). Note: the
     root-scope-engagement release line seen in the peer-mode canary is the
     per-scope remote record path; container sessions are non-per-scope, so
     the stream shows the `released an inherited session` arm and the
     absence of the per-scope engagement record is correct under container
     hosting.
   - **2b** Reuse pair (`shell.reusep`, `continuation = "reuse"` on both
     sides): every `child-reuse` re-entry logs `re-declared adapter already
     provisioned by parent scope; reusing scope=sw-reuse-N
     adapter_instance=shell.reusep` (`internal/engine/lifecycle.go:1048`) —
     the inherited engagement keeps serving the body (one container child
     per definition; the same conversation continues, no second provision,
     no re-verify).
   - Stream windows: `reentry-fresh-1` → `sw-fresh-1` → `sw-fresh-2`, then
     `reentry-reuse-1` → `sw-reuse-1` → `sw-reuse-2`, then `done-marker` on
     the respawned crasher, terminal `RunCompleted{finalState:"done"}`.

3. **Acceptance mapping.** One stream carries both the typed journal-derived
   crash reason for a killed container-hosted child (marker 1, cited by seq)
   and both re-entry fates under container hosting (marker 2, cited by seq).

## Observation (live run)

The harness itself gates the evidence: it exits non-zero naming any missing
marker (1a, 1c, and 1d asserted only on the executor lane, RunCompleted
finalState=done success=true, 2a, 2b, and no RunFailed events), so a red
evidence run cannot pass by exit status alone. Pending the ops-side run of
the procedure above; cite run id and seq numbers for the markers 1a-1d and
2a-2b here when posting the card comment.
