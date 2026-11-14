package smoke

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The docs-only CI gate (path-filtered skip of repo-state jobs on pull
// requests whose changed files are all under docs/** or named *.md; pushes
// to main and mixed diffs always run full CI; empty diffs and failed
// classification fail closed to full CI in .github/scripts/detect-docs-only.sh)
// is enforced in the job graphs of .github/workflows/ci.yml and
// .github/workflows/container-e2e.yml. These guards pin the wiring (needs
// legs plus docs_only conditions, the skip-tolerant all-checks aggregate,
// the positive docs-only-skip signal, the read-only token permissions block,
// classifier coverage under make lint-sh) and the classifier's behavior
// against real git fixtures, so a workflow edit cannot silently ungate a
// repo-state scan or loosen the skip-tolerant aggregate.

type ciWorkflow struct {
	Jobs        map[string]ciJob  `yaml:"jobs"`
	Permissions map[string]string `yaml:"permissions"`
}

type ciJob struct {
	Name    string            `yaml:"name"`
	Needs   ciNeeds           `yaml:"needs"`
	If      string            `yaml:"if"`
	Outputs map[string]string `yaml:"outputs"`
	Steps   []ciStep          `yaml:"steps"`
}

// ciStep models the subset of a step needed to extract the all-checks
// aggregate recompute script and to pin the changes job's classify wiring.
// Env values stay as yaml.Node because GHA expressions and the numeric
// values used elsewhere in the workflow are not strings.
type ciStep struct {
	Run   string               `yaml:"run"`
	Env   map[string]yaml.Node `yaml:"env"`
	Uses  string               `yaml:"uses"`
	If    string               `yaml:"if"`
	ID    string               `yaml:"id"`
	Shell string               `yaml:"shell"`
}

// ciNeeds accepts both YAML shapes of a workflow `needs` entry (scalar or
// sequence).
type ciNeeds struct {
	items []string
}

func (n *ciNeeds) UnmarshalYAML(node *yaml.Node) error {
	var single string
	if err := node.Decode(&single); err == nil {
		n.items = []string{single}
		return nil
	}
	return node.Decode(&n.items)
}

func (n ciNeeds) Contains(name string) bool {
	for _, item := range n.items {
		if item == name {
			return true
		}
	}
	return false
}

func loadCIWorkflow(t *testing.T, name string) ciWorkflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(findModuleRoot(t), ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s parsed to no jobs", name)
	}
	return wf
}

const (
	docsOnlyGate        = "needs.changes.outputs.docs_only != 'true'"
	docsOnlyPlaceholder = "needs.changes.outputs.docs_only == 'true'"
	allChecksIf         = "${{ !cancelled() }}"
)

// TestCIWorkflowDocsOnlyGates pins the complete docs-only wiring in both
// PR-triggered workflows: which jobs are gated, under which conditions, and
// how the aggregate treats skips.
func TestCIWorkflowDocsOnlyGates(t *testing.T) {
	t.Parallel()

	ci := loadCIWorkflow(t, "ci.yml")

	changes, ok := ci.Jobs["changes"]
	if !ok {
		t.Fatal("ci.yml has no changes job; docs-only gating is unbacked")
	}
	if changes.Outputs["docs_only"] == "" {
		t.Fatal("changes must publish the docs_only job output consumed by the repo-state gates")
	}

	// Repo-state heavy jobs, gated for docs-only PRs: none of them read or
	// affect documentation.
	gated := []string{"unit-tests", "e2e", "osv-scan", "govulncheck", "deps-report", "coverage-check"}
	for _, name := range gated {
		job, ok := ci.Jobs[name]
		if !ok {
			t.Errorf("job %s disappeared: the docs-only gate must track the CI graph", name)
			continue
		}
		if !job.Needs.Contains("changes") {
			t.Errorf("job %s must need the changes job to evaluate needs.changes.outputs.docs_only", name)
		}
		if job.If != docsOnlyGate {
			t.Errorf("job %s: if = %q, want %q", name, job.If, docsOnlyGate)
		}
	}
	if !ci.Jobs["coverage-check"].Needs.Contains("unit-tests") {
		t.Error("coverage-check must keep needing unit-tests for its coverage diff")
	}
	// The floor guard keeps its PR-only scoping (non-PR events have no base
	// floor file to diff) but must also be docs-only gated.
	if got := ci.Jobs["coverage-floor-ratchet-guard"].If; got != "github.event_name == 'pull_request' && "+docsOnlyGate {
		t.Errorf("coverage-floor-ratchet-guard must keep its PR-only scoping: if = %q", got)
	}
	if !ci.Jobs["coverage-floor-ratchet-guard"].Needs.Contains("changes") {
		t.Error("coverage-floor-ratchet-guard must need the changes job")
	}

	// release-artifacts rides unit-tests/e2e; with docs-only PRs skipping
	// those it must also need changes so skipped deps keep it skipped.
	if !ci.Jobs["release-artifacts"].Needs.Contains("changes") {
		t.Error("release-artifacts must need changes so docs-only PRs keep it skipped")
	}

	placeholder, ok := ci.Jobs["docs-only-skip"]
	if !ok {
		t.Fatal("docs-only-skip placeholder job is missing; docs-only PRs would show an all-skipped board")
	}
	if !placeholder.Needs.Contains("changes") {
		t.Error("docs-only-skip must declare needs: [changes] for its condition")
	}
	if placeholder.If != docsOnlyPlaceholder {
		t.Errorf("docs-only-skip: if = %q, want %q", placeholder.If, docsOnlyPlaceholder)
	}

	// Aggregator: recomputes needs results instead of relying on plain needs
	// succession, so skipped (docs-only) jobs count as green while failures
	// still fail the gate.
	all, ok := ci.Jobs["all-checks"]
	if !ok {
		t.Fatal("all-checks aggregator job is missing")
	}
	if all.If != allChecksIf {
		t.Errorf("all-checks: if = %q, want %q (skip-tolerant aggregate)", all.If, allChecksIf)
	}
	// A failed changes job skips its dependents; if the aggregate accepted
	// those dependent skips while ignoring the classification result, a
	// broken classifier would produce a green CI set.
	if !all.Needs.Contains("changes") {
		t.Fatal("all-checks must need changes so a failed docs-only classification fails the aggregate")
	}
	for _, name := range []string{"lint", "unit-tests", "e2e", "install-script-syntax", "proto-drift", "workspace-sync-drift", "osv-scan", "govulncheck", "coverage-check", "coverage-floor-ratchet-guard"} {
		if !all.Needs.Contains(name) {
			t.Errorf("all-checks must keep needing %s", name)
		}
	}

	// The classifier is scoped to repo-state jobs: cheap and contract jobs
	// run on every PR, docs-only or not, and must not reference docs_only.
	for _, name := range []string{"lint", "install-script-syntax", "homebrew-tap-script", "proto-drift", "workspace-sync-drift", "release-artifacts"} {
		job, ok := ci.Jobs[name]
		if !ok {
			t.Errorf("job %s disappeared: the always-on job list must track the CI graph", name)
			continue
		}
		if strings.Contains(job.If, "docs_only") {
			t.Errorf("job %s references docs_only (cheap job, always runs): if = %q", name, job.If)
		}
	}

	// container-e2e.yml carries the same wiring (outputs cannot cross
	// workflow files; its release and schedule lanes run via workflow_call
	// and are non-pull_request events, so they always run full CI).
	container := loadCIWorkflow(t, "container-e2e.yml")
	if change, ok := container.Jobs["changes"]; !ok || change.Outputs["docs_only"] == "" {
		t.Error("container-e2e.yml must carry its own changes job publishing docs_only")
	}
	ce2e, ok := container.Jobs["container-e2e"]
	if !ok {
		t.Fatal("container-e2e.yml has no container-e2e job")
	}
	if !ce2e.Needs.Contains("changes") {
		t.Error("container-e2e job must need its changes job")
	}
	if ce2e.If != docsOnlyGate {
		t.Errorf("container-e2e: if = %q, want %q", ce2e.If, docsOnlyGate)
	}

	// The classifier script stays executable and under shell syntax checks.
	script := filepath.Join(findModuleRoot(t), ".github", "scripts", "detect-docs-only.sh")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("stat classifier script: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("detect-docs-only.sh must stay executable")
	}
	makefile, err := os.ReadFile(filepath.Join(findModuleRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if !strings.Contains(string(makefile), "detect-docs-only.sh") {
		t.Error("make lint-sh must check .github/scripts/detect-docs-only.sh")
	}
}

// TestCIWorkflowsDeclareReadOnlyPermissions pins the least-privilege GitHub
// token contract on both PR-triggered workflows: a workflow-level
// permissions mapping grants read-only repository content to every job and
// nothing broader. Write scopes, if a job ever needs one, belong on that
// job alone.
func TestCIWorkflowsDeclareReadOnlyPermissions(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"ci.yml", "container-e2e.yml"} {
		t.Run(name, func(t *testing.T) {
			wf := loadCIWorkflow(t, name)
			if wf.Permissions == nil {
				t.Fatalf("%s has no workflow-level permissions block; every job inherits the full default token", name)
			}
			if got := wf.Permissions["contents"]; got != "read" {
				t.Errorf("%s: permissions.contents = %q, want \"read\"", name, got)
			}
			for scope, level := range wf.Permissions {
				if scope != "contents" && level != "none" {
					t.Errorf("%s: workflow-level permissions[%s] = %q; only contents: read (or none) belongs at workflow level", name, scope, level)
				}
			}
		})
	}
}

// TestCIChangesJobSafeWithoutRepository pins the non-pull_request safety of
// the changes job in both PR-triggered workflows: the job skips
// actions/checkout on every non-PR event, so its classify step must never
// reference the repo-relative classifier script without a repository — it
// must short-circuit to docs_only=false first. A regression here pushes
// push/workflow_dispatch/workflow_call runs into exit 127 on the missing
// script, which skips every repo-state dependent and hard-fails the
// skip-tolerant all-checks aggregate.
func TestCIChangesJobSafeWithoutRepository(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"ci.yml", "container-e2e.yml"} {
		t.Run("wiring/"+name, func(t *testing.T) {
			job, ok := loadCIWorkflow(t, name).Jobs["changes"]
			if !ok {
				t.Fatalf("%s has no changes job", name)
			}
			var classify *ciStep
			unconditionalCheckout := false
			for i := range job.Steps {
				step := &job.Steps[i]
				if step.ID == "detect" {
					classify = step
				}
				if strings.HasPrefix(step.Uses, "actions/checkout") && step.If == "" {
					unconditionalCheckout = true
				}
			}
			if classify == nil {
				t.Fatalf("%s changes job has no classify step (id: detect)", name)
			}
			if unconditionalCheckout {
				return // checkout is unconditional: the script always has its repo
			}
			// With the checkout skipped there is no repository, so everything
			// the short-circuit needs (the EVENT_NAME comparison, the
			// docs_only=false output, the positive exit) must precede the
			// script reference.
			scriptIdx := strings.Index(classify.Run, ".github/scripts/detect-docs-only.sh")
			if scriptIdx < 0 {
				t.Fatal("classify step never invokes the classifier script")
			}
			guard := classify.Run[:scriptIdx]
			for _, want := range []string{
				`$EVENT_NAME" != "pull_request"`,
				"docs_only=false",
				"GITHUB_OUTPUT",
				"exit 0",
			} {
				if !strings.Contains(guard, want) {
					t.Errorf("classify step must short-circuit non-pull_request events to docs_only=false before invoking the script; %q missing before the script reference:\n%s", want, classify.Run)
				}
			}
		})

		t.Run("workspace-less/"+name, func(t *testing.T) {
			job, ok := loadCIWorkflow(t, name).Jobs["changes"]
			if !ok {
				t.Fatalf("%s has no changes job", name)
			}
			var classifyStep ciStep
			for _, step := range job.Steps {
				if step.ID == "detect" {
					classifyStep = step
				}
			}
			if classifyStep.ID != "detect" {
				t.Fatalf("%s changes job has no classify step (id: detect)", name)
			}
			script := filepath.Join(t.TempDir(), "classify.sh")
			if err := os.WriteFile(script, []byte(classifyStep.Run), 0o755); err != nil {
				t.Fatalf("write classify script: %v", err)
			}
			shell := classifyStep.Shell
			if shell == "" {
				shell = "bash" // GHA default shell for run steps, invoked with -e
			}
			for _, event := range []string{"push", "workflow_dispatch", "workflow_call"} {
				t.Run(event, func(t *testing.T) {
					// The working directory is an EMPTY temp dir on purpose,
					// mirroring a runner where the conditional checkout did
					// not fetch the repository.
					cmd := exec.Command(shell, "-e", script)
					cmd.Dir = t.TempDir()
					outputPath := filepath.Join(t.TempDir(), "github-output")
					cmd.Env = append(os.Environ(), "EVENT_NAME="+event, "BASE_REF=main", "GITHUB_OUTPUT="+outputPath)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("classify step in an empty directory (EVENT_NAME=%s): %v\n%s", event, err, out)
					}
					data, err := os.ReadFile(outputPath)
					if err != nil {
						t.Fatalf("read GITHUB_OUTPUT (EVENT_NAME=%s): %v", event, err)
					}
					if !strings.Contains(string(data), "docs_only=false") {
						t.Errorf("EVENT_NAME=%s: GITHUB_OUTPUT missing docs_only=false, got:\n%s", event, data)
					}
				})
			}
		})
	}
}

// TestAllChecksRecomputeScript executes the all-checks aggregate recompute
// script from ci.yml under POSIX sh with explicit result fixtures. The
// aggregate is the docs-only safeguard: skipped (docs-only) legs must count
// as green while any failure, cancellation, or unrecognised result must fail
// the gate, and the changes job itself must report success — not merely
// skip — so a broken classification cannot yield a green aggregate.
func TestAllChecksRecomputeScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Parallel()

	ci := loadCIWorkflow(t, "ci.yml")
	all, ok := ci.Jobs["all-checks"]
	if !ok {
		t.Fatal("all-checks aggregator job is missing")
	}
	if len(all.Steps) != 1 {
		t.Fatalf("all-checks must stay a single recompute step, have %d", len(all.Steps))
	}
	step := all.Steps[0]
	if step.Run == "" {
		t.Fatal("all-checks recompute step has no run script")
	}
	resultsNode, ok := step.Env["RESULTS"]
	if !ok {
		t.Fatal("recompute step must declare the RESULTS env contract")
	}
	if node, ok := step.Env["CHANGES_RESULT"]; !ok || node.Value == "" {
		t.Fatal("recompute step must declare the CHANGES_RESULT env contract")
	}

	// Tracked legs are the "<job>=${{ ... }}" lines of the workflow's own
	// RESULTS contract, so the fixtures keep following the aggregate list.
	var legs []string
	for _, line := range strings.Split(resultsNode.Value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, _, found := strings.Cut(line, "=")
		if !found {
			t.Fatalf("RESULTS line %q is not a <job>=<result expression> entry", line)
		}
		legs = append(legs, name)
	}
	if len(legs) == 0 {
		t.Fatal("RESULTS must carry one entry per needs leg")
	}
	for _, guarded := range []string{"lint", "unit-tests", "osv-scan", "govulncheck"} {
		if !slices.Contains(legs, guarded) {
			t.Errorf("RESULTS must track %s so its red result fails the aggregate", guarded)
		}
	}

	script := filepath.Join(t.TempDir(), "recompute.sh")
	if err := os.WriteFile(script, []byte(step.Run), 0o755); err != nil {
		t.Fatalf("write recompute script: %v", err)
	}

	run := func(t *testing.T, results, changes string) (int, string) {
		t.Helper()
		cmd := exec.Command("sh", "-e", script)
		cmd.Env = append(os.Environ(), "RESULTS="+results, "CHANGES_RESULT="+changes)
		out, err := cmd.CombinedOutput()
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("run recompute script: %v\n%s", err, out)
			}
			return exitErr.ExitCode(), string(out)
		}
		return 0, string(out)
	}
	results := func(override map[string]string, def string) string {
		lines := make([]string, 0, len(legs))
		for _, leg := range legs {
			value := def
			if given, ok := override[leg]; ok {
				value = given
			}
			lines = append(lines, leg+"="+value)
		}
		return strings.Join(lines, "\n")
	}

	// Alternating legs: a green gate must accept skips between successes
	// regardless of list order.
	mixed := make(map[string]string, len(legs))
	for i, leg := range legs {
		if i%2 == 0 {
			mixed[leg] = "success"
		} else {
			mixed[leg] = "skipped"
		}
	}

	for _, tc := range []struct {
		name    string
		fixture string
	}{
		{"all_success", results(nil, "success")},
		{"docs_only_all_skipped", results(nil, "skipped")},
		{"mixed_success_and_skipped", results(mixed, "success")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := run(t, tc.fixture, "success")
			if code != 0 {
				t.Fatalf("exit = %d, want 0\n%s", code, out)
			}
			if !strings.Contains(out, "All required checks passed") {
				t.Errorf("green aggregate must emit its passing signal: output %s", out)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		fixture string
		changes string
		mention string
	}{
		{"one_leg_fails", results(map[string]string{"osv-scan": "failure"}, "success"), "success", "osv-scan"},
		{"one_leg_cancelled", results(map[string]string{"unit-tests": "cancelled"}, "success"), "success", "unit-tests"},
		{"unknown_result", results(map[string]string{"lint": "in_progress"}, "success"), "success", "lint"},
		{"changes_failure", results(nil, "success"), "failure", "changes"},
		{"changes_skipped", results(nil, "skipped"), "skipped", "changes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := run(t, tc.fixture, tc.changes)
			if code == 0 {
				t.Fatalf("exit = 0, want failure\n%s", out)
			}
			if !strings.Contains(out, "::error::") {
				t.Errorf("red aggregate must emit an actionable ::error:: line: output %s", out)
			}
			if !strings.Contains(out, tc.mention) {
				t.Errorf("red aggregate must name the offending job %q: output %s", tc.mention, out)
			}
		})
	}
}

// TestDetectDocsOnlyScript exercises the classification command contract on
// real git fixtures: docs/ tree, root markdown, deletions, renames in and
// out of docs, mixed diffs, empty diffs, non-PR events, base movement, and
// unset inputs.
func TestDetectDocsOnlyScript(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Run("docs_tree_and_root_markdown", func(t *testing.T) {
		repo := newGitFixture(t)
		writeAndCommit(t, repo, map[string]string{
			"docs/adr-0004/decision.md": "new ADR text",
			"NOTES.md":                  "root-level markdown",
		})
		classify(t, repo, wantDocsOnly{docsOnly: true})
	})

	t.Run("changelog_counts_as_docs", func(t *testing.T) {
		repo := newGitFixture(t)
		writeAndCommit(t, repo, map[string]string{"CHANGELOG.md": "## Unreleased\n\n- entry\n"})
		classify(t, repo, wantDocsOnly{docsOnly: true})
	})

	t.Run("deleted_docs_file", func(t *testing.T) {
		// A PR that deletes docs shipped in the base commit is still
		// docs-only; the deletion appears in the merge-base diff.
		repo := newGitFixture(t)
		removeAndCommit(t, repo, "docs/base.md")
		classify(t, repo, wantDocsOnly{docsOnly: true})
	})

	t.Run("mixed_diff_names_the_non_docs_file", func(t *testing.T) {
		repo := newGitFixture(t)
		writeAndCommit(t, repo, map[string]string{
			"docs/note.md": "note",
			"cmd/tool.go":  "package main\n",
		})
		classify(t, repo, wantDocsOnly{docsOnly: false, nonDocMentioned: "cmd/tool.go"})
	})

	t.Run("workflow_and_proto_changes", func(t *testing.T) {
		repo := newGitFixture(t)
		writeAndCommit(t, repo, map[string]string{
			".github/workflows/ci.yml": "name: CI\n",
		})
		classify(t, repo, wantDocsOnly{docsOnly: false})
	})

	t.Run("module_files", func(t *testing.T) {
		repo := newGitFixture(t)
		writeAndCommit(t, repo, map[string]string{
			"go.mod": "module example.com/fake\n",
			"go.sum": "unused\n",
		})
		classify(t, repo, wantDocsOnly{docsOnly: false})
	})

	t.Run("empty_diff_fails_closed", func(t *testing.T) {
		repo := newGitFixture(t)
		gitIn(t, repo, "commit", "--allow-empty", "-m", "empty PR commit")
		classify(t, repo, wantDocsOnly{docsOnly: false})
	})

	t.Run("non_pull_request_events_always_full_CI", func(t *testing.T) {
		repo := newGitFixture(t)
		for _, event := range []string{"push", "workflow_dispatch"} {
			writeAndCommit(t, repo, map[string]string{fmt.Sprintf("docs/%s.md", event): "docs"})
			classifyEvent(t, repo, event, wantDocsOnly{docsOnly: false})
		}
	})

	t.Run("docs_only_after_base_moved", func(t *testing.T) {
		repo := newGitFixture(t)
		writeAndCommit(t, repo, map[string]string{"docs/later.md": "docs"})
		moveBaseForward(t, repo, map[string]string{"cmd/other.go": "package main\n"})
		classify(t, repo, wantDocsOnly{docsOnly: true})
	})

	t.Run("renamed_code_file_into_docs", func(t *testing.T) {
		// A rename lists only its destination path unless renames are
		// disabled, so cmd/tool.go moving to docs/tool.md must not
		// self-classify as docs-only while the diff removes it from the
		// code tree.
		repo := newGitFixture(t)
		gitIn(t, repo, "mv", "base.go", "docs/moved.md")
		gitIn(t, repo, "commit", "--quiet", "-m", "fixture rename")
		classify(t, repo, wantDocsOnly{docsOnly: false, nonDocMentioned: "base.go"})
	})

	t.Run("renamed_docs_file_stays_docs_only", func(t *testing.T) {
		repo := newGitFixture(t)
		gitIn(t, repo, "mv", "docs/base.md", "docs/guide.md")
		gitIn(t, repo, "commit", "--quiet", "-m", "docs rename")
		classify(t, repo, wantDocsOnly{docsOnly: true})
	})

	t.Run("unset_inputs_abort_the_step", func(t *testing.T) {
		repo := newGitFixture(t)
		script := classifierPath(t)
		cases := [][]string{
			{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()},
			{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "EVENT_NAME=pull_request"},
		}
		for i, env := range cases {
			cmd := exec.Command(script)
			cmd.Dir = repo
			cmd.Env = env
			if _, err := cmd.CombinedOutput(); err == nil {
				t.Errorf("case %d: classifier must fail closed with unset required inputs", i)
			}
		}
	})
}

// wantDocsOnly asserts the classifier's stdout verdict.
type wantDocsOnly struct {
	docsOnly        bool
	nonDocMentioned string
}

func classifierPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(findModuleRoot(t), ".github", "scripts", "detect-docs-only.sh")
}

func classify(t *testing.T, repo string, want wantDocsOnly) {
	t.Helper()
	classifyEvent(t, repo, "pull_request", want)
}

func classifyEvent(t *testing.T, repo, event string, want wantDocsOnly) {
	t.Helper()
	cmd := exec.Command(classifierPath(t))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"EVENT_NAME="+event,
		"BASE_REF=main")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("classifier (%s) failed: %v\n%s", event, err, out)
	}
	// The verdict is the classifier's only stdout line; diagnostics go to
	// stderr and never contain "docs_only=".
	verdict := fmt.Sprintf("docs_only=%t", want.docsOnly)
	if !strings.Contains(string(out), verdict) {
		t.Fatalf("classifier (%s): output %q missing %q", event, out, verdict)
	}
	if want.nonDocMentioned != "" && !strings.Contains(string(out), want.nonDocMentioned) {
		t.Errorf("classifier (%s) should name the first non-docs file %q: output %s", event, want.nonDocMentioned, out)
	}
}

// newGitFixture creates a bare "origin" holding a code base commit on main,
// plus a clean clone checked out at that commit. The clone is returned; the
// script runs inside it with BASE_REF=main, exactly as the changes checkout
// prepares it.
func newGitFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	repo := filepath.Join(base, "repo")
	// --initial-branch=main keeps the bare origin's unborn HEAD on the branch
	// that is pushed to, so later clones follow refs/heads/main instead of
	// committing on an unborn default branch and racing HEAD:main pushes.
	gitIn(t, base, "init", "--bare", "--initial-branch=main", origin)
	gitIn(t, base, "clone", "--quiet", origin, repo)
	gitIn(t, repo, "checkout", "-B", "main")
	writeAndCommit(t, repo, map[string]string{
		"base.go":      "package example\n",
		"docs/base.md": "base docs\n",
	})
	gitIn(t, repo, "push", "--quiet", "origin", "main")
	return repo
}

// moveBaseForward advances origin/main past the fixture's fork point with a
// code commit pushed from a second clone, then refreshes the test clone's
// remote tracking refs. The classifier's merge-base must ignore the new
// base-side changes when computing the PR diff.
func moveBaseForward(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	parent := filepath.Dir(repo)
	other := filepath.Join(parent, "baseclone")
	gitIn(t, parent, "clone", "--quiet", filepath.Join(parent, "origin.git"), other)
	writeAndCommit(t, other, files)
	gitIn(t, other, "push", "--quiet", "origin", "HEAD:main")
	gitIn(t, repo, "fetch", "--quiet", "origin")
}

func writeAndCommit(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--quiet", "-m", "fixture change")
}

func removeAndCommit(t *testing.T, repo, rel string) {
	t.Helper()
	gitIn(t, repo, "rm", "--quiet", rel)
	gitIn(t, repo, "commit", "--quiet", "-m", "fixture deletion")
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=tester",
		"GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=tester",
		"GIT_COMMITTER_EMAIL=tester@example.com",
		"GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
