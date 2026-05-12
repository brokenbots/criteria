package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BlockDoc describes a top-level block type extracted from schema.go.
type BlockDoc struct {
	Name         string    // HCL block keyword, e.g. "step"
	Labels       []string  // ordered label names, e.g. ["name"] or ["type", "name"]
	Attributes   []AttrDoc // non-label, non-block, non-remain fields
	NestedBlocks []string  // HCL block keywords of nested block types
	SourceLine   int       // line number of the struct type in schema.go
	RemainNote   string    // non-empty when the block's remain field carries documentation
}

// AttrDoc describes a single attribute within a block.
type AttrDoc struct {
	Name        string // HCL attribute name
	Type        string // HCL type string, e.g. "string", "bool", "number", "list(string)"
	Required    bool   // true when tag has no "optional" modifier, or when spec:required annotation is present
	Description string // from field doc comment, or "_(no description)_"
}

// FuncDoc describes a single HCL expression function.
type FuncDoc struct {
	Name        string
	Params      []ParamDoc
	VarParam    *ParamDoc // nil when no variadic param
	ReturnType  string
	SourceLine  int
	SourceFile  string // relative path to the file defining the builder function
	Description string
}

// ParamDoc describes a function parameter.
type ParamDoc struct {
	Name string
	Type string
}

// hasRequiredAnnotation reports whether the comment group contains a
// "// spec:required" line, which marks a semantically compile-time-required
// attribute despite having the hcl "optional" tag.
func hasRequiredAnnotation(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, c := range doc.List {
		// Strip leading "// " or "//" to get the raw comment text.
		line := strings.TrimPrefix(c.Text, "// ")
		line = strings.TrimPrefix(line, "//")
		line = strings.TrimSpace(line)
		if line == "spec:required" {
			return true
		}
	}
	return false
}

// remainNoteText extracts a human-readable note from a remain field's doc
// comment (lines above the field) or line comment (end-of-line). Returns empty
// when neither is present.
func remainNoteText(field *ast.Field) string {
	if field.Doc != nil {
		text := docText(field.Doc)
		if text != "" {
			return text
		}
	}
	if field.Comment != nil {
		text := docText(field.Comment)
		if text != "" {
			return text
		}
	}
	return ""
}

type hclTag struct {
	name string
	kind string // "label", "block", "optional", "remain", "" (required attr)
}

// parseHCLTag parses the raw struct tag literal (including surrounding backticks).
func parseHCLTag(tagValue string) hclTag {
	s := strings.Trim(tagValue, "`")
	const prefix = `hcl:"`
	idx := strings.Index(s, prefix)
	if idx < 0 {
		return hclTag{}
	}
	rest := s[idx+len(prefix):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return hclTag{}
	}
	val := rest[:end]
	parts := strings.SplitN(val, ",", 2)
	if len(parts) == 1 {
		return hclTag{name: parts[0], kind: ""}
	}
	return hclTag{name: parts[0], kind: parts[1]}
}

// underlyingTypeName returns the struct type name from a field's Go type expression,
// stripping pointer indirection and slice wrapping.
func underlyingTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return underlyingTypeName(e.X)
	case *ast.ArrayType:
		return underlyingTypeName(e.Elt)
	default:
		return ""
	}
}

// goTypeToHCLType maps a Go type AST expression to a human-readable HCL type string.
func goTypeToHCLType(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		switch e.Name {
		case "string":
			return "string"
		case "bool":
			return "bool"
		case "int":
			return "number"
		default:
			return e.Name
		}
	case *ast.StarExpr:
		return goTypeToHCLType(e.X)
	case *ast.ArrayType:
		inner := goTypeToHCLType(e.Elt)
		return "list(" + inner + ")"
	case *ast.MapType:
		return "map(string)"
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		if ok {
			return x.Name + "." + e.Sel.Name
		}
		return "unknown"
	default:
		return "unknown"
	}
}

// docText converts an AST comment group to a single description string.
// It stops at the first blank comment line to take only the opening paragraph.
func docText(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	var lines []string
	for _, c := range doc.List {
		line := strings.TrimPrefix(c.Text, "// ")
		line = strings.TrimPrefix(line, "//")
		line = strings.TrimSpace(line)
		if line == "" && len(lines) > 0 {
			break // stop at first blank comment line
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}

// NamespaceDoc describes an evaluation-context namespace binding.
type NamespaceDoc struct {
	Key     string   // top-level key, e.g. "var", "steps", "each"
	SubKeys []string // for "each": the per-iteration field names extracted from WithEachBinding
}

// extractNamespaces parses evalFile and returns the top-level namespace keys
// registered in BuildEvalContextWithOpts, plus the sub-keys for "each" from
// WithEachBinding. The descriptions are hard-curated in render.go; this
// function owns only the key discovery so namespace drift is detectable.
func extractNamespaces(evalFile string) ([]NamespaceDoc, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, evalFile, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", evalFile, err)
	}

	funcDecls := make(map[string]*ast.FuncDecl)
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		funcDecls[fn.Name.Name] = fn
	}

	buildFn, ok := funcDecls["BuildEvalContextWithOpts"]
	if !ok {
		return nil, fmt.Errorf("BuildEvalContextWithOpts not found in %s", evalFile)
	}
	keys, varFound := extractCtxVarKeys(buildFn)
	if !varFound {
		return nil, fmt.Errorf(
			"BuildEvalContextWithOpts: variable 'ctxVars' not found in function body — has the symbol been renamed?")
	}


	var eachSubKeys []string
	if bindFn, ok := funcDecls["WithEachBinding"]; ok {
		eachSubKeys = extractEachMapKeys(bindFn)
	}

	docs := make([]NamespaceDoc, 0, len(keys))
	for _, key := range keys {
		nd := NamespaceDoc{Key: key}
		if key == "each" {
			nd.SubKeys = eachSubKeys
		}
		docs = append(docs, nd)
	}
	return docs, nil
}

// extractCtxVarKeys walks a function body and collects all string literal keys
// assigned to the local variable named "ctxVars", in declaration order.
// It handles both the initial composite literal (`ctxVars := map[...]{...}`)
// and subsequent index assignments (`ctxVars["key"] = ...`).
// The second return value is true when the "ctxVars" identifier was observed
// at all; false means the variable was not found (likely renamed) and the
// caller should return an actionable error.
func extractCtxVarKeys(fn *ast.FuncDecl) ([]string, bool) {
	var keys []string
	seen := make(map[string]bool)
	varFound := false

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			switch e := lhs.(type) {
			case *ast.Ident:
				if e.Name != "ctxVars" || i >= len(assign.Rhs) {
					continue
				}
				varFound = true
				cl, ok := assign.Rhs[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					bl, ok := kv.Key.(*ast.BasicLit)
					if !ok {
						continue
					}
					key, err := strconv.Unquote(bl.Value)
					if err != nil || key == "" || seen[key] {
						continue
					}
					keys = append(keys, key)
					seen[key] = true
				}
			case *ast.IndexExpr:
				ident, ok := e.X.(*ast.Ident)
				if !ok || ident.Name != "ctxVars" {
					continue
				}
				varFound = true
				bl, ok := e.Index.(*ast.BasicLit)
				if !ok {
					continue
				}
				key, err := strconv.Unquote(bl.Value)
				if err != nil || key == "" || seen[key] {
					continue
				}
				keys = append(keys, key)
				seen[key] = true
			}
		}
		return true
	})
	return keys, varFound
}

// extractEachMapKeys finds the map[string]cty.Value literal assigned to
// newVars["each"] inside WithEachBinding and returns its string literal keys.
func extractEachMapKeys(fn *ast.FuncDecl) []string {
	var keys []string
	var found bool

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			ie, ok := lhs.(*ast.IndexExpr)
			if !ok {
				continue
			}
			bl, ok := ie.Index.(*ast.BasicLit)
			if !ok {
				continue
			}
			idxKey, err := strconv.Unquote(bl.Value)
			if err != nil || idxKey != "each" || i >= len(assign.Rhs) {
				continue
			}
			// Walk the RHS to find the first map[...] composite literal.
			ast.Inspect(assign.Rhs[i], func(n ast.Node) bool {
				if found {
					return false
				}
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if _, ok := cl.Type.(*ast.MapType); !ok {
					return true
				}
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					keyLit, ok := kv.Key.(*ast.BasicLit)
					if !ok {
						continue
					}
					k, err := strconv.Unquote(keyLit.Value)
					if err == nil && k != "" {
						keys = append(keys, k)
					}
				}
				found = true
				return false
			})
		}
		return true
	})
	return keys
}

type structInfo struct {
	st   *ast.StructType
	line int
	doc  *ast.CommentGroup
}

// parseStructs parses filename and returns a map of type name → structInfo.
func parseStructs(filename string) (map[string]structInfo, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}
	structs := make(map[string]structInfo)
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			doc := genDecl.Doc
			if ts.Doc != nil {
				doc = ts.Doc
			}
			structs[ts.Name.Name] = structInfo{
				st:   st,
				line: fset.Position(ts.Pos()).Line,
				doc:  doc,
			}
		}
	}
	return structs, nil
}

// extractBlockFromStruct builds a BlockDoc from a struct type.
func extractBlockFromStruct(hclName string, si structInfo, structs map[string]structInfo) BlockDoc {
	bd := BlockDoc{
		Name:       hclName,
		SourceLine: si.line,
	}
	for _, field := range si.st.Fields.List {
		if field.Tag == nil {
			continue
		}
		tag := parseHCLTag(field.Tag.Value)
		switch tag.kind {
		case "label":
			bd.Labels = append(bd.Labels, tag.name)
		case "block":
			if tag.name != "" {
				bd.NestedBlocks = append(bd.NestedBlocks, tag.name)
			}
		case "remain":
			// Extract any documentation on the remain field as a note for the spec.
			if note := remainNoteText(field); note != "" && bd.RemainNote == "" {
				bd.RemainNote = note
			}
		case "", "attr", "optional":
			if tag.name == "" {
				continue // no valid hcl attribute name
			}
			required := tag.kind == "" || tag.kind == "attr"
			// A field annotated with "// spec:required" is treated as required
			// regardless of the HCL tag, because compile.go enforces it even
			// though HCL-level parsing accepts absence.
			if !required && hasRequiredAnnotation(field.Doc) {
				required = true
			}
			desc := docText(field.Doc)
			if desc == "" {
				desc = "_(no description)_"
			}
			hclType := goTypeToHCLType(field.Type)
			bd.Attributes = append(bd.Attributes, AttrDoc{
				Name:        tag.name,
				Type:        hclType,
				Required:    required,
				Description: desc,
			})
		}
	}
	return bd
}

// buildBlockTypeMap returns a map from HCL block name to the underlying Go struct
// type name, by scanning all struct types for fields with "block" hcl tags.
// When the same HCL name is referenced in multiple struct types, the first
// occurrence (in non-deterministic map order) wins; in practice each name maps
// to a single struct type across the schema.
func buildBlockTypeMap(structs map[string]structInfo) map[string]string {
	m := make(map[string]string)
	for _, si := range structs {
		for _, field := range si.st.Fields.List {
			if field.Tag == nil {
				continue
			}
			tag := parseHCLTag(field.Tag.Value)
			if tag.kind != "block" || tag.name == "" {
				continue
			}
			if _, exists := m[tag.name]; !exists {
				m[tag.name] = underlyingTypeName(field.Type)
			}
		}
	}
	return m
}

// extractBlocks reads schemaFile and returns one BlockDoc per block type,
// starting from the root Spec struct and following nested blocks transitively.
// Top-level blocks (direct children of Spec) come first; nested block types
// are appended in BFS order so links from parent sections can resolve.
func extractBlocks(schemaFile string) ([]BlockDoc, error) {
	structs, err := parseStructs(schemaFile)
	if err != nil {
		return nil, err
	}

	specInfo, ok := structs["Spec"]
	if !ok {
		return nil, fmt.Errorf("Spec struct not found in %s", schemaFile)
	}

	blockTypeOf := buildBlockTypeMap(structs)
	seen := make(map[string]bool)
	var queue []BlockDoc

	// Seed the queue with top-level blocks declared in the Spec struct.
	for _, field := range specInfo.st.Fields.List {
		if field.Tag == nil {
			continue
		}
		tag := parseHCLTag(field.Tag.Value)
		if tag.kind != "block" || tag.name == "" {
			continue
		}
		typeName := underlyingTypeName(field.Type)
		si, ok := structs[typeName]
		if !ok {
			continue
		}
		if seen[tag.name] {
			continue
		}
		seen[tag.name] = true
		queue = append(queue, extractBlockFromStruct(tag.name, si, structs))
	}

	// BFS: extract nested block structs and append them after all top-level blocks.
	var result []BlockDoc
	for len(queue) > 0 {
		bd := queue[0]
		queue = queue[1:]
		result = append(result, bd)
		for _, nestedName := range bd.NestedBlocks {
			if seen[nestedName] {
				continue
			}
			typeName, ok := blockTypeOf[nestedName]
			if !ok {
				continue
			}
			nsi, ok := structs[typeName]
			if !ok {
				continue
			}
			seen[nestedName] = true
			queue = append(queue, extractBlockFromStruct(nestedName, nsi, structs))
		}
	}

	return result, nil
}

// extractFunctions reads the workflow functions package directory and returns
// one FuncDoc per entry registered by workflowFunctions. It handles two
// registration patterns:
//
//  1. A single map literal returned directly: return map[string]function.Function{...}
//  2. An incremental build with register helpers:
//     out := map[string]function.Function{...}
//     for k, v := range registerXxx() { out[k] = v }
//
// All non-test .go files in the same directory as functionsFile are parsed so
// that builder function declarations in sibling files are discoverable.
func extractFunctions(functionsFile string) ([]FuncDoc, error) {
	fset := token.NewFileSet()

	// Parse all non-test .go files in the package directory so that builder
	// functions defined in sibling files (e.g. eval_functions_hash.go) are
	// reachable when resolving map entries.
	dir := filepath.Dir(functionsFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}

	funcDecls := make(map[string]*ast.FuncDecl)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s: %w", name, parseErr)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			funcDecls[fn.Name.Name] = fn
		}
	}

	// Find workflowFunctions (may live in any parsed file).
	wfFn, ok := funcDecls["workflowFunctions"]
	if !ok {
		return nil, fmt.Errorf("workflowFunctions not found in %s", functionsFile)
	}

	// collectFromMapLit extracts FuncDoc entries from a composite map literal.
	collectFromMapLit := func(mapLit *ast.CompositeLit) []FuncDoc {
		var docs []FuncDoc
		for _, elt := range mapLit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			keyLit, ok := kv.Key.(*ast.BasicLit)
			if !ok {
				continue
			}
			funcName, err := strconv.Unquote(keyLit.Value)
			if err != nil {
				continue
			}
			// Value is a call to a builder function: builderName(opts) or builderName().
			call, ok := kv.Value.(*ast.CallExpr)
			if !ok {
				continue
			}
			builderName := callFuncName(call.Fun)
			builderDecl, ok := funcDecls[builderName]
			if !ok {
				continue
			}
			doc := extractFuncDoc(builderDecl, fset)
			doc.Name = funcName
			docs = append(docs, doc)
		}
		return docs
	}

	// Pattern 1: return map[string]function.Function{...}
	var mapLit *ast.CompositeLit
	ast.Inspect(wfFn.Body, func(n ast.Node) bool {
		if mapLit != nil {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if len(ret.Results) != 1 {
			return true
		}
		cl, ok := ret.Results[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		mapLit = cl
		return false
	})
	if mapLit != nil {
		return collectFromMapLit(mapLit), nil
	}

	// Pattern 2: out := map[string]function.Function{...}
	//            for k, v := range registerXxx() { out[k] = v }
	var initMap *ast.CompositeLit
	var registerCalls []string
	for _, stmt := range wfFn.Body.List {
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			if len(s.Rhs) == 1 {
				if cl, ok := s.Rhs[0].(*ast.CompositeLit); ok {
					initMap = cl
				}
			}
		case *ast.RangeStmt:
			call, ok := s.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			name := callFuncName(call.Fun)
			if name != "" {
				registerCalls = append(registerCalls, name)
			}
		}
	}
	if initMap == nil {
		return nil, fmt.Errorf("workflowFunctions map literal not found in %s", functionsFile)
	}

	docs := collectFromMapLit(initMap)
	// Follow each registerXxx() call to extract entries from its returned map.
	for _, regName := range registerCalls {
		regDecl, ok := funcDecls[regName]
		if !ok {
			continue
		}
		var regMapLit *ast.CompositeLit
		ast.Inspect(regDecl.Body, func(n ast.Node) bool {
			if regMapLit != nil {
				return false
			}
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			if len(ret.Results) != 1 {
				return true
			}
			cl, ok := ret.Results[0].(*ast.CompositeLit)
			if !ok {
				return true
			}
			regMapLit = cl
			return false
		})
		if regMapLit != nil {
			docs = append(docs, collectFromMapLit(regMapLit)...)
		}
	}
	return docs, nil
}

// callFuncName extracts the identifier name from a call expression's function.
func callFuncName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		ident, ok := e.X.(*ast.Ident)
		if ok {
			return ident.Name + "." + e.Sel.Name
		}
		return e.Sel.Name
	default:
		return ""
	}
}

// extractFuncDoc builds a FuncDoc from a builder function declaration.
func extractFuncDoc(decl *ast.FuncDecl, fset *token.FileSet) FuncDoc {
	pos := fset.Position(decl.Pos())
	doc := FuncDoc{
		SourceLine: pos.Line,
		SourceFile: pos.Filename,
	}

	// Description: first paragraph of the doc comment, minus "funcName implements" prefix.
	if decl.Doc != nil {
		text := docText(decl.Doc)
		prefix := decl.Name.Name + " implements "
		if strings.HasPrefix(text, prefix) {
			text = text[len(prefix):]
		}
		// Take only the first sentence.
		if idx := strings.Index(text, "."); idx >= 0 {
			text = text[:idx+1]
		}
		if text != "" {
			doc.Description = text
		}
	}
	if doc.Description == "" {
		doc.Description = "_(no description)_"
	}

	// Find function.New(&function.Spec{...}) inside the builder body.
	var specLit *ast.CompositeLit
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		if specLit != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "New" {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		unary, ok := call.Args[0].(*ast.UnaryExpr)
		if !ok || unary.Op != token.AND {
			return true
		}
		cl, ok := unary.X.(*ast.CompositeLit)
		if !ok {
			return true
		}
		specLit = cl
		return false
	})
	if specLit == nil {
		return doc
	}

	for _, elt := range specLit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Params":
			doc.Params = extractParams(kv.Value)
		case "VarParam":
			doc.VarParam = extractVarParam(kv.Value)
		case "Type":
			doc.ReturnType = extractReturnType(kv.Value)
		}
	}
	return doc
}

// extractParams parses a []function.Parameter{...} composite literal.
func extractParams(expr ast.Expr) []ParamDoc {
	cl, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	var params []ParamDoc
	for _, elt := range cl.Elts {
		cl2, ok := elt.(*ast.CompositeLit)
		if !ok {
			continue
		}
		var p ParamDoc
		for _, f := range cl2.Elts {
			kv, ok := f.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Name":
				if bl, ok := kv.Value.(*ast.BasicLit); ok {
					p.Name, _ = strconv.Unquote(bl.Value)
				}
			case "Type":
				p.Type = extractCtyType(kv.Value)
			}
		}
		params = append(params, p)
	}
	return params
}

// extractVarParam parses a &function.Parameter{...} expression.
func extractVarParam(expr ast.Expr) *ParamDoc {
	unary, ok := expr.(*ast.UnaryExpr)
	if !ok {
		return nil
	}
	cl, ok := unary.X.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	var p ParamDoc
	for _, f := range cl.Elts {
		kv, ok := f.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Name":
			if bl, ok := kv.Value.(*ast.BasicLit); ok {
				p.Name, _ = strconv.Unquote(bl.Value)
			}
		case "Type":
			p.Type = extractCtyType(kv.Value)
		}
	}
	return &p
}

// extractReturnType parses a function.StaticReturnType(cty.X) call expression.
func extractReturnType(expr ast.Expr) string {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return "unknown"
	}
	if len(call.Args) == 1 {
		return extractCtyType(call.Args[0])
	}
	return "unknown"
}

// extractCtyType maps a cty type expression to a type name string.
// It handles simple selector expressions (cty.String, cty.Bool, etc.) and
// parameterised call expressions (cty.List(cty.String), cty.Set(X), cty.Map(X)).
func extractCtyType(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		if !ok {
			return "unknown"
		}
		if x.Name == "cty" {
			switch e.Sel.Name {
			case "String":
				return "string"
			case "Bool":
				return "bool"
			case "Number":
				return "number"
			case "DynamicPseudoType":
				return "any"
			}
		}
		return x.Name + "." + e.Sel.Name
	case *ast.CallExpr:
		fun, ok := e.Fun.(*ast.SelectorExpr)
		if !ok || len(e.Args) == 0 {
			return "unknown"
		}
		x, ok := fun.X.(*ast.Ident)
		if !ok || x.Name != "cty" {
			return "unknown"
		}
		switch fun.Sel.Name {
		case "List", "Set", "Map":
			inner := extractCtyType(e.Args[0])
			return strings.ToLower(fun.Sel.Name) + "(" + inner + ")"
		}
		return "unknown"
	}
	return "unknown"
}
