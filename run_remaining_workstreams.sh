#!/usr/bin/env bash
set -euo pipefail

OWNER="brokenbots"
REPO="overseer"
WORKSTREAM_IDS=(04 05 06 07 08 09)

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

require_cmd git
require_cmd gh

if [[ ! -x "bin/criteria" ]]; then
  echo "Missing executable: bin/criteria" >&2
  exit 1
fi

for i in "${WORKSTREAM_IDS[@]}"; do
  echo "=== Workstream $i ==="

  git checkout main
  git pull --ff-only origin main

  WORKSTREAM=$(ls "workstreams/${i}-"*.md 2>/dev/null | head -n 1 || true)
  if [[ -z "${WORKSTREAM}" ]]; then
    echo "No workstream file found for ${i}; skipping"
    continue
  fi

  echo "Running: ${WORKSTREAM}"
  bin/criteria apply examples/workstream_review_loop.hcl --var workstream_file="${WORKSTREAM}"

  BR=$(git branch --show-current)

  if [[ "${BR}" == "main" ]]; then
    if ! git diff --quiet || ! git diff --cached --quiet; then
      git add -A
      git commit -m "workstream: complete ${WORKSTREAM}" || true
      git push origin main
    else
      echo "No changes to commit on main"
    fi
    continue
  fi

  if ! git diff --quiet || ! git diff --cached --quiet; then
    git add -A
    git commit -m "workstream: complete ${WORKSTREAM}" || true
  else
    echo "No changes to commit on ${BR}"
  fi

  git push -u origin "${BR}"

  PR_URL=$(gh pr create \
    --repo "${OWNER}/${REPO}" \
    --base main \
    --head "${BR}" \
    --title "workstream: complete ${WORKSTREAM}" \
    --body "Automated completion for ${WORKSTREAM}." 2>/dev/null || true)

  if [[ -n "${PR_URL}" ]]; then
    gh pr merge "${PR_URL}" --squash --delete-branch --repo "${OWNER}/${REPO}"
  else
    gh pr merge "${BR}" --squash --delete-branch --repo "${OWNER}/${REPO}"
  fi

  git checkout main
  git pull --ff-only origin main

done

echo "All requested workstreams processed."
