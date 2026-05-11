package main

import (
	"bytes"
	"fmt"
	"strings"
)

// renderBlocks renders the blocks section as a markdown string.
// schemaRelPath is used in source links, e.g. "workflow/schema.go".
func renderBlocks(blocks []BlockDoc, schemaRelPath string) string {
	// Build an anchor map so nested block references can be linked to their own sections.
	anchorOf := make(map[string]string, len(blocks))
	for _, b := range blocks {
		anchorOf[b.Name] = blockAnchor(b)
	}

	var buf bytes.Buffer
	for _, b := range blocks {
		// Build the block signature line.
		labelStr := blockLabelStr(b)

		fmt.Fprintf(&buf, "### `%s %s`\n\n", b.Name, labelStr)
		fmt.Fprintf(&buf, "- **Source:** [`%s:%d`](../%s#L%d)\n",
			schemaRelPath, b.SourceLine, schemaRelPath, b.SourceLine)

		if len(b.Labels) > 0 {
			var labelDesc string
			if len(b.Labels) == 1 {
				labelDesc = fmt.Sprintf("`%s`", b.Labels[0])
			} else {
				parts := make([]string, len(b.Labels))
				for i, l := range b.Labels {
					parts[i] = fmt.Sprintf("`%s`", l)
				}
				labelDesc = strings.Join(parts, " ")
			}
			fmt.Fprintf(&buf, "- **Labels:** %s\n", labelDesc)
		}

		if len(b.Attributes) > 0 {
			fmt.Fprintf(&buf, "- **Attributes:**\n\n")
			fmt.Fprintf(&buf, "| Attribute | Type | Required | Description |\n")
			fmt.Fprintf(&buf, "|---|---|---|---|\n")
			for _, a := range b.Attributes {
				req := "no"
				if a.Required {
					req = "yes"
				}
				fmt.Fprintf(&buf, "| `%s` | %s | %s | %s |\n", a.Name, a.Type, req, a.Description)
			}
			fmt.Fprintf(&buf, "\n")
		}

		if len(b.NestedBlocks) > 0 {
			parts := make([]string, len(b.NestedBlocks))
			for i, nb := range b.NestedBlocks {
				if anchor, ok := anchorOf[nb]; ok {
					parts[i] = fmt.Sprintf("[`%s`](%s)", nb, anchor)
				} else {
					parts[i] = fmt.Sprintf("`%s`", nb)
				}
			}
			fmt.Fprintf(&buf, "- **Nested blocks:** %s\n", strings.Join(parts, ", "))
		}

		fmt.Fprintf(&buf, "\n")
	}
	return strings.TrimRight(buf.String(), "\n")
}

// blockLabelStr returns the label+body portion of a block's heading signature,
// e.g. `"name" { ... }` for one label or `"type" "name" { ... }` for two.
func blockLabelStr(b BlockDoc) string {
	switch len(b.Labels) {
	case 0:
		return "{ ... }"
	case 1:
		return fmt.Sprintf(`"%s" { ... }`, b.Labels[0])
	default:
		parts := make([]string, len(b.Labels))
		for i, l := range b.Labels {
			parts[i] = fmt.Sprintf(`"%s"`, l)
		}
		return strings.Join(parts, " ") + " { ... }"
	}
}

// blockAnchor computes the GitHub Markdown heading anchor for a BlockDoc.
//
// GitHub's slugifier applied to `` ### `name labelStr` `` works as follows:
//  1. Extract the text content of the rendered heading (backtick code span
//     content: `name labelStr`).
//  2. Lowercase the text.
//  3. Drop every character that is not alphanumeric, a hyphen, an underscore,
//     or a space.
//  4. Replace spaces with hyphens (consecutive hyphens are not collapsed).
//
// Examples:
//
//	`config { ... }`     → #config---
//	`outcome "name" { ... }` → #outcome-name---
//	`adapter "type" "name" { ... }` → #adapter-type-name---
func blockAnchor(b BlockDoc) string {
	text := strings.ToLower(b.Name + " " + blockLabelStr(b))
	var sb strings.Builder
	for _, r := range text {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			sb.WriteRune(r)
		case r == ' ':
			sb.WriteRune('-')
		}
	}
	return "#" + sb.String()
}

// renderFunctions renders the functions section as a markdown table.
// functionsRelPath is used in source links, e.g. "workflow/eval_functions.go".
func renderFunctions(funcs []FuncDoc, functionsRelPath string) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "| Function | Signature | Returns | Source | Description |\n")
	fmt.Fprintf(&buf, "|---|---|---|---|---|\n")
	for _, fn := range funcs {
		var paramParts []string
		for _, p := range fn.Params {
			paramParts = append(paramParts, fmt.Sprintf("%s: %s", p.Name, p.Type))
		}
		if fn.VarParam != nil {
			paramParts = append(paramParts, fmt.Sprintf("%s...: %s", fn.VarParam.Name, fn.VarParam.Type))
		}
		sig := fmt.Sprintf("%s(%s)", fn.Name, strings.Join(paramParts, ", "))
		source := fmt.Sprintf("[%s:%d](../%s#L%d)",
			functionsRelPath, fn.SourceLine, functionsRelPath, fn.SourceLine)
		fmt.Fprintf(&buf, "| `%s` | `%s` | `%s` | %s | %s |\n",
			fn.Name, sig, fn.ReturnType, source, fn.Description)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// renderNamespaces renders the namespaces section as a markdown table.
// Namespace keys are sourced from the extracted NamespaceDoc slice; descriptions
// are sourced from the curated tables below so they stay readable across releases.
func renderNamespaces(namespaces []NamespaceDoc) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "| Namespace | Available in | Description |\n")
	fmt.Fprintf(&buf, "|---|---|---|\n")
	for _, ns := range namespaces {
		var nsCol string
		if len(ns.SubKeys) > 0 {
			parts := make([]string, len(ns.SubKeys))
			for i, sk := range ns.SubKeys {
				parts[i] = fmt.Sprintf("`%s.%s`", ns.Key, sk)
			}
			nsCol = strings.Join(parts, " / ")
		} else if format, ok := namespaceColumnFormat[ns.Key]; ok {
			nsCol = format
		} else {
			nsCol = fmt.Sprintf("`%s.*`", ns.Key)
		}
		availIn, ok := namespaceAvailableIn[ns.Key]
		if !ok {
			availIn = "_(unknown)_"
		}
		desc, ok := namespaceDescription[ns.Key]
		if !ok {
			desc = "_(no description)_"
		}
		fmt.Fprintf(&buf, "| %s | %s | %s |\n", nsCol, availIn, desc)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// namespaceColumnFormat defines the "Namespace" column text for keys whose
// column is a static pattern rather than a dynamic sub-key list.
var namespaceColumnFormat = map[string]string{
	"var":    "`var.*`",
	"steps":  "`steps.<name>.<key>`",
	"local":  "`local.*`",
	"shared": "`shared.*`",
}

// namespaceAvailableIn gives the "Available in" column text per namespace key.
var namespaceAvailableIn = map[string]string{
	"var":    "all expressions",
	"steps":  "post-completion of `<name>`",
	"each":   "iterating-step expressions only",
	"local":  "all expressions",
	"shared": "all expressions; mutable via `shared_writes`",
}

// namespaceDescription gives the description column text per namespace key.
// Text is hand-curated here; keys are discovered from workflow/eval.go.
var namespaceDescription = map[string]string{
	"var":    "Read-only typed input variables declared with `variable` blocks.",
	"steps":  "Captured outputs from a prior step.",
	"each":   "Per-iteration bindings; see Iteration semantics.",
	"local":  "Compile-time constants declared with `local` blocks.",
	"shared": "Runtime-mutable shared values declared with `shared_variable` blocks.",
}
