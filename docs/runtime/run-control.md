# Run control: propagation from the parent run to child runs (ADR-0008 D2/D3)

Run-control verbs issued on the parent run's operator surface
(`criteria apply --server ...` → StopRun / PauseRun / ResumeRun) propagate to
every live adapter session, including a workflow.v1 peer whose session backs a
child run. This page is the semantics reference: what each parent verb does
child-side, and where the evidence lands on both event feeds.

## Parent verbs → child effects

| Parent verb | Session state | Child effect | Timeout / degenerate case |
|---|---|---|---|
| PauseRun | any session (tool/resources) | `Session.Pause` gates + drains nested tool calls, then the v2 `Pause` RPC | pause gate lifts if the adapter pause fails |
| PauseRun | workflow.v1 peer, child run mid-call | v2 `Pause` RPC → the child engine parks the run at its checkpoint boundary (child's own store) | barrier keeps failing until the child acks |
| PauseRun | workflow.v1 peer, run idle / settled | idempotent ack, **no RPC** — the run already persisted its own checkpoint | always acks |
| ResumeRun | any session (tool/resources) | v2 `Resume` RPC | joined per-session error on failure |
| ResumeRun | workflow.v1 peer, child run parked mid-call | v2 `Resume` RPC → the child resumes **from its own persisted checkpoint**, not parent state | runs to its terminal |
| ResumeRun | workflow.v1 peer, run idle / settled | idempotent ack, no RPC | always acks |
| StopRun / `criteria apply` interrupt | any live session | 1. cooperative `CancelChildRun` (empty run id = "the current one") · 2. bounded wait for terminal evidence · 3. session close | cancel rejected ⇒ nothing waited on |
| StopRun / interrupt | workflow.v1 peer, run still in flight after the wait | force `KillChild` (3s grace), then close | child journals the partial-teardown arm |

Two aggregate rules hold on the parent side:

- **The pause barrier lands only when ALL sessions acked.** `PauseAll` /
  `ResumeAll` join every per-session failure with its session name — a second
  failing session is visible, not dropped — and the engine's pause does not
  land while any error stands. The boundary pause (run-loop boundary, CRI-255)
  treats session checkpoint failures as best-effort (fail-open): the resume
  path rebuilds sessions from the step checkpoint written after the ack.
- **Teardown is bounded, never detached.** The settle wait runs detached from
  the caller's context (teardown paths race context cancellation by design)
  with a default budget of 15s (test-shrinkable), polling the tracker every
  100ms while waiting on a run the tracker has not observed yet.

## Evidence on both feeds

| Parent verb × child state | Child feed (child journal / run record / v2 events) | Parent feed (peer supervision journal → tracker) |
|---|---|---|
| stop × run settled by the cancel | run record `cancelled` + `ChildRunTerminal` (cancelled) | terminal arm settles the tracked run → recorded outcome `cancelled`, then the close |
| stop × run mid-call, cancel acked but unsettled | `ChildRunTeardownPartial` (+ `Exited` 143 when the kill ends the process) | force-killed mark settles the tracked record: outcome `force_killed`, `ForcedTeardown` set; a late real terminal outranks the mark |
| pause × run mid-call | run parked at checkpoint; `workflow.v1.run_paused` on the Execute feed | child run stays tracked in flight; no cancel is issued |
| pause × run idle | no child-run activity (idempotent ack) | nothing tracked; no RPC on the journal |
| resume × run parked mid-call | run resumes from its own checkpoint; `workflow.v1.run_resumed`, then its real terminal | terminal arm settles the tracked run through the ordinary path |
| resume × run idle / settled | no child-run activity (idempotent ack) | nothing tracked; no RPC on the journal |

The parent never issues a cancel or kill outside teardown, and pause/resume
never cancels a tracked child run (a parked run stays in flight and keeps its
record). The child-run wire vocabulary lives in
`proto/criteria/v1/peer.proto` (`ChildRunStarted` 11 / `ChildRunTerminal` 12 /
`ChildRunTeardownPartial` 13); the journaling contract is the child host's
`peer.ChildRunCanceler` surface: cancel → engine stop machinery → cancelled
record; kill accepted → partial-teardown arm → exit.