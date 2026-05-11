#!/bin/sh
# Pick the next pending workstream to process.
#
# Scans workstreams/*.md (top-level only — never archived/) and prints a single
# workstream path on stdout (no trailing newline). If nothing is pending, prints
# nothing. Always exits 0; non-zero exit means an unexpected error.
#
# A workstream is "done" iff a branch named `<basename .md>` exists locally or
# on origin AND is a strict ancestor of main (squash-merged or fast-forwarded).
# Anything else (no branch, in-progress branch, branch ahead of main) is pending.
#
# Override: set WORKSTREAM=<path> to force a specific file (must exist).
#
# Designed to be embedded in a make target:
#   ws=$(sh .criteria/workflows/bootstrap/scripts/pick-next-workstream.sh)
#   if [ -z "$ws" ]; then echo "no pending workstreams"; exit 0; fi
set -eu

workstreams_dir="${WORKSTREAMS_DIR:-workstreams}"

if [ ! -d "$workstreams_dir" ]; then
  echo "missing_workstreams_dir:${workstreams_dir}" >&2
  exit 1
fi

if [ -n "${WORKSTREAM:-}" ]; then
  if [ ! -f "$WORKSTREAM" ]; then
    echo "override_not_found:${WORKSTREAM}" >&2
    exit 1
  fi
  printf '%s' "$WORKSTREAM"
  exit 0
fi

git fetch origin --prune >/dev/null 2>&1 || true

main_ref="main"
if git show-ref --verify --quiet refs/remotes/origin/main; then
  main_ref="origin/main"
fi

is_strict_ancestor() {
  git merge-base --is-ancestor "$1" "$2" 2>/dev/null && \
    ! git merge-base --is-ancestor "$2" "$1" 2>/dev/null
}

for f in "$workstreams_dir"/*.md; do
  [ -f "$f" ] || continue
  case "$(basename "$f")" in
    README.md) continue ;;
  esac
  branch="$(basename "$f" .md)"

  merged="no"
  if git show-ref --verify --quiet "refs/remotes/origin/${branch}"; then
    if is_strict_ancestor "origin/${branch}" "$main_ref"; then
      merged="yes"
    fi
  elif git show-ref --verify --quiet "refs/heads/${branch}"; then
    if is_strict_ancestor "$branch" "$main_ref"; then
      merged="yes"
    fi
  fi

  if [ "$merged" = "no" ]; then
    printf '%s' "$f"
    exit 0
  fi
done

# Nothing pending: print nothing, exit 0.
