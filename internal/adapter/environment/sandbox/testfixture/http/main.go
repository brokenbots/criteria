//go:build linux

// http is a test helper that runs inside the sandbox shim, applies the
// shim restrictions via ApplyEnv, and then performs an HTTP GET to a URL
// passed as the first argument. It prints HTTP_OK on success or HTTP_FAIL
// followed by the error on failure.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

func main() {
	if err := sandbox.ApplyEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "shim error:", err)
		os.Exit(1)
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: http <url>")
		os.Exit(1)
	}
	url := os.Args[1]
	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("HTTP_FAIL", err)
		os.Exit(0)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && string(body) == "OK" {
		fmt.Println("HTTP_OK")
	} else {
		fmt.Printf("HTTP_FAIL status=%d body=%q\n", resp.StatusCode, string(body))
	}
}
