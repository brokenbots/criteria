#!/bin/sh
# Deterministic aggregated PR status.
#
# Always exits 0 on success; the workflow routes via `switch` on the first
# stdout line. Non-zero exit means the call itself failed (no PR, bad git state).
#
# First line is always one of:
#   status:merged             — PR already MERGED; sync local main only
#   status:ready              — checks green, no unresolved threads, !CHANGES_REQUESTED
#   status:pending            — required checks still running; caller should backoff
#   status:changes_requested  — reviewDecision = CHANGES_REQUESTED
#   status:threads_open       — unresolved (and !outdated) threads remain
#   status:checks_failed      — one or more required checks failed
#
# Remaining lines are k=v context for downstream prompts (pr_number, checks
# buckets, review_decision, unresolved_threads).
set -eu

branch="$(git branch --show-current 2>/dev/null || true)"
if [ -z "$branch" ] || [ "$branch" = "main" ]; then
  echo "status:error" >&2
  echo "bad_branch:${branch:-detached}" >&2
  exit 1
fi

pr_number="$(gh pr view "$branch" --json number --jq '.number' 2>/dev/null || true)"
if [ -z "$pr_number" ]; then
  echo "status:error" >&2
  echo "no_pr:${branch}" >&2
  exit 1
fi

pr_state="$(gh pr view "$pr_number" --json state --jq '.state')"
if [ "$pr_state" = "MERGED" ]; then
  echo "status:merged"
  echo "pr_number=${pr_number}"
  echo "pr_state=${pr_state}"
  exit 0
fi
if [ "$pr_state" = "CLOSED" ]; then
  echo "status:error" >&2
  echo "pr_closed:${pr_number}" >&2
  exit 1
fi

checks_rc=0
checks_json="$(gh pr checks "$pr_number" --required --json bucket,name,state,workflow 2>&1)" || checks_rc=$?

if [ "$checks_rc" -eq 8 ]; then
  echo "status:pending"
  echo "pr_number=${pr_number}"
  echo "checks=pending"
  printf '%s\n' "$checks_json" | jq -r 'group_by(.bucket) | map([.[0].bucket, (length|tostring)] | join("=")) | .[]' 2>/dev/null || true
  exit 0
fi
if [ "$checks_rc" -ne 0 ]; then
  echo "status:checks_failed"
  echo "pr_number=${pr_number}"
  echo "checks=failed"
  printf '%s\n' "$checks_json"
  exit 0
fi

owner="$(gh repo view --json owner --jq '.owner.login')"
repo="$(gh repo view --json name --jq '.name')"

review_decision="$(gh pr view "$pr_number" --json reviewDecision --jq '.reviewDecision // "REVIEW_REQUIRED"')"

threads_json="$(gh api graphql -f query='query($owner:String!,$repo:String!,$number:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100){totalCount pageInfo{hasNextPage} nodes{id isResolved isOutdated}}}}}' -f owner="$owner" -f repo="$repo" -F number="$pr_number")"
threads_has_next="$(printf '%s' "$threads_json" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage')"
unresolved="$(printf '%s' "$threads_json" | jq '[.data.repository.pullRequest.reviewThreads.nodes[] | select((.isOutdated|not) and (.isResolved|not))] | length')"

if [ "$review_decision" = "CHANGES_REQUESTED" ]; then
  echo "status:changes_requested"
  echo "pr_number=${pr_number}"
  echo "review_decision=${review_decision}"
  echo "unresolved_threads=${unresolved}"
  exit 0
fi

if [ "$unresolved" -gt 0 ] || [ "$threads_has_next" = "true" ]; then
  echo "status:threads_open"
  echo "pr_number=${pr_number}"
  echo "review_decision=${review_decision}"
  echo "unresolved_threads=${unresolved}"
  echo "review_threads_has_next_page=${threads_has_next}"
  exit 0
fi

echo "status:ready"
echo "pr_number=${pr_number}"
echo "review_decision=${review_decision}"
echo "checks=passed"
echo "unresolved_threads=0"
printf '%s\n' "$checks_json" | jq -r 'group_by(.bucket) | map([.[0].bucket, (length|tostring)] | join("=")) | .[]' 2>/dev/null || true
exit 0
