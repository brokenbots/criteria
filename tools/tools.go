//go:build tools
// +build tools

// Package tools records tool dependencies so their exact versions are pinned
// in go.mod and reproducible for all contributors. Invoke via go tool, not a
// globally-installed binary.
package tools

import (
	_ "github.com/golangci/golangci-lint/cmd/golangci-lint"
)
