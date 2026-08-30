//go:build linux

// curl is a test helper that runs inside the sandbox shim, applies the
// shim restrictions via ApplyEnv, and then exercises a real libc resolver
// path followed by an HTTP fetch. It is used to verify that network-enabled
// sandboxes allow the batched socket syscalls (sendmmsg/recvmmsg) used by
// glibc's threaded resolver.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

func main() {
	if err := sandbox.ApplyEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "shim error:", err)
		os.Exit(1)
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: curl <url> [hostname-to-resolve]")
		os.Exit(1)
	}
	url := os.Args[1]
	host := "api.linear.app"
	if len(os.Args) > 2 {
		host = os.Args[2]
	}

	// Print the nsswitch hosts configuration so CI logs show which resolver
	// modules are available on the runner (e.g. "files dns" vs "files resolve
	// [!UNAVAIL=return] dns"). This helps verify that GETENT_OK actually
	// exercises the glibc "dns" module and therefore sendmmsg/recvmmsg.
	printNSSwitchHosts()

	// Exercise the glibc threaded resolver via getent. This path is known
	// to issue paired A/AAAA queries using sendmmsg on Debian arm64.
	if err := runCommand("getent", "hosts", host); err != nil {
		fmt.Println("GETENT_FAIL", err)
	} else {
		fmt.Println("GETENT_OK")
	}

	// Perform an HTTP fetch with curl. If DNS resolution is blocked by
	// seccomp this fails before the TCP connect. Write the response body to
	// a temp file so the status code printed by -w is clean.
	bodyFile, err := os.CreateTemp("/tmp", "criteria-curl-*.body")
	if err != nil {
		fmt.Println("CURL_FAIL", err)
		return
	}
	defer os.Remove(bodyFile.Name())
	bodyFile.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "curl", "-sS", "-o", bodyFile.Name(), "-w", "%{http_code}", "--max-time", "10", url)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("CURL_FAIL exit=%v output=%q\n", err, string(out))
		return
	}
	if strings.TrimSpace(string(out)) == "200" {
		fmt.Println("CURL_OK")
	} else {
		fmt.Printf("CURL_FAIL status=%q\n", string(out))
	}
}

func runCommand(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Run()
}

// printNSSwitchHosts emits the nsswitch.conf "hosts:" line, if present, to
// stdout. This makes the CI logs self-documenting about which resolver path
// getent actually exercised (files/dns vs files/resolve/dbus).
func printNSSwitchHosts() {
	data, err := os.ReadFile("/etc/nsswitch.conf")
	if err != nil {
		fmt.Println("NSSWITCH_READ_ERR", err)
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "hosts:") {
			fmt.Println("NSSWITCH_HOSTS", line)
			return
		}
	}
	fmt.Println("NSSWITCH_HOSTS not_found")
}
