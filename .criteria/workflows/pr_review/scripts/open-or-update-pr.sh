#!/bin/sh
# Idempotently open or update a PR for the current workstream branch.
#
# Usage: open-or-update-pr.sh <workstream_file>
#
# Emits one of:
#   created:<number>    new PR opened
#   updated:<number>    existing PR body refreshed
#   exists:<number>     PR exists, no body update needed
#
# The PR title is derived from the workstream filename. The body is the first
# H1 + Context section from the workstream md, plus a footer noting the run.
set -eu

workstream_file="${1:-}"

if [ -z "$workstream_file" ] || [ ! -f "$workstream_file" ]; then
  echo "missing_workstream:${workstream_file}" >&2
  exit 1
fi

branch="$(git branch --show-current 2>/dev/null || true)"
if [ -z "$branch" ] || [ "$branch" = "main" ]; then
  echo "bad_branch:${branch:-detached}" >&2
  exit 1
fi

# Push branch (idempotent; first push sets upstream).
git push --set-upstream origin "$branch" >/dev/null 2>&1 || git push origin "$branch" >/dev/null 2>&1 || {
  echo "push_failed:${branch}" >&2
  exit 1
}

# Title: strip leading `# ` from first heading; fallback to branch name.
title="$(awk '/^# / { sub(/^# /, ""); print; exit }' "$workstream_file")"
if [ -z "$title" ]; then
  title="$branch"
fi

# Body: workstream filename pointer + first 60 lines of the md (Context + headers
# give reviewers enough to navigate). PR review agent will refine if needed.
body_file="$(mktemp)"
trap 'rm -f "$body_file"' EXIT

printf 'Implements `%s`.\n\n' "$workstream_file" > "$body_file"
head -n 60 "$workstream_file" >> "$body_file"
printf '\n\n---\n_Opened by `.criteria/workflows/pr_review`._\n' >> "$body_file"

existing="$(gh pr view "$branch" --json number,state --jq '.number' 2>/dev/null || true)"

if [ -z "$existing" ]; then
  number="$(gh pr create --base main --head "$branch" --title "$title" --body-file "$body_file" --json number --jq '.number')"
  echo "created:${number}"
  exit 0
fi

state="$(gh pr view "$existing" --json state --jq '.state')"
if [ "$state" != "OPEN" ]; then
  echo "exists:${existing}"
  exit 0
fi

gh pr edit "$existing" --title "$title" --body-file "$body_file" >/dev/null
echo "updated:${existing}"
