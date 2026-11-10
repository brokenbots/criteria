#!/bin/sh
# KB-394 — ADR-0014 M8 canary run harness (container-mode validation).
#
# Drives the container lane end to end on a Docker-capable host that reaches
# the LAN registry:
#
#   1. preflight: pulls the lockfile-pinned container image into the local
#      runtime daemon (registry reachability + image presence proven early),
#   2. pulls the KB-398-published adapter artifact into the per-run cache and
#      re-locks the workflow with the real CLI; the regenerated lockfile's
#      semantic pins (schema version, reference, version, digest, protocol,
#      platforms, container image) are compared against the committed pin and
#      any difference FAILS CLOSED — only formatting (comments/blank lines)
#      is ignored, so registry-side change stops the run,
#   3. runs `criteria apply` with an ND-JSON --events-file under fresh
#      HOME/XDG/TMPDIR isolation, so every run starts pristine,
#   4. mid-crash_demo kills via the container runtime the container hosting
#      the crash_demo executor's adapter child — located by its live run
#      cidfile plus a `sleep 12` process in-image. The engine's in-process
#      supervisor journal classifies the supervised exit (typed
#      CrashClassified); on_crash = "respawn" re-opens the session on a fresh
#      container child (recorded by cidfile-path novelty in the injector log)
#      and the run continues green through both re-entry
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
TMPDIR="$OUT/tmp"
mkdir -p "$OUT" "$TMPDIR"
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
# Re-lock with the real CLI: the committed lockfile is the reviewable source
# of record, the regenerated file is what apply consumes, and any semantic
# drift between them is registry-side change.
SCRATCH="$OUT/workflow"
rm -rf "$SCRATCH"
cp -R "$TREE" "$SCRATCH"
rm -f "$SCRATCH/.criteria.lock.hcl"
if ! "$CRITERIA" adapter lock "$SCRATCH" --allow-unsigned >"$OUT/lock.log" 2>&1; then
  echo "canary: adapter lock failed; see $OUT/lock.log" >&2
  exit 2
fi
# Semantic pin comparison: the canonical lockfile emitter (workflow/lockfile
# Write) emits no comments and no blank lines while the committed pin carries
# a header, so a raw text diff would always report drift and the gate could
# never pass. Extract each adapter's identity + pin fields — comments and
# blank lines ignored — and compare those; any semantic difference fails
# closed: the run does not proceed on a silently substituted pin.
# The adapter context clears only when the adapter block itself closes
# (brace-depth tracking, not the first `}` seen): the emitter writes each
# adapter's attributes, then signature, then container_image (ref, digest),
# then remote, so a nested block's close must not drop the adapter context —
# that once made signed lockfiles print their container_image pins as absent
# and the gate pass while blind to them.
lock_pin() {
  awk '
    function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
    {
      line = $0
      sub(/#.*/, "", line)
      sub(/^[ \t]+/, "", line)
      sub(/[ \t]+$/, "", line)
      if (line == "") next
      if (line ~ /^adapter[ \t]*"/) {
        split(line, f, "\"")
        at = f[2] "." f[4]
        depth = 1
        cimg = 0
        next
      }
      i = index(line, "=")
      if (at == "") {
        # top level: only the lockfile-wide schema_version matters
        if (i > 0 && trim(substr(line, 1, i - 1)) == "schema_version") {
          v = trim(substr(line, i + 1))
          gsub(/^"|"$/, "", v)
          schema = v
        }
        next
      }
      opens = gsub(/\{/, "&", line)
      closes = gsub(/\}/, "&", line)
      if (depth == 1 && opens == 1 && line == "container_image {") cimg = 1
      depth += opens - closes
      if (depth <= 0) {
        at = ""
        next
      }
      if (cimg && depth == 1) cimg = 0
      if (opens == 0 && closes == 0 && i > 0) {
        k = trim(substr(line, 1, i - 1))
        v = trim(substr(line, i + 1))
        gsub(/^"|"$/, "", v)
        pin[at "|" (cimg ? "container_image." k : k)] = v
      }
    }
    END {
      print "schema_version=" (schema == "" ? "absent" : schema)
      nf = split("reference version resolved_digest sdk_protocol_version platforms container_image.ref container_image.digest", fl, " ")
      for (a in pin) { split(a, p, "|"); ad[p[1]] = 1 }
      for (a in ad) {
        for (j = 1; j <= nf; j++)
          print "adapter=" a " " fl[j] "=" ((a "|" fl[j]) in pin ? pin[a "|" fl[j]] : "absent")
      }
    }' "$1" | sort
}
lock_pin "$TREE/.criteria.lock.hcl" >"$OUT/lock-pin-committed.txt"
lock_pin "$SCRATCH/.criteria.lock.hcl" >"$OUT/lock-pin-regenerated.txt"
if ! diff -u "$OUT/lock-pin-committed.txt" "$OUT/lock-pin-regenerated.txt" >"$OUT/lock-drift.diff" 2>&1; then
  echo "canary: LOCKFILE PIN DRIFT committed vs registry — failing closed (update the committed tree or accept the registry change deliberately)" >&2
  cat "$OUT/lock-drift.diff" >&2
  exit 2
fi
echo "lock gate: semantic pins match the committed pin (comments/blank lines ignored)"

# Preflight compile check: a red tree must fail before the crash injection
# spends a run. The scratch copy already passed the pin gate above; a plain
# validate compiles the root plus both child subworkflows (allowed paths are
# not required for the scratch copy) and only emits the permissive
# shell-adapter schema warning when the adapter binary is not installed.
if ! "$CRITERIA" validate "$SCRATCH" >"$OUT/validate.log" 2>&1; then
  echo "canary: preflight compile check failed; see $OUT/validate.log" >&2
  exit 2
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
REPLEN=0
# The replacement executor is matched by cidfile-path novelty, not mtime: the
# container runner creates a NEW criteria-cid-<uniq>.txt per start and removes
# the killed container's cidfile on exit, so the pre-kill path set is the
# discriminator and the in-image `sleep 12` probe confirms the role.
PREKILL_CIDFILES="$OUT/cidfile-prekill.txt"
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
          # pre-poll novelty base: written just before the kill; the reconcile
          # sweep only treats the file as ready once this exists
          ls "$TMPDIR"/criteria-cid-*.txt 2>/dev/null >"$PREKILL_CIDFILES.tmp" && mv -f "$PREKILL_CIDFILES.tmp" "$PREKILL_CIDFILES"
          echo "crash injector: $RUNTIME kill cid=$cid (crash_demo executor, sleep 12 in-image)" >>"$OUT/injector.log"
          "$RUNTIME" kill "$cid" >/dev/null 2>&1
        else
          echo "crash injector: no container with a live sleep 12 in-image found" >>"$OUT/injector.log"
        fi
      ) &
    fi
    if [ "$INJECTED" -eq 1 ] && [ "$REPLEN" -eq 0 ] && [ -f "$PREKILL_CIDFILES" ]; then
      for f in "$TMPDIR"/criteria-cid-*.txt; do
        [ -f "$f" ] || continue
        grep -F -q -x -- "$f" "$PREKILL_CIDFILES" && continue
        cid=$(cat "$f" 2>/dev/null)
        [ -n "$cid" ] || continue
        if "$RUNTIME" top "$cid" 2>/dev/null | grep -q 'sleep 12'; then
          echo "crash injector: replacement cidfile $f cid=$cid (post-respawn crash_demo executor)" >>"$OUT/injector.log"
          REPLEN=1
          break
        fi
      done
    fi
    if grep -E -q 'RunCompleted|RunFailed' "$EVENTS" 2>/dev/null; then
      break
    fi
  fi
  sleep 0.2
done
if [ "$INJECTED" -eq 1 ] && [ "$REPLEN" -eq 0 ]; then
  echo "crash injector: no replacement cidfile observed" >>"$OUT/injector.log"
fi

# ------------------------------------------------------------- drain + wrap
sleep 2
kill -0 "$APPLY_PID" 2>/dev/null && sleep 5
kill "$APPLY_PID" 2>/dev/null
wait "$APPLY_PID" 2>/dev/null
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
grep -E 'RunCompleted|RunFailed' "$EVENTS" 2>/dev/null | tail -1
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

# ------------------------------------------------------------ evidence gate
# An acceptance run is only green if the evidence exists: assert the markers
# the EVIDENCE.md pre-declared shape names and exit non-zero naming the
# missing ones, so a red evidence run can never masquerade as success by exit
# status. Crash markers (1a/1c/1d) are asserted on the executor lane only —
# CANARY_KILL_LANE none intentionally skips the injection.
[ -f "$EVENTS" ] || : >"$EVENTS"
echo
echo "=== evidence gate ==="
missing=""
gate() { # gate <pattern> <label> <files...>
  pat=$1
  label=$2
  shift 2
  if grep -E -q -- "$pat" "$@" 2>/dev/null; then
    echo "  ok: $label"
  else
    echo "  MISSING: $label"
    missing="$missing
  - $label"
  fi
}
if [ "$KILL_LANE" = "executor" ]; then
  gate '"msg":"adapter child exited unexpectedly"' \
    "1a: supervisor journal exit classification in operator logs" \
    "$OUT/apply-stderr.log" "$OUT/apply-stdout.log"
  gate '"kind":"session.respawned".*"crash_reason":"adapter process terminated"' \
    "1c: session.respawned event carrying the typed crash_reason" \
    "$EVENTS"
  gate 'crash injector: replacement cidfile .+ cid=.+ \(post-respawn crash_demo executor\)' \
    "1d: respawn opened the replacement crash_demo executor container (fresh cidfile recorded)" \
    "$OUT/injector.log"
else
  echo "  (kill lane none: crash markers 1a/1c not asserted)"
fi
gate '"payload_type":"RunCompleted".*"finalState":"done".*"success":true' \
  "RunCompleted finalState=done success=true (green run)" \
  "$EVENTS"
gate '"msg":"declared-fresh re-entry released an inherited session"' \
  "2a: fresh re-entry released the inherited session" \
  "$OUT/apply-stderr.log" "$OUT/apply-stdout.log"
gate '"msg":"re-declared adapter already provisioned by parent scope; reusing"' \
  "2b: reuse re-entry continued the inherited conversation" \
  "$OUT/apply-stderr.log" "$OUT/apply-stdout.log"
if grep -F -q '"payload_type":"RunFailed"' "$EVENTS" 2>/dev/null; then
  echo "  MISSING: no RunFailed events in the stream"
  missing="$missing
  - no RunFailed events in the stream"
fi
if [ -n "$missing" ]; then
  echo "canary: EVIDENCE GATE FAILED — missing markers:" >&2
  printf '%s\n' "$missing" >&2
  exit 1
fi
echo "evidence gate: all acceptance markers present"
