package testutil

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var (
	unixDialOnce    sync.Once
	unixDialAllowed bool
	tcpDialOnce     sync.Once
	tcpDialAllowed  bool
)

// CanDialLoopbackTCP returns whether dialing a loopback TCP socket (127.0.0.1)
// is permitted in the current runtime environment.
func CanDialLoopbackTCP() bool {
	tcpDialOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return
		}
		defer ln.Close()
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), 50*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		tcpDialAllowed = true
	})
	return tcpDialAllowed
}

// RequireLoopbackTCP skips the test if dialing loopback TCP sockets is restricted.
func RequireLoopbackTCP(t testing.TB) {
	t.Helper()
	if !CanDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}
}

// CanDialUnixSockets returns whether unix domain socket connections are permitted
// in the current runtime environment. In restricted sandbox environments (such as
// macOS Seatbelt without IPC socket permissions), connect(2) fails with EPERM.
func CanDialUnixSockets() bool {
	unixDialOnce.Do(func() {
		dir, err := os.MkdirTemp("", "crit-sock-*")
		if err != nil {
			return
		}
		defer os.RemoveAll(dir)
		sock := filepath.Join(dir, "check.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return
		}
		defer ln.Close()
		c, err := net.DialTimeout("unix", sock, 20*time.Millisecond)
		if err != nil {
			return
		}
		_ = c.Close()
		unixDialAllowed = true
	})
	return unixDialAllowed
}

// RequireUnixSockets skips the test if dialing unix domain sockets is not permitted.
func RequireUnixSockets(t testing.TB) {
	t.Helper()
	if !CanDialUnixSockets() {
		t.Skip("skipping test: dialing unix sockets is restricted in current environment")
	}
}

// BuildPermissiveAdapter builds the permissive adapter test fixture and returns
// the output binary path.
func BuildPermissiveAdapter(t *testing.T) string {
	t.Helper()
	RequireUnixSockets(t)

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	adapterBin := filepath.Join(t.TempDir(), "criteria-adapter-permissive")

	cmd := exec.Command("go", "build", "-o", adapterBin, "./internal/adapterhost/testfixtures/permissive")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build permissive adapter: %v\n%s", err, string(output))
	}

	return adapterBin
}
