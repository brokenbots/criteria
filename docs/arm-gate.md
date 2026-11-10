# Arm gate: compiling pinned trees against the runner-pod engine stamp

This page states the operative engine rule behind the arm contract and the
compile gate the shepherd runs before arming a card. The rule is landed in
the [workflow-example operator guide](https://github.com/brokenbots/workflow-example/blob/main/docs/jobs-setup.md)
as the third hard guard in `docs/jobs-setup.md`, "Hard guards enforced by the
tool" (arm-time compile gate) plus the everyday-loop gate step, carried on
that repo's `KB-368` branch; the arc shepherd contract statement sits in
`arc_shepherd_v1/scripts/confirm_action.sh.tftpl` (arm case) and
`arc_shepherd_v1/scripts/doctrine.md` in the same repo. This page carries
the working copy and the script.

## The engine rule

Run behavior is stamped by the runner pod's `criteria-base` **image** — a pod
running a `v0.5.56` base serves v0.5.56 engine semantics regardless of what
the workflow tree pins, which release tags exist, or which binary the operator
has on hand. The base-image roll gate ("no base roll while any run is
RUNNING") is orthogonal to compile exposure: a tree is compiled by a runner
pod only when its card fires, so a queued/armed card inherits **whichever
base the operator carries at its fire moment**, not the base that existed when
the card was registered. A card registered (and compile-checked) before a
base roll, then armed, fires its first leg on the rolled image — and if the
rolled engine tightened a compile rule, the tree fails compile at pod start,
deterministically.

## The arm-contract rule

**Before arming any card whose run will fire on a `criteria-base` image,
compile-gate the trees the route pins with a criteria binary built at or above
the base image's engine version.** If no release tag carries that engine yet
(a roll landing ahead of the matching release), a tag-exact local build of
the engine commit suffices — the gate consumes a binary and its reported
version, not a release page.

The gate ships in this repository as
[`tools/armgate/arm-gate.sh`](../tools/armgate/arm-gate.sh). It resolves the
engine seed from the image ref (the last `vX.Y.Z` token in the tag, e.g.
`criteria-base:v0.1-v0.5.56` → engine v0.5.56), picks a binary reporting at
least that version (explicit `--criteria-bin`, then PATH, then an opt-in
tag-exact build from `--criteria-repo`), and fails closed when the supplied
tree set is empty or every tree was blanked (zero trees gated), when the
seed is unparseable, no binary qualifies, or any tree fails
`criteria validate`.
Warnings are not failures — an unverified adapter schema is surfaced as a
warning by `criteria validate` and does not block arming; compile errors do.

## The queue-shepherd one-liner

Run per route, for every tree the route pins, before moving the trigger column:

```sh
tools/armgate/arm-gate.sh \
  --base-image "$CRITERIA_BASE_IMAGE" \
  --criteria-repo /srv/criteria \
  --tree /srv/routes/<route>/tree.main \
  --tree /srv/routes/<route>/tree.oncall
```

Exit 0 → safe to arm. Exit 1 → the gate failed closed: read the `criteria
validate` output above the summary line, fix the tree or roll back the image
expectation, and leave the card disarmed. A zero-tree outcome is also a
fail-closed exit 1 — an unset or empty route variable yields a refused
`--tree` (or no gated tree at all), never a pass. Exit 2 → usage error
(nothing was checked).

The `--criteria-repo` fallback is the release-lag path: it checks out the
repo at tag `v<engine>`, runs `make build`, and gates with that binary. It
needs the repo checkout to carry the tag — when it does not, the gate fails
closed with the tag name it wanted, and the operator either pins the image
expectation to an engine that actually ships or lands the engine change
first.

## Context

Arming bypass was observed first-hand during the kb-171 refire sequence: the
arming half of the disposition treated "the base roll is gated" as "the refire
still runs on the old engine", and the first fire after the roll compiled the
pinned tree under the new engine at pod start (crash loop from the new
outcome-completeness rule). The mechanical engine-side fixes are tracked
separately (see KB-374, KB-375); this gate is the shepherd-side contract that
keeps arming decisions consistent with the image that will actually run the
tree.
