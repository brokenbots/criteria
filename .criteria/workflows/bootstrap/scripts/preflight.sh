#!/bin/sh
# Preflight environment + repo-state check for `make self`.
#
# Emits a bare classifier word on stdout (no trailing newline) so the workflow
# switch can match it with `==`. Diagnostic detail goes to stderr.
#
# Classifiers (stdout):
#   ok               all required tooling present, repo clean, main up to date
#   missing_tool     a required CLI (copilot|gh|jq) is not on PATH
#   gh_unauth        gh is not authenticated
#   stale_main       local main is behind origin/main (fast-forward needed)
#   dirty_main       working tree is dirty and we are on main (won't auto-resolve)
#   not_a_repo       current directory is not a git work tree
set -eu

note() { echo "$1" >&2; }

# 1. git work tree.
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  note "current_directory=$(pwd) is not a git work tree"
  printf '%s' "not_a_repo"
  exit 0
fi

# 2. Required tools.
for tool in copilot gh jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    note "required tool not on PATH: ${tool}"
    case "$tool" in
      copilot) note "install: https://docs.github.com/copilot/github-copilot-in-the-cli" ;;
      gh)      note "install: https://cli.github.com/" ;;
      jq)      note "install: https://stedolan.github.io/jq/download/" ;;
    esac
    printf '%s' "missing_tool"
    exit 0
  fi
done

# 3. gh auth.
if ! gh auth status >/dev/null 2>&1; then
  note "gh is not authenticated; run: gh auth login"
  printf '%s' "gh_unauth"
  exit 0
fi

# 4. Main freshness.
git fetch origin --prune >/dev/null 2>&1 || note "warning: git fetch origin failed (offline?); continuing with cached refs"

current_branch="$(git branch --show-current 2>/dev/null || true)"
dirty="$(git status --porcelain)"

if [ "$current_branch" = "main" ] && [ -n "$dirty" ]; then
  note "current branch is main with uncommitted changes:"
  note "$dirty"
  printf '%s' "dirty_main"
  exit 0
fi

if git show-ref --verify --quiet refs/remotes/origin/main && \
   git show-ref --verify --quiet refs/heads/main; then
  ahead=$(git rev-list --count main..origin/main 2>/dev/null || echo 0)
  if [ "$ahead" -gt 0 ]; then
    note "local main is ${ahead} commit(s) behind origin/main; run: git checkout main && git pull --ff-only origin main"
    printf '%s' "stale_main"
    exit 0
  fi
fi

note "preflight ok: copilot+gh+jq present, gh authenticated, main is fresh"
printf '%s' "ok"
