package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	specdata "github.com/brokenbots/criteria"
)

func NewSpecCmd() *cobra.Command {
	var withPatterns bool

	cmd := &cobra.Command{
		Use:   "spec",
		Short: "Print the Criteria workflow language specification",
		Long: `Print the Criteria workflow language specification to stdout.

With --with-patterns, also appends the eight LLM prompt-pack pattern files,
producing a complete system prompt for LLM-assisted workflow authoring.

Examples:
  criteria spec                           # print spec only
  criteria spec --with-patterns           # print spec + all patterns
  criteria spec --with-patterns | pbcopy  # copy to clipboard (macOS)
  criteria spec > spec.md                 # write to file`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return printSpec(os.Stdout, withPatterns)
		},
	}

	cmd.Flags().BoolVar(&withPatterns, "with-patterns", false,
		"Append the eight LLM prompt-pack pattern files after the spec")

	return cmd
}

func printSpec(w io.Writer, withPatterns bool) error {
	if _, err := fmt.Fprint(w, specdata.LangSpec); err != nil {
		return err
	}
	if !withPatterns {
		return nil
	}
	for _, pattern := range specdata.LLMPatterns {
		if _, err := fmt.Fprintf(w, "\n\n---\n\n%s", strings.TrimRight(pattern, "\n")); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}
