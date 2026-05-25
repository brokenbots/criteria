//go:build windows

package oci

import (
	"fmt"
	"os"
	"sync"
)

// lockFile is a best-effort in-process mutex on Windows.
// Cross-process safety via LockFileEx is a known gap on this platform.
var windowsLocks sync.Map // map[string]*sync.Mutex

func lockFile(path string) (release func(), err error) {
	v, _ := windowsLocks.LoadOrStore(path, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	return func() {
		_ = f.Close()
		mu.Unlock()
	}, nil
}
