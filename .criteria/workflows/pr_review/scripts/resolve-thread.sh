#!/bin/sh
# Resolve a single PR review thread by ID.
# Usage: resolve-thread.sh <thread_node_id>
# Emits: resolved:<id> on success.
set -eu

thread_id="${1:-}"
if [ -z "$thread_id" ]; then
  echo "missing_thread_id" >&2
  exit 1
fi

gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -f id="$thread_id" >/dev/null

echo "resolved:${thread_id}"
