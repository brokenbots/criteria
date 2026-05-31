package langserver

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"go.lsp.dev/protocol"

	"github.com/brokenbots/criteria/workflow"
)

func (s *server) handleDocumentSymbol(params *protocol.DocumentSymbolParams) []protocol.DocumentSymbol {
	dir := filepath.Dir(uriToPath(params.TextDocument.URI))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var symbols []protocol.DocumentSymbol
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".chcl" && ext != ".hcl" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		fileSyms, err := fileSymbols(path)
		if err != nil {
			continue
		}
		symbols = append(symbols, fileSyms...)
	}

	// Sort by position for stable output.
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Range.Start.Line != symbols[j].Range.Start.Line {
			return symbols[i].Range.Start.Line < symbols[j].Range.Start.Line
		}
		return symbols[i].Range.Start.Character < symbols[j].Range.Start.Character
	})

	return symbols
}

func fileSymbols(path string) ([]protocol.DocumentSymbol, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	file, diags := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() || file == nil {
		return nil, fmt.Errorf("parse error")
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("not hclsyntax body")
	}

	var symbols []protocol.DocumentSymbol
	for _, block := range body.Blocks {
		if len(block.Labels) == 0 {
			continue
		}
		name := block.Labels[0]
		var kind protocol.SymbolKind
		var detail string

		switch block.Type {
		case "step":
			kind = protocol.SymbolKindFunction
		case "state":
			kind = protocol.SymbolKindEnum
		case "adapter":
			if len(block.Labels) >= 2 {
				name = block.Labels[0] + "." + block.Labels[1]
			}
			kind = protocol.SymbolKindClass
		case "switch":
			kind = protocol.SymbolKindInterface
		case "variable":
			kind = protocol.SymbolKindVariable
		case "local":
			kind = protocol.SymbolKindConstant
		case "data":
			if len(block.Labels) >= 2 {
				name = block.Labels[0] + "." + block.Labels[1]
			}
			kind = protocol.SymbolKindObject
		case "output":
			kind = protocol.SymbolKindProperty
		case "wait":
			kind = protocol.SymbolKindEvent
		case "approval":
			kind = protocol.SymbolKindEvent
		case "subworkflow":
			kind = protocol.SymbolKindModule
		case "environment":
			if len(block.Labels) >= 2 {
				name = block.Labels[0] + "." + block.Labels[1]
			}
			kind = protocol.SymbolKindNamespace
		default:
			continue
		}

		rng := block.DefRange()
		symbols = append(symbols, protocol.DocumentSymbol{
			Name:           name,
			Detail:         detail,
			Kind:           kind,
			Range:          hclRangeToProtocol(rng),
			SelectionRange: hclRangeToProtocol(rng),
		})
	}

	return symbols, nil
}

func hclRangeToProtocol(r hcl.Range) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{
			Line:      uint32(r.Start.Line - 1),
			Character: uint32(r.Start.Column - 1),
		},
		End: protocol.Position{
			Line:      uint32(r.End.Line - 1),
			Character: uint32(r.End.Column - 1),
		},
	}
}

// buildSymbolsFromSpec builds document symbols from a parsed workflow.Spec.
// This is used by tests to avoid filesystem access.
func buildSymbolsFromSpec(spec *workflow.Spec) []protocol.DocumentSymbol {
	var symbols []protocol.DocumentSymbol
	// Note: Spec does not carry per-block source ranges, so this function
	// is not used for real LSP responses; fileSymbols is used instead.
	_ = spec
	return symbols
}
