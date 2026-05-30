//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

func main() {
	err := sandbox.ApplyEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim error:", err)
		os.Exit(1)
	}
	// Check /etc/passwd (should fail, not in ReadPaths).
	f, err := os.Open("/etc/passwd")
	if err == nil {
		f.Close()
		fmt.Println("OPEN_OK")
	} else {
		fmt.Println("OPEN_FAIL")
	}
	// Check /tmp (should succeed, in ReadPaths).
	g, err := os.Open("/tmp")
	if err == nil {
		g.Close()
		fmt.Println("TMP_OK")
	} else {
		fmt.Println("TMP_FAIL")
	}
	// Check setuid(0) (should fail due to userns mapping).
	_, _, e1 := syscall.RawSyscall(syscall.SYS_SETUID, 0, 0, 0)
	if e1 == 0 {
		fmt.Println("SETUID_OK")
	} else {
		fmt.Println("SETUID_FAIL")
	}
	// Check connect to 8.8.8.8:53 (should fail due to net namespace).
	conn, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		fmt.Println("SOCKET_FAIL")
		os.Exit(0)
	}
	addr := syscall.SockaddrInet4{Port: 53, Addr: [4]byte{8, 8, 8, 8}}
	err = syscall.Connect(conn, &addr)
	if err == nil {
		fmt.Println("CONNECT_OK")
	} else {
		fmt.Println("CONNECT_FAIL")
	}
	syscall.Close(conn)
	os.Exit(0)
}
