package langserver

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"go.lsp.dev/protocol"
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

var blockKindMap = map[string]protocol.SymbolKind{
	"step":        protocol.SymbolKindFunction,
	"state":       protocol.SymbolKindEnum,
	"switch":      protocol.SymbolKindInterface,
	"variable":    protocol.SymbolKindVariable,
	"local":       protocol.SymbolKindConstant,
	"output":      protocol.SymbolKindProperty,
	"wait":        protocol.SymbolKindEvent,
	"approval":    protocol.SymbolKindEvent,
	"subworkflow": protocol.SymbolKindModule,
}

func compoundName(labels []string, sep string) string {
	if len(labels) >= 2 {
		return labels[0] + sep + labels[1]
	}
	return labels[0]
}

func blockSymbolInfo(block *hclsyntax.Block) (name string, kind protocol.SymbolKind, ok bool) {
	if len(block.Labels) == 0 {
		return "", 0, false
	}
	name = block.Labels[0]
	switch block.Type {
	case "adapter":
		name = compoundName(block.Labels, ".")
		kind = protocol.SymbolKindClass
	case "data":
		name = compoundName(block.Labels, ".")
		kind = protocol.SymbolKindObject
	case "environment":
		name = compoundName(block.Labels, ".")
		kind = protocol.SymbolKindNamespace
	default:
		kind, ok = blockKindMap[block.Type]
		if !ok {
			return "", 0, false
		}
	}
	return name, kind, true
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

	symbols := make([]protocol.DocumentSymbol, 0, len(body.Blocks))
	for _, block := range body.Blocks {
		name, kind, ok := blockSymbolInfo(block)
		if !ok {
			continue
		}
		rng := block.DefRange()
		symbols = append(symbols, protocol.DocumentSymbol{
			Name:           name,
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
