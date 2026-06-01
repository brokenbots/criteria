// Package specdata embeds the Criteria language specification and LLM prompt
// pattern files so they can be included in the binary.
package specdata

import _ "embed"

//go:embed docs/LANGUAGE-SPEC.md
var LangSpec string

//go:embed docs/llm/01-linear.md
var LLMPattern01 string

//go:embed docs/llm/02-branching-switch.md
var LLMPattern02 string

//go:embed docs/llm/03-iteration-for-each.md
var LLMPattern03 string

//go:embed docs/llm/04-iteration-parallel.md
var LLMPattern04 string

//go:embed docs/llm/05-subworkflow.md
var LLMPattern05 string

//go:embed docs/llm/06-approval-and-wait.md
var LLMPattern06 string

//go:embed docs/llm/07-shared-variable.md
var LLMPattern07 string

//go:embed docs/llm/08-fileset-template.md
var LLMPattern08 string

var LLMPatterns = []string{
	LLMPattern01, LLMPattern02, LLMPattern03, LLMPattern04,
	LLMPattern05, LLMPattern06, LLMPattern07, LLMPattern08,
}
