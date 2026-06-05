// Command criteria-adapter-shell is the standalone out-of-process shell
// adapter binary. It serves the protocol-v2 shell adapter via the public Go
// SDK. The same logic is also reachable from the main criteria binary via the
// --builtin-shell dispatch, so shell ships with criteria today; this standalone
// binary makes a future extraction to its own repository a move, not a rewrite.
package main

import "github.com/brokenbots/criteria/adapters/shell"

func main() {
	shell.Serve()
}
