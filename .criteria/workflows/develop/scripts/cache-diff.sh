#!/bin/sh
# Capture the current branch's diff vs origin/main to a shared cache file.
# Reviewers read this instead of each running their own `git diff`, saving
# tokens and a few seconds per parallel agent.
#
# Usage: cache-diff.sh
# Writes: .criteria/tmp/diff.patch, .criteria/tmp/diff.stat
# Stdout: bare classifier — "ok" on success, "no_changes" if diff is empty,
#         "error" if the diff cannot be produced. Switch on this.
set -eu

mkdir -p .criteria/tmp

git fetch origin main >/dev/null 2>&1 || true

if ! git rev-parse --verify origin/main >/dev/null 2>&1; then
  echo "origin/main ref missing; cannot compute diff" >&2
  printf '%s' "error"
  exit 0
fi

git diff origin/main...HEAD > .criteria/tmp/diff.patch
git diff --stat origin/main...HEAD > .criteria/tmp/diff.stat

if [ ! -s .criteria/tmp/diff.patch ]; then
  printf '%s' "no_changes"
  exit 0
fi

bytes=$(wc -c < .criteria/tmp/diff.patch)
echo "wrote .criteria/tmp/diff.patch (${bytes} bytes)" >&2
echo "stat:" >&2
cat .criteria/tmp/diff.stat >&2
printf '%s' "ok"
