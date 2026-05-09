# Your First PR to Criteria

<!-- Last reviewed: Phase 3 (2026-05) -->

Welcome to Criteria — a standalone workflow execution engine that compiles HCL
workflow files into finite-state machines and runs them locally or against an
orchestrator server. We're glad you're here.

This guide takes you from zero to a merged pull request. It is intentionally
concrete: real file paths, real commands, and a real worked example. It assumes
you are already comfortable with Go and git, but you do not need to know the
Criteria codebase before you start.

## What to expect

Criteria uses a **workstream-driven contribution model**. Each workstream file
(in `workstreams/`) defines a bounded scope, a list of files that may be
changed, and explicit exit criteria. PRs are expected to match exactly one
workstream. This keeps diffs small and reviews fast.

The best first PRs are self-contained, single-file changes that burn down one
entry from the lint baseline. The linter is a hard CI gate; removing one
suppression is a meaningful contribution that follows the full contribution
path end-to-end.

---

## Step 1 — Pick an issue

Browse the [`good first issue`][gfi] label on the issue tracker. Each issue
includes:

- The exact file and line to change.
- An effort estimate (almost always ≤ 2 hours).
- An "this is a good first contribution because…" paragraph explaining why the
  task is bounded.

[gfi]: https://github.com/brokenbots/criteria/labels/good%20first%20issue

Other labels you will encounter:

| Label | Meaning |
|-------|---------|
| `bug` | Something is broken; fix is expected |
| `enhancement` | New capability or improvement |
| `good first issue` | Self-contained, low-risk, well-scoped |
| `help wanted` | Maintainer wants outside help specifically |

Leave a comment on the issue you intend to pick up so that two contributors
do not work on the same thing at the same time.

---

## Step 2 — Set up your environment

Follow the **Setup** section in [CONTRIBUTING.md](../../CONTRIBUTING.md) — it
covers cloning, `make bootstrap`, and the `make build` / `make test` flow.
Come back here once `make test` passes locally. Do not skip that step: if
tests are already broken, you want to know before you make any changes.

---

## Step 3 — Worked example: a lint baseline burn-down PR

The lint baseline (`.golangci.baseline.yml`) quarantines pre-existing lint
findings so the CI gate is green on day one. Each entry is annotated with the
workstream that will eventually remove it. Removing one suppression — by fixing
the underlying issue — is a great first PR.

The mechanical `gofmt`/`goimports` entries were cleared in Workstream 1.
The entries remaining in the baseline are gocritic style fixes, errcheck
omissions, and complexity findings. This example uses a `gocritic`
`emptyStringTest` entry — the same three-file diff pattern as a
`gofmt`/`goimports` fix, just with a one-line code substitution instead of
running a formatter.

This section walks through the `emptyStringTest` finding in
`internal/plugin/loader.go`.

[gocritic]: https://github.com/go-critic/go-critic

### 3.1 — Find the baseline entry

Open `.golangci.baseline.yml` and locate this block:

```yaml
    - path: internal/plugin/loader.go
      linters:
        - gocritic
      text: 'emptyStringTest: replace `len\(s\) > 0` with `s != ""`'
```

The `path` field tells you which file has the finding; the `text` field shows
the exact gocritic message (with regex-escaped characters — ignore the
backslashes).

### 3.2 — Open the file and make the fix

Open `internal/plugin/loader.go` and find the `stringsTrim` function.
You will see two `for` loop conditions that use `len(s) > 0`:

```go
func stringsTrim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		last := s[len(s)-1]
		...
	}
	return s
}
```

Replace both `len(s) > 0` comparisons with `s != ""`:

```go
func stringsTrim(s string) string {
	for s != "" && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for s != "" {
		last := s[len(s)-1]
		...
	}
	return s
}
```

`len(s) > 0` and `s != ""` are semantically equivalent for a `string` in Go;
the latter is the idiomatic form that gocritic prefers.

### 3.3 — Remove the baseline entry

Delete the four-line block from `.golangci.baseline.yml`:

```yaml
    - path: internal/plugin/loader.go
      linters:
        - gocritic
      text: 'emptyStringTest: replace `len\(s\) > 0` with `s != ""`'
```

Do not leave the block in place — the lint gate checks that you removed the
suppression when you fix the underlying issue.

### 3.4 — Lower the baseline cap

`tools/lint-baseline/cap.txt` contains the maximum allowed number of baseline
entries. Read the current value and subtract 1:

```bash
cat tools/lint-baseline/cap.txt   # e.g. 70
echo 69 > tools/lint-baseline/cap.txt
```

The CI gate (`make lint-baseline-check`) fails if the live count exceeds the
cap, so lowering the cap by 1 enforces that the entry stays removed.

### 3.5 — Run `make ci`

```bash
make ci
```

This runs the full CI suite: build, tests, import-boundary check, golangci-lint
with the merged baseline, baseline cap check, and example workflow validation.
All gates must be green before you open the PR.

If the lint gate fails, double-check that:
- The `len(s) > 0` occurrences are actually changed to `s != ""` in the file.
- The entry is fully deleted from `.golangci.baseline.yml` (no trailing
  blank line or orphaned YAML keys).
- The cap in `tools/lint-baseline/cap.txt` is one less than it was before.

### 3.6 — Open the PR

Create a branch, commit, and push:

```bash
git checkout -b fix/emptystring-loader
git add internal/plugin/loader.go .golangci.baseline.yml tools/lint-baseline/cap.txt
git commit -m "fix: replace len(s)>0 with s!=\"\" in plugin/loader stringsTrim

Removes the gocritic emptyStringTest suppression for internal/plugin/loader.go.
No behavior change; len(s) > 0 and s != \"\" are semantically identical for
a Go string.

Closes #<issue-number>"
git push origin fix/emptystring-loader
```

Open a pull request against `main`. In the PR description:

- Link the issue you are closing with `Closes #NNN`.
- Confirm that `make ci` passed locally.
- Describe in one sentence what changed and why it is safe.

Keep the PR to the three files listed in the `git add` above. Do not bundle
unrelated changes.

---

## Step 4 — What the PR review looks like

Criteria uses a **workstream-reviewer** model. The reviewer's job is to verify:

1. The implementation matches the issue scope — no extra changes sneaking in.
2. The fix is correct — semantics are preserved, no edge cases broken.
3. CI is green — all gates pass without new suppressions.
4. The baseline entry is removed — not left behind or replaced with
   `//nolint:`.

Expect a review within **one week** of opening the PR. You may receive:

- **Approval** — all good, the PR is merged.
- **Comment** — a question or suggestion; respond and push a fixup commit.
- **R1 blocker** — a required change before the PR can merge; address it and
  re-request review.

Small, well-scoped PRs typically reach approval in one round. If you are stuck
on a review comment, ask for clarification — that is always welcome.

---

## Step 5 — What to do next

Once your first PR is merged:

- Browse the [`good first issue`][gfi] label for more items.
- Look at the workstream files in [`workstreams/`](../../workstreams/) for
  larger, structured contributions. Each workstream file contains its own
  scope, allowed-files list, and exit criteria.
- See [docs/contributing/lint-baseline.md](lint-baseline.md) for the full
  burn-down contract if you want to tackle more baseline entries.
- Check `make help` for the full list of available development targets.

Thank you for contributing. Every PR matters.
