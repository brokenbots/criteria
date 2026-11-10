#!/bin/sh
# Arm-contract compile gate: before arming a card whose run will fire on a
# criteria-base image, compile the route's pinned trees against the image's
# ENGINE stamp.
#
# The engine semantics a queued card runs under are stamped at FIRE time by
# the runner pod's criteria-base image — not by tree pins, release tags, or
# the operator's host binary. The base-roll gate (zero RUNNING runs) says
# nothing about tree-compile exposure, because a tree is compiled for a card
# only when the card fires; a card armed before a base roll inherits the
# rolled image on its first fire. Arming must therefore prove the trees
# compile under the base image's engine version, with a criteria binary
# built at or above that version. A tag-exact local build suffices when the
# published release lags the image.
#
# This gate fails closed: it refuses to pass when the image carries no
# parseable engine version, when no binary at/above the image's engine
# version is available, or when any tree fails `criteria validate`.
set -eu

PROG=arm-gate

err() { printf '%s\n' "$PROG: $*" >&2; }

usage() {
  cat >&2 <<'USAGE'
usage: arm-gate.sh --base-image <image-ref> --tree <dir-or-file> [--tree ...]
                   [--criteria-bin <path>] [--criteria-repo <dir>]

  --base-image    runner-pod criteria-base image ref; the engine version is
                  the last vX.Y.Z token in the tag, e.g.
                  criteria-base:v0.1-v0.5.56 -> engine v0.5.56
  --tree          workflow tree to gate (.hcl/.chcl file or directory);
                  repeatable, at least one required
  --criteria-bin  criteria binary to gate with; skips PATH discovery and the
                  build fallback; must itself report an engine version >= the
                  base image's, otherwise the gate fails closed
  --criteria-repo criteria repo checkout used to build a tag-exact binary
                  (git worktree at v<engine> + `make build`) when no usable
                  binary is found on PATH

exit codes: 0 gate passed; 1 gate failed (fails closed); 2 usage error
USAGE
}

# engine_from_image prints the engine semver carried by a criteria-base image
# ref, or nothing when the tag carries no vX.Y.Z token. Digest and registry
# components are ignored; only the tag's tokens are considered, and the LAST
# matching token wins so composite tags like v0.1-v0.5.56 resolve to v0.5.56.
engine_from_image() {
  printf '%s\n' "$1" |
    awk '{
      tag = $0; sub(/@.*/, "", tag);
      n = split(tag, a, ":"); tag = a[n];
      n = split(tag, t, "-"); best = "";
      for (i = 1; i <= n; i++) {
        v = t[i]; sub(/^[vV]/, "", v);
        if (v ~ /^[0-9]+\.[0-9]+\.[0-9]+$/) best = v;
      }
      if (best != "") print best;
    }'
}

# bin_engine_version prints the engine semver a criteria binary reports from
# `criteria version`, or nothing when the output carries no clean X.Y.Z
# token. Tokens are scanned for build-metadata suffixes (+build... is an
# ahead-of-stable stamping, stripped so the stable base compares) and for
# prerelease suffixes, which never clean-parse and count as unknown.
bin_engine_version() {
  "$1" version 2>/dev/null | head -n1 |
    awk '{
      n = split($0, t, " ");
      for (i = 1; i <= n; i++) {
        v = t[i]; sub(/\+.*/, "", v); sub(/^[vV]/, "", v);
        if (v ~ /^[0-9]+\.[0-9]+\.[0-9]+$/) { print v; exit }
      }
    }'
}

# semver_ge: exit 0 when $1 >= $2, field-wise numeric compare
semver_ge() {
  ah=${1%%.*}; arest=${1#*.}; am=${arest%%.*}; arest=${arest#*.}; ap=$arest
  bh=${2%%.*}; brest=${2#*.}; bm=${brest%%.*}; brest=${brest#*.}; bp=$brest
  if [ "$ah" -gt "$bh" ]; then return 0; fi
  if [ "$ah" -lt "$bh" ]; then return 1; fi
  if [ "$am" -gt "$bm" ]; then return 0; fi
  if [ "$am" -lt "$bm" ]; then return 1; fi
  [ "$ap" -ge "$bp" ]
}

CRITERIA_REPO=
CRITERIA_BIN=
BASE_IMAGE=
TREES=

while [ $# -gt 0 ]; do
  case $1 in
    --base-image)
      [ $# -ge 2 ] || { err "missing value for $1"; usage; exit 2; }
      BASE_IMAGE=$2; shift 2 ;;
    --tree)
      [ $# -ge 2 ] || { err "missing value for $1"; usage; exit 2; }
      TREES="$TREES$2
"; shift 2 ;;
    --criteria-bin)
      [ $# -ge 2 ] || { err "missing value for $1"; usage; exit 2; }
      CRITERIA_BIN=$2; shift 2 ;;
    --criteria-repo)
      [ $# -ge 2 ] || { err "missing value for $1"; usage; exit 2; }
      CRITERIA_REPO=$2; shift 2 ;;
    -h|--help) usage; exit 2 ;;
    *) err "unknown argument: $1"; usage; exit 2 ;;
  esac
done

if [ -z "$BASE_IMAGE" ]; then
  err "--base-image is required"
  usage
  exit 2
fi
if [ -z "$TREES" ]; then
  err "at least one --tree is required"
  usage
  exit 2
fi

ENGINE=$(engine_from_image "$BASE_IMAGE")
if [ -z "$ENGINE" ]; then
  err "cannot determine an engine version (vX.Y.Z) from base image \"$BASE_IMAGE\""
  err "refusing to arm: the engine version the trees must compile under is unknown"
  exit 1
fi
printf '%s: base image %s -> engine v%s\n' "$PROG" "$BASE_IMAGE" "$ENGINE"

# gate_reported_version prints v<ver> and returns 0 when the binary reports
# an engine at/above the base image's version; otherwise prints a rejection
# reason and returns 1.
gate_reported_version() {
  reported=$(bin_engine_version "$1")
  if [ -z "$reported" ]; then
    printf 'criteria binary %s reports no parseable engine version (X.Y.Z); refusing to trust it against base engine v%s' "$1" "$ENGINE"
    return 1
  fi
  if ! semver_ge "$reported" "$ENGINE"; then
    printf 'criteria binary %s reports v%s, older than base engine v%s; a first fire under that image would compile the trees with unvalidated semantics' "$1" "$reported" "$ENGINE"
    return 1
  fi
  printf 'v%s' "$reported"
}

BIN=
BIN_REPORTED=

# Candidate 1: an explicitly requested binary. If it is unusable there is
# nothing better than what was asked for, so fail closed instead of silently
# substituting another candidate.
if [ -n "$CRITERIA_BIN" ]; then
  if [ ! -x "$CRITERIA_BIN" ]; then
    err "--criteria-bin \"$CRITERIA_BIN\" is not an executable binary"
    exit 1
  fi
  reason=$(gate_reported_version "$CRITERIA_BIN") && {
    BIN=$CRITERIA_BIN
    BIN_REPORTED=$reason
  } || {
    err "explicit --criteria-bin rejected: $reason"
    exit 1
  }
else
  # Candidate 2: a criteria binary on PATH.
  if path_bin=$(command -v criteria 2>/dev/null); then
    reason=$(gate_reported_version "$path_bin") && {
      BIN=$path_bin
      BIN_REPORTED=$reason
    } || {
      printf '%s: PATH candidate %s rejected: %s\n' "$PROG" "$path_bin" "$reason" >&2
    }
  fi

  # Candidate 3: a tag-exact local build from a provided repo checkout.
  # `git worktree add --detach <tmp> v<engine>` + `make build` produces a
  # binary carrying the base image's engine version exactly; the worktree is
  # cleaned up by the EXIT trap.
  if [ -z "$BIN" ] && [ -n "$CRITERIA_REPO" ]; then
    if ! git -C "$CRITERIA_REPO" rev-parse --git-dir >/dev/null 2>&1; then
      err "--criteria-repo \"$CRITERIA_REPO\" is not a git checkout"
      exit 1
    fi
    worktree=$(mktemp -d "${TMPDIR:-/tmp}/arm-gate-build.XXXXXX")
    cleanup_worktree() {
      git -C "$CRITERIA_REPO" worktree remove --force "$worktree" >/dev/null 2>&1 || rm -rf "$worktree"
    }
    if ! git -C "$CRITERIA_REPO" worktree add --detach "$worktree" "refs/tags/v$ENGINE" >/dev/null 2>&1; then
      rm -rf "$worktree"
      err "cannot build a tag-exact binary: tag v$ENGINE not found in $CRITERIA_REPO"
      err "refusing to arm: no criteria binary at engine v$ENGINE or newer is available"
      err "remedies: pass --criteria-bin with a binary built at v$ENGINE+, or --criteria-repo with a checkout carrying tag v$ENGINE"
      exit 1
    fi
    trap cleanup_worktree EXIT
    printf '%s: building tag-exact criteria v%s in %s\n' "$PROG" "$ENGINE" "$worktree" >&2
    if ! make -C "$worktree" build; then
      err "tag-exact build of criteria v$ENGINE failed"
      err "refusing to arm: no verified engine for base v$ENGINE"
      exit 1
    fi
    reason=$(gate_reported_version "$worktree/bin/criteria") && {
      BIN=$worktree/bin/criteria
      BIN_REPORTED=$reason
    } || {
      err "tag-exact build rejected: $reason"
      exit 1
    }
  fi

  if [ -z "$BIN" ]; then
    err "cannot gate engine v$ENGINE: no usable criteria binary was found"
    err "remedies: pass --criteria-bin with a binary built at v$ENGINE+, or --criteria-repo with a checkout carrying tag v$ENGINE"
    exit 1
  fi
fi

printf '%s: gating with %s (reports %s)\n' "$PROG" "$BIN" "$BIN_REPORTED"

failures=0
total=0
while IFS= read -r tree; do
  [ -n "$tree" ] || continue
  total=$((total + 1))
  if "$BIN" validate "$tree"; then
    printf '%s: OK   %s\n' "$PROG" "$tree"
  else
    printf '%s: FAIL %s (see validate output above)\n' "$PROG" "$tree" >&2
    failures=$((failures + 1))
  fi
done <<EOF
$TREES
EOF

if [ "$failures" -gt 0 ]; then
  printf '%s: GATE FAILED: %s of %s tree(s) do not compile under engine v%s — do NOT arm\n' \
    "$PROG" "$failures" "$total" "$ENGINE" >&2
  exit 1
fi
printf '%s: GATE PASSED: %s tree(s) compile clean under engine v%s — safe to arm\n' \
  "$PROG" "$total" "$ENGINE"
