#!/usr/bin/env bash
# tools/release/tests/extract-tag-claims_test.sh
#
# Smoke tests for tools/release/extract-tag-claims.sh.
# Each test sets REPO_ROOT to a temporary directory so the REAL script runs
# against controlled input — not an inline copy of the logic.
#
# Usage: ./tools/release/tests/extract-tag-claims_test.sh
# Exit 0 on all pass, non-zero on any failure.

set -euo pipefail

REPO_ROOT_REAL="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SCRIPT="$REPO_ROOT_REAL/tools/release/extract-tag-claims.sh"
TESTDATA="$REPO_ROOT_REAL/tools/release/tests/testdata"

PASS=0
FAIL=0

# Accumulate all temp dirs; clean up once on exit.
TMPDIRS=()
cleanup() {
    if [[ ${#TMPDIRS[@]} -gt 0 ]]; then
        rm -rf "${TMPDIRS[@]}"
    fi
}
trap cleanup EXIT

assert_contains() {
    local desc="$1" expected="$2" actual="$3"
    if echo "$actual" | grep -qxF "$expected"; then
        echo "PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "FAIL: $desc — expected '$expected' in output:"
        echo "$actual" | sed 's/^/  /'
        FAIL=$((FAIL + 1))
    fi
}

assert_not_contains() {
    local desc="$1" unexpected="$2" actual="$3"
    if echo "$actual" | grep -qxF "$unexpected"; then
        echo "FAIL: $desc — unexpected '$unexpected' found in output:"
        echo "$actual" | sed 's/^/  /'
        FAIL=$((FAIL + 1))
    else
        echo "PASS: $desc"
        PASS=$((PASS + 1))
    fi
}

# make_repo ROOT — scaffold the minimum directory tree the script requires
make_repo() {
    local root="$1"
    mkdir -p "$root/docs" "$root/workstreams"
    touch "$root/README.md" "$root/PLAN.md" "$root/CHANGELOG.md" "$root/workstreams/README.md"
}

# ---------------------------------------------------------------------------
# Test: script is executable
# ---------------------------------------------------------------------------
if [[ -x "$SCRIPT" ]]; then
    echo "PASS: script is executable"
    PASS=$((PASS + 1))
else
    echo "FAIL: script is not executable: $SCRIPT"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# Test: CHANGELOG heading in CHANGELOG.md is emitted (root-level file scan)
# ---------------------------------------------------------------------------
t="$(mktemp -d)"; TMPDIRS+=("$t"); make_repo "$t"
printf '## [v9.9.9]\n\nSome release notes.\n' > "$t/CHANGELOG.md"
out="$(REPO_ROOT="$t" "$SCRIPT")"
assert_contains "CHANGELOG.md heading → v9.9.9" "v9.9.9" "$out"

# ---------------------------------------------------------------------------
# Test: "tag" keyword in PLAN.md is emitted (root-level file scan)
# ---------------------------------------------------------------------------
t="$(mktemp -d)"; TMPDIRS+=("$t"); make_repo "$t"
printf '%s\n' '- Close gate: archive, tag `v9.8.0`.' > "$t/PLAN.md"
out="$(REPO_ROOT="$t" "$SCRIPT")"
assert_contains "PLAN.md tag keyword → v9.8.0" "v9.8.0" "$out"

# ---------------------------------------------------------------------------
# Test: positive fixture in docs/ is found (recursive docs/ scan)
# Uses the shipped fixture-positive.md: CHANGELOG heading v9.9.9 + release
# keyword v9.8.0.
# ---------------------------------------------------------------------------
t="$(mktemp -d)"; TMPDIRS+=("$t"); make_repo "$t"
cp "$TESTDATA/fixture-positive.md" "$t/docs/fixture.md"
out="$(REPO_ROOT="$t" "$SCRIPT")"
assert_contains "docs/ fixture: CHANGELOG heading → v9.9.9" "v9.9.9" "$out"
assert_contains "docs/ fixture: release keyword → v9.8.0" "v9.8.0" "$out"

# ---------------------------------------------------------------------------
# Test: docs/ subdirectory traversal (file nested one level deep)
# ---------------------------------------------------------------------------
t="$(mktemp -d)"; TMPDIRS+=("$t"); make_repo "$t"
mkdir -p "$t/docs/roadmap"
printf 'Status: Closed at v9.7.0 release.\n' > "$t/docs/roadmap/summary.md"
out="$(REPO_ROOT="$t" "$SCRIPT")"
assert_contains "docs/roadmap/ traversal → v9.7.0" "v9.7.0" "$out"

# ---------------------------------------------------------------------------
# Test: false-positive fixture — RC versions not emitted; no-keyword semver
# not emitted; tag-keyword semver is emitted.
# Uses the shipped fixture-false-positive.md.
# ---------------------------------------------------------------------------
t="$(mktemp -d)"; TMPDIRS+=("$t"); make_repo "$t"
cp "$TESTDATA/fixture-false-positive.md" "$t/docs/fixture.md"
out="$(REPO_ROOT="$t" "$SCRIPT")"
assert_not_contains "false-positive: v9.9.9-rc1 does NOT emit v9.9.9" "v9.9.9" "$out"
assert_not_contains "false-positive: v9.7.0 (no keyword) NOT emitted" "v9.7.0" "$out"
assert_contains     "false-positive: v9.6.0 (tag keyword) IS emitted" "v9.6.0" "$out"

# ---------------------------------------------------------------------------
# Test: empty repo emits nothing
# ---------------------------------------------------------------------------
t="$(mktemp -d)"; TMPDIRS+=("$t"); make_repo "$t"
out="$(REPO_ROOT="$t" "$SCRIPT")"
if [[ -z "$out" ]]; then
    echo "PASS: empty repo emits nothing"
    PASS=$((PASS + 1))
else
    echo "FAIL: empty repo emitted unexpected output: $out"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# Test: deduplication — same tag from multiple files emitted once
# ---------------------------------------------------------------------------
t="$(mktemp -d)"; TMPDIRS+=("$t"); make_repo "$t"
printf '## [v9.5.0]\n' > "$t/CHANGELOG.md"
printf 'See v9.5.0 release notes.\n' > "$t/docs/note.md"
out="$(REPO_ROOT="$t" "$SCRIPT")"
count="$(echo "$out" | grep -cxF 'v9.5.0' || true)"
if [[ "$count" -eq 1 ]]; then
    echo "PASS: deduplication — v9.5.0 emitted exactly once"
    PASS=$((PASS + 1))
else
    echo "FAIL: deduplication — v9.5.0 emitted $count times (expected 1)"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "Results: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
