#!/bin/sh
# Deterministic aggregated PR status. Classifier word on stdout (no newline);
# detail on stderr for downstream prompts.
#
# Always exits 0 on success; the workflow switch routes via stdout equality.
# Non-zero exit means the call itself failed (no PR, bad git state).
#
# Classifiers (stdout):
#   merged             PR already MERGED; sync local main only
#   ready              checks green, no unresolved threads, !CHANGES_REQUESTED
#   pending            required checks still running; caller should backoff
#   changes_requested  reviewDecision = CHANGES_REQUESTED
#   threads_open       unresolved (and !outdated) threads remain
#   checks_failed      one or more required checks failed
#
# Stderr is k=v context (pr_number, checks state buckets, review_decision,
# unresolved_threads) that downstream agent prompts interpolate.
set -eu

emit() {
  # $1 = classifier word, rest = k=v context lines for stderr
  printf '%s' "$1"
  shift
  while [ $# -gt 0 ]; do
    echo "$1" >&2
    shift
  done
}

branch="$(git branch --show-current 2>/dev/null || true)"
if [ -z "$branch" ] || [ "$branch" = "main" ]; then
  echo "bad_branch:${branch:-detached}" >&2
  exit 1
fi

pr_number="$(gh pr view "$branch" --json number --jq '.number' 2>/dev/null || true)"
if [ -z "$pr_number" ]; then
  echo "no_pr:${branch}" >&2
  exit 1
fi

pr_state="$(gh pr view "$pr_number" --json state --jq '.state')"
if [ "$pr_state" = "MERGED" ]; then
  emit "merged" "pr_number=${pr_number}" "pr_state=${pr_state}"
  exit 0
fi
if [ "$pr_state" = "CLOSED" ]; then
  echo "pr_closed:${pr_number}" >&2
  exit 1
fi

checks_rc=0
checks_json="$(gh pr checks "$pr_number" --required --json bucket,name,state,workflow 2>&1)" || checks_rc=$?

if [ "$checks_rc" -eq 8 ]; then
  bucket_summary="$(printf '%s\n' "$checks_json" | jq -r 'group_by(.bucket) | map([.[0].bucket, (length|tostring)] | join("=")) | .[]' 2>/dev/null || true)"
  emit "pending" "pr_number=${pr_number}" "checks=pending" "${bucket_summary}"
  exit 0
fi
if [ "$checks_rc" -ne 0 ]; then
  emit "checks_failed" "pr_number=${pr_number}" "checks=failed" "details=$(printf '%s' "$checks_json" | tr '\n' '|')"
  exit 0
fi

owner="$(gh repo view --json owner --jq '.owner.login')"
repo="$(gh repo view --json name --jq '.name')"

review_decision="$(gh pr view "$pr_number" --json reviewDecision --jq '.reviewDecision // "REVIEW_REQUIRED"')"

threads_json="$(gh api graphql -f query='query($owner:String!,$repo:String!,$number:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100){totalCount pageInfo{hasNextPage} nodes{id isResolved isOutdated}}}}}' -f owner="$owner" -f repo="$repo" -F number="$pr_number")"
threads_has_next="$(printf '%s' "$threads_json" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage')"
unresolved="$(printf '%s' "$threads_json" | jq '[.data.repository.pullRequest.reviewThreads.nodes[] | select((.isOutdated|not) and (.isResolved|not))] | length')"

if [ "$review_decision" = "CHANGES_REQUESTED" ]; then
  emit "changes_requested" "pr_number=${pr_number}" "review_decision=${review_decision}" "unresolved_threads=${unresolved}"
  exit 0
fi

if [ "$unresolved" -gt 0 ] || [ "$threads_has_next" = "true" ]; then
  emit "threads_open" "pr_number=${pr_number}" "review_decision=${review_decision}" "unresolved_threads=${unresolved}" "review_threads_has_next_page=${threads_has_next}"
  exit 0
fi

bucket_summary="$(printf '%s\n' "$checks_json" | jq -r 'group_by(.bucket) | map([.[0].bucket, (length|tostring)] | join("=")) | .[]' 2>/dev/null || true)"
emit "ready" "pr_number=${pr_number}" "review_decision=${review_decision}" "checks=passed" "unresolved_threads=0" "${bucket_summary}"
