//go:build !windows

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockWorkflowCacheIndex acquires an exclusive flock on the workflow cache
// index lock file (creating it if necessary) and returns a release function.
// It mirrors the OCI adapter cache's layout lock.
func lockWorkflowCacheIndex(cacheRoot string) (release func(), err error) {
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create workflow cache root: %w", err)
	}
	f, err := os.OpenFile(workflowCacheIndexLockPath(cacheRoot), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open workflow cache index lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock workflow cache index: %w", err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
