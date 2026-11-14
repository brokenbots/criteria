#!/usr/bin/env bash
# Classifies the CI diff of a workflow run as documentation-only or mixed,
# backing the docs-only gating in .github/workflows/ci.yml and
# .github/workflows/container-e2e.yml.
#
# A docs-only diff is one where EVERY changed file matches docs/** or
# **/*.md (CHANGELOG.md and other root markdown count as docs). Documentation
# cannot introduce an advisory, cannot change the dependency tree, and cannot
# affect release artifacts, so the repo-state heavy jobs (vuln scans,
# dependency freshness, unit tests, build/E2E, container E2E, coverage) skip
# such pull requests instead of failing on pre-existing main health. Any
# other file — Go sources, go.mod/go.sum, Dockerfiles, proto, workflow YAML —
# makes the diff mixed, which runs the full CI set. Renames list both their
# paths, so a rename touching a non-docs path is mixed.
#
# The skip applies to pull_request events only: pushes to main always run
# full CI so main health (real advisory clearing) is never skipped.
#
# Inputs (env):
#   EVENT_NAME  github.event_name; only "pull_request" diffs are classified.
#   BASE_REF    github.base_ref (pull_request target branch).
# A pull_request checkout must include enough history for merge-base with
# origin/${BASE_REF} (actions/checkout with fetch-depth: 0).
#
# Output (stdout): docs_only=true|false — the caller feeds this into
# $GITHUB_OUTPUT as a job output. Diagnostics go to stderr. Anything that
# cannot be classified (missing head, absent base, empty diff) fails closed
# to docs_only=false, i.e. full CI.

set -eu

event="${EVENT_NAME:?EVENT_NAME is required}"
if [ "$event" != "pull_request" ]; then
  echo "event $event: not a pull request, running full CI" >&2
  echo "docs_only=false"
  exit 0
fi

base_ref="${BASE_REF:?BASE_REF is required for pull_request events}"
base="origin/${base_ref}"
head_sha="$(git rev-parse HEAD)"
if ! merge_base="$(git merge-base "$base" "$head_sha")"; then
  echo "cannot compute merge-base against ${base}, running full CI (fail closed)" >&2
  echo "docs_only=false"
  exit 0
fi

# Empty or undiffable PRs are not observable as docs-only: run full CI.
docs_only=true
changed=0
while IFS= read -r file; do
  [ -z "$file" ] && continue
  changed=$((changed + 1))
  case "$file" in
    docs/*|*.md) ;;
    *)
      echo "non-docs change: $file" >&2
      docs_only=false
      break
      ;;
  esac
# --no-renames: a default rename listing shows only the destination path, so
# cmd/tool.go moving to docs/tool.md would list just "docs/tool.md" and
# self-classify as docs-only while the diff removes it from the code tree.
# Listing both sides keeps renames touching non-docs paths mixed.
done <<EOF
$(git diff --name-only --no-renames "$merge_base" "$head_sha")
EOF

if [ "$changed" -eq 0 ]; then
  echo "PR diff contains no changed files: full CI (fail closed)" >&2
  docs_only=false
fi

echo "docs_only=$docs_only"
