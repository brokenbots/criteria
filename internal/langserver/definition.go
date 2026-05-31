package langserver

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"go.lsp.dev/protocol"
)

// symbolIndex maps (kind, name) -> location in the workflow directory.
type symbolIndex map[string]protocol.Location

func (idx symbolIndex) key(kind, name string) string {
	return kind + ":" + name
}

func (idx symbolIndex) add(kind, name string, loc protocol.Location) {
	idx[idx.key(kind, name)] = loc
}

func (idx symbolIndex) get(kind, name string) (protocol.Location, bool) {
	loc, ok := idx[idx.key(kind, name)]
	return loc, ok
}

func buildIndex(dir string) (symbolIndex, error) {
	idx := make(symbolIndex)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".chcl" && ext != ".hcl" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		file, diags := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() || file == nil {
			continue
		}
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}

		for _, block := range body.Blocks {
			if len(block.Labels) == 0 {
				continue
			}
			loc := protocol.Location{
				URI:   pathToURI(path),
				Range: hclRangeToProtocol(block.DefRange()),
			}
			switch block.Type {
			case "step":
				idx.add("step", block.Labels[0], loc)
			case "state":
				idx.add("state", block.Labels[0], loc)
			case "adapter":
				if len(block.Labels) >= 2 {
					idx.add("adapter", block.Labels[0]+"."+block.Labels[1], loc)
				}
			case "switch":
				idx.add("switch", block.Labels[0], loc)
			case "variable":
				idx.add("variable", block.Labels[0], loc)
			case "local":
				idx.add("local", block.Labels[0], loc)
			case "data":
				if len(block.Labels) >= 2 {
					idx.add("data", block.Labels[0]+"."+block.Labels[1], loc)
				}
			case "subworkflow":
				idx.add("subworkflow", block.Labels[0], loc)
			case "wait":
				idx.add("wait", block.Labels[0], loc)
			case "approval":
				idx.add("approval", block.Labels[0], loc)
			}
		}
	}

	return idx, nil
}

var traversalRegex = regexp.MustCompile(`\b(step|state|adapter|switch|variable|local|data|subworkflow|wait|approval|steps|var)\s*\.\s*([a-zA-Z_][a-zA-Z0-9_-]*(?:\s*\.\s*[a-zA-Z_][a-zA-Z0-9_-]*)?)`)

func (s *server) handleDefinition(params *protocol.DefinitionParams) []protocol.LocationLink {
	docPath := uriToPath(params.TextDocument.URI)
	dir := filepath.Dir(docPath)

	idx, err := buildIndex(dir)
	if err != nil {
		return nil
	}

	line, err := readLine(docPath, int(params.Position.Line)+1)
	if err != nil {
		return nil
	}

	col := int(params.Position.Character) + 1 // 1-based
	kind, name := extractTraversalAt(line, col)
	if kind == "" {
		return nil
	}

	loc, ok := idx.get(kind, name)
	if !ok {
		return nil
	}

	return []protocol.LocationLink{
		{
			TargetURI:            loc.URI,
			TargetRange:          loc.Range,
			TargetSelectionRange: loc.Range,
		},
	}
}

// extractTraversalAt finds the traversal reference at the given 1-based column
// in an HCL source line.
func extractTraversalAt(line string, col int) (kind, name string) {
	matches := traversalRegex.FindAllStringIndex(line, -1)
	for _, m := range matches {
		start := m[0]
		end := m[1]
		if col >= start+1 && col <= end+1 {
			// col is within this match.
			match := line[start:end]
			parts := traversalRegex.FindStringSubmatch(match)
			if len(parts) < 3 {
				continue
			}
			kind = parts[1]
			name = strings.ReplaceAll(parts[2], " ", "")
			// Normalize aliases.
			switch kind {
			case "var":
				kind = "variable"
			case "steps":
				kind = "step"
				// steps.<name>.* only needs the first segment.
				if dot := strings.Index(name, "."); dot >= 0 {
					name = name[:dot]
				}
			}
			return kind, name
		}
	}
	return "", ""
}

func readLine(path string, lineNum int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if lineNum < 1 || lineNum > len(lines) {
		return "", fmt.Errorf("line %d out of range", lineNum)
	}
	return lines[lineNum-1], nil
}

// resolveDefinition is the testable entrypoint for definition resolution.
func resolveDefinition(idx symbolIndex, line string, col int) *protocol.Location {
	kind, name := extractTraversalAt(line, col)
	if kind == "" {
		return nil
	}
	loc, ok := idx.get(kind, name)
	if !ok {
		return nil
	}
	return &loc
}

// buildTestIndex creates a symbolIndex from a workflow directory for tests.
func buildTestIndex(dir string) (symbolIndex, error) {
	return buildIndex(dir)
}
