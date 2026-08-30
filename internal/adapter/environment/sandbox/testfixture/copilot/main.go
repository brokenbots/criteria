//go:build linux

// copilot is a test helper that runs inside the sandbox shim, applies the
// shim restrictions via ApplyEnv, and then syscall.Exec the real GitHub
// Copilot CLI *native* binary (not the Node.js npm loader). Keeping the
// restrictions in-process before exec means the Copilot process inherits
// rlimits, seccomp, landlock, and namespaces.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

func main() {
	err := sandbox.ApplyEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim error:", err)
		os.Exit(1)
	}
	copilotBin, err := resolveNativeCopilot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "copilot native binary not found:", err)
		os.Exit(1)
	}
	argv := []string{"copilot"}
	if len(os.Args) > 1 {
		argv = append(argv, os.Args[1:]...)
	}
	err = syscall.Exec(copilotBin, argv, os.Environ())
	fmt.Fprintln(os.Stderr, "exec error:", err)
	os.Exit(1)
}

// resolveNativeCopilot locates the platform-specific native Copilot binary
// relative to the npm loader script on PATH. The loader is a Node script that
// spawns the native binary from a sibling package such as
// @github/copilot-linux-arm64/copilot.
func resolveNativeCopilot() (string, error) {
	loader, err := exec.LookPath("copilot")
	if err != nil {
		return "", err
	}
	loader, err = filepath.EvalSymlinks(loader)
	if err != nil {
		return "", err
	}
	baseDir := filepath.Dir(loader)
	arch := runtime.GOARCH
	candidates := []string{
		// Typical global npm layout.
		filepath.Join(baseDir, "node_modules", "@github", "copilot-linux-"+arch, "copilot"),
		filepath.Join(baseDir, "node_modules", "@github", "copilot-linuxmusl-"+arch, "copilot"),
		// If the loader lives inside the @github/copilot package, the
		// platform packages are in its node_modules.
		filepath.Join(baseDir, "..", "copilot-linux-"+arch, "copilot"),
		filepath.Join(baseDir, "..", "copilot-linuxmusl-"+arch, "copilot"),
		filepath.Join(baseDir, "..", "node_modules", "@github", "copilot-linux-"+arch, "copilot"),
		filepath.Join(baseDir, "..", "node_modules", "@github", "copilot-linuxmusl-"+arch, "copilot"),
	}
	for _, cand := range candidates {
		if p, err := filepath.Abs(cand); err == nil {
			cand = p
		}
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no native binary for %s under %s", arch, baseDir)
}
