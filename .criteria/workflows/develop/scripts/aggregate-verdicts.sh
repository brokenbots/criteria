#!/bin/sh
# Aggregate VERDICT: lines from the four specialist reviewer reports passed in
# on stdin (concatenated, free-form). Emits a single classifier on stdout:
#
#   unanimous   exactly 4 VERDICT lines present and ALL say "approved"
#   mixed       any other state (some changes_requested, missing verdicts, etc.)
#
# When unanimous, the parent workflow can skip the owner adjudicator and go
# straight to commit. When mixed, the owner adjudicates the disagreements.
#
# Switch matches stdout with == (no trailing newline via printf '%s').
set -eu

input="$(cat)"

total=$(printf '%s' "$input" | grep -cE '^VERDICT:' 2>/dev/null || true)
approved=$(printf '%s' "$input" | grep -cE '^VERDICT: approved\b' 2>/dev/null || true)

echo "total_verdicts=${total} approved=${approved}" >&2

if [ "${total:-0}" = "4" ] && [ "${approved:-0}" = "4" ]; then
  printf '%s' "unanimous"
else
  printf '%s' "mixed"
fi
