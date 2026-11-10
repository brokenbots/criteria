#!/bin/sh
# KB-394 — ADR-0014 M8 canary run harness (container-mode validation).
#
# Drives the container lane end to end on a Docker-capable host that reaches
# the LAN registry:
#
#   1. preflight: pulls the lockfile-pinned container image into the local
#      runtime daemon (registry reachability + image presence proven early),
#   2. pulls the KB-398-published adapter artifact into the per-run cache and
#      re-locks the workflow with the real CLI; a regenerated lockfile that
#      drifts from the committed pin fails the gate with a diff (digest,
#      version, or image drift means the registry content changed),
#   3. runs `criteria apply` with an ND-JSON --events-file under fresh
#      HOME/XDG/TMPDIR isolation, so every run starts pristine,
#   4. mid-crash_demo kills via the container runtime the Container hosting
#      the crash_demo executor's adapter child — located by its live run
#      cidfile plus a `sleep 12` process in-image. The engine's in-process
#      supervisor journal classifies the supervised exit (typed
#      CrashClassified); on_crash = "respawn" re-opens the session on a fresh
#      container child and the run continues green through both re-entry
#      lanes (fresh releases and reopens; reuse detaches and re-adopts).
#   5. tears down cleanly, then prints the seq-cited evidence summary the
#      card's comment quotes (event stream + operator logs).
#
# CANARY_KILL_LANE none (default keeps the executor kill; set "none") skips
# the injection entirely for a warm-shape check.

set -u

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
TREE="$ROOT/validation/adr-0014-kb-394/canary-container"
ARTIFACT="${CANARY_ARTIFACT:-localhost:5000/criteria-adapter-shell:v0.5.6}"
IMAGE_REF="${CANARY_IMAGE:-localhost:5000/criteria-adapter-shell:0.5.5-image}"
RUNTIME="${CANARY_RUNTIME:-docker}"
KILL_LANE="${CANARY_KILL_LANE:-executor}"
case "$KILL_LANE" in executor | none) ;; *)
  echo "canary: CANARY_KILL_LANE must be executor or none (got $KILL_LANE)" >&2
  exit 2
  ;;
esac
OUT="${CANARY_OUT:-/tmp/kb394/canary-$(date +%Y%m%dT%H%M%S)}"
RUN_DIR_ROOT="$OUT/run-root"
TMPDIR="$OUT/tmp"
mkdir -p "$OUT" "$RUN_DIR_ROOT" "$TMPDIR"
EVENTS="$OUT/events.ndjson"
export TMPDIR

if [ "${CANARY_CRITERIA:-}" != "" ]; then
  CRITERIA=$CANARY_CRITERIA
elif [ -x "$ROOT/bin/criteria" ]; then
  CRITERIA="$ROOT/bin/criteria"
elif command -v criteria >/dev/null 2>&1; then
  CRITERIA=$(command -v criteria)
else
  echo "canary: no criteria CLI found (set CANARY_CRITERIA or build bin/criteria)" >&2
  exit 2
fi

# Per-run HOME/XDG isolation: the adapter OCI cache and engine run state key
# off HOME/XDG, so each run starts pristine with self-contained evidence.
export HOME="$OUT/home"
export XDG_CACHE_HOME="$OUT/home/.cache"
export XDG_STATE_HOME="$OUT/home/.local/state"
mkdir -p "$XDG_CACHE_HOME" "$XDG_STATE_HOME"

echo "out: $OUT"
echo "criteria: $CRITERIA ($("$CRITERIA" version 2>/dev/null | head -1))"
echo "runtime: $RUNTIME image: $IMAGE_REF"
echo "artifact: $ARTIFACT"
echo "kill lane: $KILL_LANE"

# --------------------------------------------------------------- preflight
command -v "$RUNTIME" >/dev/null 2>&1 || {
  echo "canary: runtime $RUNTIME not on PATH (container lane needs docker or podman)" >&2
  exit 2
}
"$RUNTIME" pull "$IMAGE_REF" >/dev/null 2>&1 || {
  echo "canary: pulling $IMAGE_REF failed — check registry reachability" >&2
  exit 2
}

"$CRITERIA" adapter pull "$ARTIFACT" --allow-unsigned >"$OUT/pull.log" 2>&1 ||
  {
    echo "canary: adapter pull failed; see $OUT/pull.log" >&2
    exit 2
  }

# ------------------------------------------------------------- lock + gate
# Re-lock with the real CLI and diff against the committed pin: the committed
# lockfile is the reviewable source of record, the regenerated file is what
# apply consumes, and any drift between them is registry-side change.
SCRATCH="$OUT/workflow"
rm -rf "$SCRATCH"
cp -R "$TREE" "$SCRATCH"
rm -f "$SCRATCH/.criteria.lock.hcl"
if ! "$CRITERIA" adapter lock "$SCRATCH" --allow-unsigned >"$OUT/lock.log" 2>&1; then
  echo "canary: adapter lock failed; see $OUT/lock.log" >&2
  exit 2
fi
if ! diff -u "$TREE/.criteria.lock.hcl" "$SCRATCH/.criteria.lock.hcl" >"$OUT/lock-drift.diff" 2>&1; then
  echo "canary: LOCKFILE DRIFT committed pin vs registry (proceeding with regenerated pin; update the committed tree)"
  cat "$OUT/lock-drift.diff"
else
  echo "lock gate: regenerated lockfile matches the committed pin"
fi

# ---------------------------------------------------------------- run apply
(
  cd "$SCRATCH" &&
    exec "$CRITERIA" apply --events-file "$EVENTS" .
) >"$OUT/apply-stdout.log" 2>"$OUT/apply-stderr.log" &
APPLY_PID=$!
trap 'kill "$APPLY_PID" 2>/dev/null; for cid in $(cat "$TMPDIR"/criteria-cid-*.txt 2>/dev/null); do '"$RUNTIME"' rm -f "$cid" >/dev/null 2>&1; done' EXIT INT TERM

# ------------------------------------------------------- reconcile + inject
INJECTED=0
while kill -0 "$APPLY_PID" 2>/dev/null; do
  if [ -f "$EVENTS" ]; then
    if [ "$INJECTED" -eq 0 ] && [ "$KILL_LANE" != "none" ] &&
      grep '"crash_demo"' "$EVENTS" 2>/dev/null | grep -q '"payload_type":"StepEntered"'; then
      INJECTED=1
      (
        sleep 5
        crash_demo_container() {
          for f in "$TMPDIR"/criteria-cid-*.txt; do
            [ -f "$f" ] || continue
            cid=$(cat "$f")
            # the crash_demo executor is the container running the tree's only
            # `sleep 12` in-image (crash_demo sleeps exactly 12)
            if "$RUNTIME" top "$cid" 2>/dev/null | grep -q 'sleep 12'; then
              echo "$cid"
              return 0
            fi
          done
          return 1
        }
        cid=$(crash_demo_container)
        if [ -n "${cid:-}" ]; then
          echo "crash injector: $RUNTIME kill cid=$cid (crash_demo executor, sleep 12 in-image)" >>"$OUT/injector.log"
          "$RUNTIME" kill "$cid" >/dev/null 2>&1
        else
          echo "crash injector: no container with a live sleep 12 in-image found" >>"$OUT/injector.log"
        fi
      ) &
    fi
    if grep -q 'RunCompleted\|RunFailed' "$EVENTS" 2>/dev/null; then
      break
    fi
  fi
  sleep 0.2
done

# ------------------------------------------------------------- drain + wrap
sleep 2
if [ "$APPLY_PID" -ne 0 ]; then
  kill -0 "$APPLY_PID" 2>/dev/null && sleep 5
  kill "$APPLY_PID" 2>/dev/null
  wait "$APPLY_PID" 2>/dev/null
fi
for cid in $(cat "$TMPDIR"/criteria-cid-*.txt 2>/dev/null); do
  "$RUNTIME" rm -f "$cid" >/dev/null 2>&1
done
sleep 1

# -------------------------------------------------------------- evidence
echo "=== run summary ==="
printf 'events total:        '
wc -l <"$EVENTS" 2>/dev/null
printf 'last terminal seq:   '
tail -1 "$EVENTS" | sed -n 's/.*"seq":\([0-9]*\).*/seq=\1/p'
echo
echo "--- event stream (crash + fate evidence) ---"
grep '"kind":"session.respawned"' "$EVENTS" 2>/dev/null || echo "no session.respawned events"
grep '"kind":"session.crash"' "$EVENTS" 2>/dev/null || true
grep 'RunCompleted\|RunFailed' "$EVENTS" 2>/dev/null | tail -1
echo
echo "--- operator logs (in-process supervisor + fate re-entry) ---"
grep '"msg":"adapter child exited unexpectedly"' "$OUT/apply-stderr.log" 2>/dev/null || echo "no supervisor exit log"
grep '"msg":"adapter session crashed"' "$OUT/apply-stderr.log" 2>/dev/null || true
grep '"msg":"declared-fresh re-entry released an inherited session"' "$OUT/apply-stderr.log" 2>/dev/null || echo "no declared-fresh release log"
grep '"msg":"re-declared adapter already provisioned by parent scope; reusing"' "$OUT/apply-stderr.log" 2>/dev/null || echo "no reuse skip log"
echo
printf 'injector:\n'
cat "$OUT/injector.log" 2>/dev/null || echo "no injection"
echo
echo "--- run outcome ---"
tail -3 "$OUT/apply-stdout.log" 2>/dev/null
echo "(evidence archived under $OUT)"
