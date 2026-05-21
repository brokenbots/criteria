#!/bin/sh
# Pick the next pending workstream to process.
#
# Scans workstreams/ recursively (excluding archived/) and prints a single
# workstream path on stdout (no trailing newline). If nothing is pending, prints
# nothing. Always exits 0; non-zero exit means an unexpected error.
#
# A workstream is "done" iff a branch named `<basename .md>` exists locally or
# on origin AND is a strict ancestor of BASE_BRANCH (squash-merged or
# fast-forwarded). Anything else (no branch, in-progress branch, branch ahead of
# BASE_BRANCH) is pending.
#
# Override: set WORKSTREAM=<path> to force a specific file (must exist).
#
# Environment:
#   WORKSTREAMS_DIR  root directory to scan (default: workstreams)
#   BASE_BRANCH      integration branch to check merge status against (default: adapter-v2)
#
# Designed to be embedded in a make target:
#   ws=$(sh .criteria/workflows/bootstrap/scripts/pick-next-workstream.sh)
#   if [ -z "$ws" ]; then echo "no pending workstreams"; exit 0; fi
set -eu

workstreams_dir="${WORKSTREAMS_DIR:-workstreams}"
BASE_BRANCH="${BASE_BRANCH:-adapter-v2}"

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

main_ref="$BASE_BRANCH"
if git show-ref --verify --quiet "refs/remotes/origin/${BASE_BRANCH}"; then
  main_ref="origin/${BASE_BRANCH}"
fi

is_strict_ancestor() {
  git merge-base --is-ancestor "$1" "$2" 2>/dev/null && \
    ! git merge-base --is-ancestor "$2" "$1" 2>/dev/null
}

find "$workstreams_dir" -name "*.md" ! -path "*/archived/*" ! -name "README.md" | LC_ALL=C sort | \
while IFS= read -r f; do
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
