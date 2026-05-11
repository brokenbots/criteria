#!/bin/sh
# Idempotent workstream branch preparation.
#
# Derives the branch name from the workstream filename (criteria convention:
# `td-01-foo.md` -> branch `td-01-foo`). Routes the caller via structured stdout:
#
#   already_merged:<branch>   branch is a strict ancestor of main; skip work
#   existing_local:<branch>   local branch exists, ahead of main; continue from it
#   existing_remote:<branch>  remote branch exists, ahead of main; check it out
#   existing_dirty:<branch>   we are on the branch with uncommitted changes
#   created:<branch>          new branch created from main
#
# Exits non-zero on dirty-other-branch or filesystem errors. Never deletes work.
set -eu

workstream_file="${1:-}"

if [ -z "$workstream_file" ] || [ ! -f "$workstream_file" ]; then
  echo "missing_workstream:${workstream_file}" >&2
  exit 1
fi

branch="$(basename "$workstream_file" .md)"
if [ -z "$branch" ]; then
  echo "missing_branch:${workstream_file}" >&2
  exit 1
fi

current_branch="$(git branch --show-current 2>/dev/null || true)"
dirty_status="$(git status --porcelain)"

if [ -n "$dirty_status" ]; then
  if [ "$current_branch" = "$branch" ]; then
    echo "existing_dirty:${branch}"
    exit 0
  fi
  echo "dirty_other:${current_branch:-detached}; expected ${branch}" >&2
  exit 1
fi

git fetch origin --prune >/dev/null 2>&1 || git fetch origin >/dev/null 2>&1 || true

main_ref="main"
if git show-ref --verify --quiet refs/remotes/origin/main; then
  main_ref="origin/main"
fi

is_strict_ancestor() {
  git merge-base --is-ancestor "$1" "$2" 2>/dev/null && \
    ! git merge-base --is-ancestor "$2" "$1" 2>/dev/null
}

if git show-ref --verify --quiet "refs/remotes/origin/${branch}"; then
  if is_strict_ancestor "origin/${branch}" "$main_ref"; then
    echo "already_merged:${branch}"
    exit 0
  fi
fi

if git show-ref --verify --quiet "refs/heads/${branch}"; then
  if is_strict_ancestor "$branch" "$main_ref"; then
    echo "already_merged:${branch}"
    exit 0
  fi

  git checkout "$branch" >/dev/null 2>&1
  if git show-ref --verify --quiet "refs/remotes/origin/${branch}"; then
    git pull --ff-only origin "$branch" >/dev/null 2>&1 || true
  fi
  echo "existing_local:${branch}"
  exit 0
fi

if git show-ref --verify --quiet "refs/remotes/origin/${branch}"; then
  git checkout -b "$branch" --track "origin/${branch}" >/dev/null 2>&1 || git checkout "$branch" >/dev/null 2>&1
  echo "existing_remote:${branch}"
  exit 0
fi

git checkout -b "$branch" "$main_ref" >/dev/null 2>&1
echo "created:${branch}"
