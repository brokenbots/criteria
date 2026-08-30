//go:build linux

// sandboxshim is a tiny standalone binary used by adapterhost tests as the
// sandbox pre-exec shim. It mirrors the criteria CLI's start-up behavior:
// when CRITERIA_SANDBOX_CONFIG_PATH is set it applies the sandbox profile and
// re-executes the real adapter binary; otherwise it exits with status 125.
// Keeping this logic in a dedicated helper prevents the test binary itself
// from being launched as a go-plugin child, which would cause the plugin
// handshake to collide with TestMain's normal test execution path.
package main

import (
	"fmt"
	"os"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

func main() {
	if ran, err := sandbox.RunIfEnv(); ran {
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandbox shim failed:", err)
			os.Exit(125)
		}
		// RunIfEnv should have replaced this process via syscall.Exec; reaching
		// here is unexpected but safe to treat as success.
		os.Exit(0)
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox shim check failed:", err)
		os.Exit(125)
	}

	fmt.Fprintln(os.Stderr, "CRITERIA_SANDBOX_CONFIG_PATH not set")
	os.Exit(125)
}
