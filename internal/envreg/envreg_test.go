package envreg

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
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

// The sweep analyzer below enforces the "the registry owns every CRITERIA_*
// literal" rule repo-wide. It parses every non-test production Go file
// (testdata included, minus the documented exclusions) and flags every env
// read that bypasses the registry:
//
//   - literal: a string-literal argument containing CRITERIA_ — the classic
//     raw read, including calls whose argument wraps past the end of the
//     line (whole-file parsing sees those; a line-based scan never would);
//   - identifier: a non-string argument, i.e. the variable name arrives as
//     data — exactly how a registry-backed alias (os.Getenv(peer.EnvLogLevel))
//     re-introduces an unaudited read;
//   - function-value: os.Getenv/os.LookupEnv passed as a function value so
//     the callee performs the read away from any argument analysis.
//
// A file that does not parse is reported loudly instead of being skipped.
// The only exceptions live in envReadExcludedFiles and
// identifierEnvReadAllowlist, each carrying its rationale.

// envReadViolation is one sweep finding.
type envReadViolation struct {
	path, line, class, detail string
}

// envReadExcludedFiles is the complete, deliberately documented set of
// files the sweep skips. Any new exclusion must state why the file cannot
// consume the registry.
var envReadExcludedFiles = map[string]string{
	// Standalone smoke fixture module (module criteria-adapter-remote-smoke,
	// own go.mod/go.sum, built GOWORK=off in remote-e2e CI). Go's
	// internal-package visibility rule makes internal/envreg unimportable
	// from that module path, and re-homing the fixture into the root module
	// would drag the root dependency graph into the smoke image. Its raw
	// reads are the peer-host-set CRITERIA_REMOTE_*/CRITERIA_ADAPTER_NAME
	// names this same package documents for the orchestrator manifests.
	"internal/ci/smoke/testdata/criteria-adapter-remote-smoke/main.go": "standalone smoke fixture module",
}

// identifierEnvReadAllowlist records the only os.Getenv/os.LookupEnv call
// sites with a non-string (identifier-shaped) argument the sweep tolerates.
// A dynamic argument means the variable name arrives as data; anything not
// listed here fails the sweep. Entries are keyed "<repo path>:<arg source>"
// with collapsed whitespace and carry why the read is legitimate.
var identifierEnvReadAllowlist = map[string]string{
	// XDG base-directory roots (XDG_CONFIG_HOME, XDG_CACHE_HOME, ...): plain
	// Unix variables, never CRITERIA_*, and the variable walks a small list.
	"internal/dirs/dirs.go:envVar": "XDG_* base-directory roots",
	// Workflow-supplied env-origin secret: the ref is operator data, so the
	// name cannot be a literal. Values are never logged.
	"internal/adapter/secrets/provider_env.go:ref.Ref": "workflow env-secret reference",
	// Test-fixture call-log path (constant PROMPTABLE_CALL_LOG): not a
	// CRITERIA_* name and out of the registry's scope.
	"internal/adapter/conformance/testdata/promptable/main.go:callLogEnv": "fixture call-log path, not CRITERIA_*",
}

// osImportName returns the local name the file binds the os import to (a
// package name different from "os" only when aliased).
func osImportName(file *ast.File) string {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != "os" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "os"
	}
	return "os"
}

// sweepEnvReads returns every os.Getenv/os.LookupEnv finding in one parsed
// file: identifier arguments and function-value passes must be allowlisted;
// string-literal arguments must not contain CRITERIA_. Lines are the AST
// positions, so calls written across lines are attributed correctly.
func sweepEnvReads(path string, src []byte, fset *token.FileSet) []envReadViolation {
	var out []envReadViolation
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return []envReadViolation{{path: path, class: "unparsable", detail: err.Error()}}
	}
	osPkg := osImportName(file)

	var calls []*ast.CallExpr
	var passes []*ast.SelectorExpr
	ast.Inspect(file, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			calls = append(calls, e)
		case *ast.SelectorExpr:
			if id, ok := e.X.(*ast.Ident); ok && id.Name == osPkg &&
				(e.Sel.Name == "Getenv" || e.Sel.Name == "LookupEnv") {
				passes = append(passes, e)
			}
		}
		return true
	})

	callFuns := make(map[ast.Expr]bool, len(calls))
	for _, call := range calls {
		callFuns[call.Fun] = true
	}
	for _, sel := range passes {
		if callFuns[sel] {
			continue
		}
		out = append(out, envReadViolation{path: path, line: strconv.Itoa(fset.Position(sel.Pos()).Line),
			class: "function-value", detail: fmt.Sprintf("os.%s passed as a function value; the read bypasses argument analysis", sel.Sel.Name)})
	}
	for _, call := range calls {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != osPkg || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") {
			continue
		}
		line := strconv.Itoa(fset.Position(call.Pos()).Line)
		if len(call.Args) != 1 {
			out = append(out, envReadViolation{path: path, line: line, class: "malformed",
				detail: fmt.Sprintf("os.%s call with %d arguments; expected exactly one", sel.Sel.Name, len(call.Args))})
			continue
		}
		arg := call.Args[0]
		argText := argSource(fset, arg, src)
		if lit, isLit := arg.(*ast.BasicLit); isLit && lit.Kind == token.STRING {
			if strings.Contains(lit.Value, "CRITERIA_") {
				out = append(out, envReadViolation{path: path, line: line, class: "literal",
					detail: "raw CRITERIA_* env read outside internal/envreg"})
			}
			continue
		}
		if argText == "" {
			out = append(out, envReadViolation{path: path, line: line, class: "malformed",
				detail: "env-read argument source could not be extracted"})
			continue
		}
		key := path + ":" + argText
		if _, allowed := identifierEnvReadAllowlist[key]; allowed {
			continue
		}
		out = append(out, envReadViolation{path: path, line: line, class: "identifier",
			detail: fmt.Sprintf("dynamic env read outside internal/envreg: os.%s(%s)", sel.Sel.Name, argText)})
	}
	return out
}

// argSource renders the argument's source text with whitespace collapsed so
// allowlist keys match how the call is written regardless of line breaks.
func argSource(fset *token.FileSet, expr ast.Expr, src []byte) string {
	lo := fset.Position(expr.Pos()).Offset
	hi := fset.Position(expr.End()).Offset
	if lo < 0 || hi > len(src) || lo >= hi {
		return ""
	}
	return strings.Join(strings.Fields(string(src[lo:hi])), " ")
}

// envReadSweep walks the repo and reports every env-read violation in
// non-test production Go files; testdata directories are scanned like any
// other production directory except the documented fixture exclusions.
func envReadSweep(t *testing.T) []envReadViolation {
	t.Helper()
	var out []envReadViolation
	fset := token.NewFileSet()
	err := filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testfixtures":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			strings.HasPrefix(filepath.Base(path), "envreg") {
			return nil
		}
		repoPath := strings.TrimPrefix(path, "../../")
		if _, excluded := envReadExcludedFiles[repoPath]; excluded {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, sweepEnvReads(repoPath, src, fset)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNoRawEnvReadsOutsideRegistry is the repo-wide sweep: every
// os.Getenv/os.LookupEnv of a CRITERIA_* literal must live inside this
// package, so the generated docs table cannot drift from real reads.
func TestNoRawEnvReadsOutsideRegistry(t *testing.T) {
	var hits []string
	for _, v := range envReadSweep(t) {
		if v.class == "literal" || v.class == "unparsable" || v.class == "malformed" {
			hits = append(hits, fmt.Sprintf("%s:%s: %s: %s", v.path, v.line, v.class, v.detail))
		}
	}
	if len(hits) > 0 {
		t.Fatalf("raw CRITERIA_* env reads outside internal/envreg:\n%s", strings.Join(hits, "\n"))
	}
}

// TestNoIdentifierEnvReadsOutsideRegistry fails when a non-test production
// file reads the environment with a dynamic name or hands os.Getenv /
// os.LookupEnv to a callee as a function value — the alias and delegation
// classes the literal sweep cannot see.
func TestNoIdentifierEnvReadsOutsideRegistry(t *testing.T) {
	var hits []string
	for _, v := range envReadSweep(t) {
		if v.class == "identifier" || v.class == "function-value" || v.class == "unparsable" || v.class == "malformed" {
			hits = append(hits, fmt.Sprintf("%s:%s: %s: %s", v.path, v.line, v.class, v.detail))
		}
	}
	if len(hits) > 0 {
		t.Fatalf("dynamic env reads outside internal/envreg:\n%s", strings.Join(hits, "\n"))
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
	if CRITERIARegenDocs.String() != "1" {
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

// TestEveryDeclaredVarIsRegistered pins the completeness invariant: every
// package-level Var declared in registry.go must appear in the `registry`
// table. All() is built from that table, so a declared-but-unregistered row
// is readable through its typed accessor yet invisible to All(), to the
// docs table, and to the drift test — this test fails on exactly that
// state by parsing the declarations instead of hand-maintaining a second
// list that could drift from them.
func TestEveryDeclaredVarIsRegistered(t *testing.T) {
	src, err := os.ReadFile("registry.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "registry.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]string{} // env name -> declaring identifier
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		isVarRow := false
		if id, ok := spec.Type.(*ast.Ident); ok && id.Name == "Var" {
			isVarRow = true
		} else if spec.Type == nil && len(spec.Values) == 1 {
			composite, ok := spec.Values[0].(*ast.CompositeLit)
			if ok {
				id, ok := composite.Type.(*ast.Ident)
				isVarRow = ok && id.Name == "Var"
			}
		}
		if !isVarRow {
			return true
		}
		envName := declaredRowName(spec)
		if envName == "" {
			t.Errorf("declaration %s is an unregistered or unparseable Var row (no `name` literal found)", spec.Names)
			return true
		}
		if prev, dup := declared[envName]; dup && prev != spec.Names[0].Name {
			t.Errorf("env name %s is declared twice (%s and %s)", envName, prev, spec.Names[0])
		}
		declared[envName] = spec.Names[0].Name
		return true
	})

	registered := map[string]bool{}
	for _, v := range All() {
		registered[v.Env()] = true
	}
	for envName, ident := range declared {
		if !registered[envName] {
			t.Errorf("declared Var %s (%s) is missing from the registry table: readable via its accessor yet invisible to All(), the docs table, and the drift test", ident, envName)
		}
	}
	for envName := range registered {
		if _, ok := declared[envName]; !ok {
			t.Errorf("registered Var %s has no declared row in registry.go", envName)
		}
	}
}

// declaredRowName extracts the `name: "CRITERIA_..."` literal of one Var
// composite-literal declaration.
func declaredRowName(spec *ast.ValueSpec) string {
	if len(spec.Values) == 0 {
		return ""
	}
	if id, ok := spec.Values[0].(*ast.Ident); ok {
		return strings.Trim(id.Name, `"`)
	}
	composite, ok := spec.Values[0].(*ast.CompositeLit)
	if !ok {
		return ""
	}
	for _, elt := range composite.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "name" {
			continue
		}
		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		unquoted, err := strconv.Unquote(lit.Value)
		if err != nil {
			return ""
		}
		return unquoted
	}
	return ""
}
