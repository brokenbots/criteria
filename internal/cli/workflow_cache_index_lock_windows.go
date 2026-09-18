//go:build windows

package cli

import (
	"fmt"
	"os"
	"sync"
)

// workflowCacheIndexLocks is a best-effort in-process mutex on Windows.
// Cross-process safety via LockFileEx is a known gap on this platform,
// mirroring the OCI adapter cache's Windows lock behavior.
var workflowCacheIndexLocks sync.Map // map[string]*sync.Mutex

// lockWorkflowCacheIndex acquires an in-process lock on the workflow cache
// index lock file and returns a release function.
func lockWorkflowCacheIndex(cacheRoot string) (release func(), err error) {
	lockPath := workflowCacheIndexLockPath(cacheRoot)
	v, _ := workflowCacheIndexLocks.LoadOrStore(lockPath, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()

	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("create workflow cache root: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("open workflow cache index lock file: %w", err)
	}
	return func() {
		_ = f.Close()
		mu.Unlock()
	}, nil
}
