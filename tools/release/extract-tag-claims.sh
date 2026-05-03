#!/usr/bin/env bash
# tools/release/extract-tag-claims.sh
#
# Scan tracked documentation for release-tag claims and emit each unique tag
# on its own line.  Used by the tag-claim-check CI job.
#
# Scanned:
#   README.md, PLAN.md, CHANGELOG.md, workstreams/README.md,
#   every *.md file under docs/
#
# Skipped:
#   workstreams/archived/  (historical claims are immutable)
#   tech_evaluations/      (eval reports document past state)
#   .git/
#
# A "tag claim" is a line that satisfies at least one of:
#   (a) CHANGELOG heading:  ## [vX.Y.Z]
#   (b) line contains the word "tag" or "release" (whole-word, case-insensitive)
#       AND a plain semver (pre-release suffixes like -rc1 are not tag claims)
#
# Pre-release version strings (vX.Y.Z-<suffix>) are stripped from lines before
# semver extraction so that RC mentions do not produce false positives.

set -euo pipefail

REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

tmpfile="$(mktemp)"
trap 'rm -f "$tmpfile"' EXIT

# extract_from_file FILE
# Appends any tag claims found in FILE to $tmpfile.
extract_from_file() {
    local file="$1"

    # (a) CHANGELOG-style headings: ## [vX.Y.Z]
    grep -oE '^## \[v[0-9]+\.[0-9]+\.[0-9]+\]' "$file" 2>/dev/null \
        | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' >> "$tmpfile" || true

    # (b) Lines with "tag" or "release" as whole words.
    #     Strip pre-release versions (vX.Y.Z-suffix) first so that mentions
    #     like "v0.3.0-rc1" do not emit "v0.3.0".
    grep -iwE 'tag|release' "$file" 2>/dev/null \
        | sed -E 's/v[0-9]+\.[0-9]+\.[0-9]+-[a-zA-Z0-9][a-zA-Z0-9-]*/PRERELEASE/g' \
        | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' >> "$tmpfile" || true
}

# --- Explicitly tracked files at repo root ---
for f in \
    "$REPO_ROOT/README.md" \
    "$REPO_ROOT/PLAN.md" \
    "$REPO_ROOT/CHANGELOG.md" \
    "$REPO_ROOT/workstreams/README.md"
do
    [[ -f "$f" ]] && extract_from_file "$f"
done

# --- docs/ tree (recursive) ---
while IFS= read -r -d '' f; do
    extract_from_file "$f"
done < <(find "$REPO_ROOT/docs" -type f -name '*.md' -print0)

sort -u "$tmpfile"
