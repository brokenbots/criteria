package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// devBindings holds the set of local-development adapter overrides.
// Keys are "type.name".
var devBindings = make(map[string]devBinding)

type devBinding struct {
	Path string
}

func newAdapterDevCmd() *cobra.Command {
	var as string

	cmd := &cobra.Command{
		Use:   "dev <local-binary-path> [--as <type>.<name>]",
		Short: "Register a local binary as an adapter (skips lockfile and signature checks)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runDev(args[0], as, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&as, "as", "", "Register as type.name (defaults to binary basename)")
	return cmd
}

func runDev(localPath, as string, out io.Writer) error {
	if out == nil {
		out = os.Stderr
	}
	localPath, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("stat %q: %w", localPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory, expected a binary", localPath)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("%q is not executable", localPath)
	}

	key := as
	if key == "" {
		base := filepath.Base(localPath)
		const prefix = "criteria-adapter-"
		base = strings.TrimPrefix(base, prefix)
		key = base
	}

	if !strings.Contains(key, ".") {
		return fmt.Errorf("--as must be <type>.<name> (got %q)", key)
	}

	parts := strings.SplitN(key, ".", 2)
	typ, name := parts[0], parts[1]
	if typ == "" || name == "" {
		return fmt.Errorf("--as must be <type>.<name> (got %q)", key)
	}

	devBindings[key] = devBinding{Path: localPath}
	fmt.Fprintf(out, "dev: registered %s as %s.%s (not yet wired into apply — see WS09)\n", localPath, typ, name)
	return nil
}

func findDevBinding(typ, name string) (string, bool) {
	b, ok := devBindings[typ+"."+name]
	if !ok {
		return "", false
	}
	return b.Path, true
}

// checkDevAllowed returns an error when the workflow verification mode is
// "strict" and a dev binding exists for the adapter.  Callers should pass the
// effective verification mode read from the workflow configuration.
// WS09 follow-up: wire findDevBinding into the adapter loader path so that
// criteria apply resolves dev bindings before falling back to OCI cache.
func checkDevAllowed(verificationMode, typ, name string) error {
	if _, ok := findDevBinding(typ, name); !ok {
		return nil
	}
	if verificationMode == "strict" {
		return fmt.Errorf("dev binding for %s.%s is not allowed when strict verification is enabled", typ, name)
	}
	return nil
}
