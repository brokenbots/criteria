package envreg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegistrySanity pins the structural contract of the table itself:
// unique names, declared kinds, single-line docs, and sorted views.
func TestRegistrySanity(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range All() {
		if seen[v.Env()] {
			t.Errorf("duplicate registration %s", v.Env())
		}
		seen[v.Env()] = true
		if !v.Kind().Valid() {
			t.Errorf("%s: undeclared kind %q", v.Env(), v.Kind())
		}
		if v.Scope() == "" || v.Doc() == "" {
			t.Errorf("%s: missing scope or doc", v.Env())
		}
		if strings.ContainsAny(v.Doc(), "\n\r") {
			t.Errorf("%s: doc must stay on one line", v.Env())
		}
	}
	all := All()
	for i := 1; i < len(all); i++ {
		if all[i-1].Env() >= all[i].Env() {
			t.Errorf("All() not sorted at %s", all[i].Env())
		}
	}
	docs := Documented()
	for i := 1; i < len(docs); i++ {
		if docs[i-1].Env() >= docs[i].Env() {
			t.Errorf("Documented() not sorted at %s", docs[i].Env())
		}
		if docs[i].Hidden() {
			t.Errorf("Documented() leaked hidden row %s", docs[i].Env())
		}
	}
	if len(docs) == 0 {
		t.Fatal("no documented rows")
	}
}

// TestKnownCoversFamilies checks name validation for exact rows and
// prefix-family members, and rejects unregistered names.
func TestKnownCoversFamilies(t *testing.T) {
	known := []string{
		"CRITERIA_HOME",
		"CRITERIA_ADAPTER_NAME",
		"CRITERIA_REMOTE_TLS_CERT",
		"CRITERIA_APPROVAL_MY_NODE",
		"CRITERIA_SIGNAL_signal_a",
		"CRITERIA_REMOTE_FUTURE_KNOB",
		"CRITERIA_ADAPTER_OTHER_BINARY",
	}
	for _, name := range known {
		if !Known(name) {
			t.Errorf("Known(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"CRITERIA_NOT_A_VAR", "CRITERIA_", "OTHER_CRITERIA_X"} {
		if Known(name) {
			t.Errorf("Known(%q) = true, want false", name)
		}
	}
}

// TestLookupIsTheAuditPoint verifies that name-based reads of registered
// variables reach the environment and that unregistered names panic.
func TestLookupIsTheAuditPoint(t *testing.T) {
	t.Setenv("CRITERIA_HOME", "/tmp/criteria-home")
	if got := Lookup("CRITERIA_HOME"); got != "/tmp/criteria-home" {
		t.Errorf("Lookup(CRITERIA_HOME) = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("Lookup of an unregistered name did not panic")
		}
	}()
	Lookup("CRITERIA_NOT_A_VAR")
}

func TestAccessors(t *testing.T) {
	t.Setenv("CRITERIA_NAME", "relay-1")
	t.Setenv("CRITERIA_WORKFLOW", "")
	if got := CRITERIAName.String(); got != "relay-1" {
		t.Errorf("String() = %q", got)
	}
	if got := CRITERIAName.StringOr("fallback"); got != "relay-1" {
		t.Errorf("StringOr() = %q, kept unset sentinel", got)
	}
	if got := CRITERIAWorkflow.StringOr("fallback"); got != "fallback" {
		t.Errorf("StringOr() on empty = %q, want fallback", got)
	}
	if got := CRITERIARemoteE2E.Default(); got != "" {
		t.Errorf("Default() = %q, want empty", got)
	}

	approval := CRITERIAApproval
	if !approval.Prefix() {
		t.Fatal("CRITERIA_APPROVAL must be a prefix row")
	}
	if got := approval.FamilyPrefix(); got != "CRITERIA_APPROVAL_" {
		t.Errorf("FamilyPrefix() = %q", got)
	}
	t.Setenv("CRITERIA_APPROVAL_DEPLOY", "approved")
	if got := approval.LookupSuffix("DEPLOY"); got != "approved" {
		t.Errorf("LookupSuffix() = %q", got)
	}
	if got := approval.SuffixName("deploy-1"); got != "CRITERIA_APPROVAL_deploy-1" {
		t.Errorf("SuffixName() = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("family accessor on a non-prefix row did not panic")
		}
	}()
	CRITERIAHome.SuffixName("x")
}

// TestHiddenRows pins the hidden posture of the protocol and harness rows
// so the docs table cannot accidentally advertise them.
func TestHiddenRows(t *testing.T) {
	for _, v := range []Var{CRITERIAPlugin, CRITERIARemoteFamily, CRITERIAAdapterFamily,
		CRITERIARemoteE2E, CRITERIAPeerE2E, CRITERIAContainerTests, CRITERIAConformanceAdapter,
		CRITERIAConformanceAdapterName, CRITERIARemoteStepStartedFile} {
		if !v.Hidden() {
			t.Errorf("%s must stay hidden from the docs table", v.Env())
		}
	}
	if !CRITERIARemoteFamily.Prefix() || !CRITERIAAdapterFamily.Prefix() {
		t.Error("scrub families must be prefix rows")
	}
	if CRITERIARemoteFamily.Env() != "CRITERIA_REMOTE" || CRITERIARemoteFamily.FamilyPrefix() != "CRITERIA_REMOTE_" {
		t.Errorf("remote family row mismatch: %q / %q", CRITERIARemoteFamily.Env(), CRITERIARemoteFamily.FamilyPrefix())
	}
}

// TestNoRawEnvReadsOutsideRegistry is the repo-wide sweep: every
// os.Getenv/os.LookupEnv of a CRITERIA_* literal must live inside this
// package, so the generated docs table cannot drift from real reads.
//
// TestNoIdentifierEnvReadsOutsideRegistry below extends this to
// identifier-shaped arguments; keep the walk rules in sync.
func TestNoRawEnvReadsOutsideRegistry(t *testing.T) {
	root := "../.."
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "testdata" || name == "testfixtures" || name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(filepath.Base(path), "envreg") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if (strings.Contains(line, `Getenv(`) || strings.Contains(line, `LookupEnv(`)) &&
				strings.Contains(line, `"CRITERIA_`) {
				return fmt.Errorf("%s:%d: raw CRITERIA_* env read outside internal/envreg: %s",
					path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// identifierEnvReadAllowlist records the only os.Getenv/os.LookupEnv call
// sites in non-test production files whose argument is an identifier or
// selector rather than a string literal. A dynamic argument means the
// variable name arrives as data, which is exactly how a registry-backed
// alias (for example os.Getenv(peer.EnvLogLevel)) reintroduces an unaudited
// CRITERIA_* read; anything not listed here fails the sweep. Entries are
// keyed "<repo path>:<argument expr>" and carry why the read is legitimate.
var identifierEnvReadAllowlist = map[string]string{
	// XDG base-directory roots (XDG_CONFIG_HOME, XDG_CACHE_HOME, ...): plain
	// Unix variables, never CRITERIA_*, and the variable walks a small list.
	"internal/dirs/dirs.go:envVar": "XDG_* base-directory roots",
	// Workflow-supplied env-origin secret: the ref is operator data, so the
	// name cannot be a literal. Values are never logged.
	"internal/adapter/secrets/provider_env.go:ref.Ref": "workflow env-secret reference",
}

// sweepDynamicEnvReads walks the repo and reports every os.Getenv or
// os.LookupEnv in a non-test production file whose argument is an
// identifier/selector. Shared by the literal and dynamic sweeps so the walk
// rules stay in sync; repoPath must be trimmed of the "../" walk prefix.
func sweepDynamicEnvReads(data []byte, repoPath string) []string {
	var violations []string
	for i, line := range strings.Split(string(data), "\n") {
		arg, ok := envReadArg(line)
		if !ok {
			continue
		}
		if strings.HasPrefix(arg, `"`) || strings.HasPrefix(arg, "`") {
			continue
		}
		key := repoPath + ":" + arg
		if _, allowed := identifierEnvReadAllowlist[key]; allowed {
			continue
		}
		violations = append(violations,
			fmt.Sprintf("%s:%d: dynamic env read outside internal/envreg (%q): %s", repoPath, i+1, arg, strings.TrimSpace(line)))
	}
	return violations
}

// envReadArg extracts the single argument expression of an
// os.Getenv/os.LookupEnv call whose opening parenthesis shares the line.
// It reports ok=false for calls whose argument is incomplete (a call that
// wraps lines), so a split call fails the sweep instead of hiding.
func envReadArg(line string) (string, bool) {
	idx := strings.Index(line, `Getenv(`)
	if idx < 0 {
		idx = strings.Index(line, `LookupEnv(`)
	}
	if idx < 0 {
		return "", false
	}
	open := strings.Index(line[idx:], "(") + idx
	depth := 0
	for j := open; j < len(line); j++ {
		switch line[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(line[open+1 : j]), true
			}
		}
	}
	return "", false
}

// TestNoIdentifierEnvReadsOutsideRegistry fails when a non-test production
// file reads the environment with a dynamic name — the alias-based raw read
// class the literal sweep cannot see.
func TestNoIdentifierEnvReadsOutsideRegistry(t *testing.T) {
	root := "../.."
	var violations []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "testdata" || name == "testfixtures" || name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(filepath.Base(path), "envreg") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		repoPath := strings.TrimPrefix(path, "../../")
		violations = append(violations, sweepDynamicEnvReads(data, repoPath)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("dynamic env reads outside internal/envreg:\n%s", strings.Join(violations, "\n"))
	}
}

// TestGeneratedDocsMatchRegistry drift-checks the AUTO:ENVREG block in
// docs/env-vars.md against the registry's documented rows.
func TestGeneratedDocsMatchRegistry(t *testing.T) {
	const docsPath = "../../docs/env-vars.md"
	raw, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	const begin = "<!-- BEGIN AUTO:ENVREG -->"
	const end = "<!-- END AUTO:ENVREG -->"
	start := strings.Index(doc, begin)
	finish := strings.Index(doc, end)
	if start < 0 || finish < 0 || finish < start {
		t.Fatalf("docs/env-vars.md missing %s/%s markers", begin, end)
	}
	body := doc[start+len(begin) : finish]
	want := renderDocTable()
	if strings.TrimSpace(body) != strings.TrimSpace(want) {
		t.Fatalf("docs/env-vars.md AUTO:ENVREG block drifted from the registry; run the renderer to regenerate.\n--- doc ---\n%s\n--- registry ---\n%s",
			strings.TrimSpace(body), strings.TrimSpace(want))
	}
	if strings.Count(doc, begin) != 1 || strings.Count(doc, end) != 1 {
		t.Fatalf("docs/env-vars.md must contain exactly one %s block", begin)
	}
}

// TestRenderDocTable regenerates the AUTO:ENVREG block in docs/env-vars.md
// when CRITERIA_REGEN_DOCS=1; without it the test skips and only
// TestGeneratedDocsMatchRegistry guards the drift.
func TestRenderDocTable(t *testing.T) {
	if os.Getenv("CRITERIA_REGEN_DOCS") != "1" {
		t.Skip("set CRITERIA_REGEN_DOCS=1 to regenerate docs/env-vars.md")
	}
	const docsPath = "../../docs/env-vars.md"
	raw, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	const begin = "<!-- BEGIN AUTO:ENVREG -->"
	const end = "<!-- END AUTO:ENVREG -->"
	start := strings.Index(doc, begin)
	finish := strings.Index(doc, end)
	if start < 0 || finish < 0 || finish < start {
		t.Fatalf("docs/env-vars.md missing %s/%s markers", begin, end)
	}
	out := doc[:start+len(begin)] + "\n" + renderDocTable() + doc[finish:]
	if err := os.WriteFile(docsPath, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// renderDocTable renders the operator-facing table from Documented(); it
// lives here so the renderer cannot drift from the drift check.
func renderDocTable() string {
	var b strings.Builder
	b.WriteString("| Variable | Kind | Default | Consumer | Description |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, v := range Documented() {
		def := v.Default()
		if def == "" {
			def = "(unset)"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
			v.Env(), v.Kind(), def, v.Scope(), v.Doc())
	}
	return b.String()
}
