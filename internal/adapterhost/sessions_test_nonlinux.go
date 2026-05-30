//go:build !linux

package adapterhost

import "testing"

// Stub so that the package compiles on non-Linux platforms. All
// non-sandbox tests live in sessions_test.go which is gated behind
// the linux build tag; they are moved here so they run on all
// platforms. Linux-only sandbox tests remain in sessions_test.go.

// TODO: move non-sandbox tests from sessions_test.go to here or to
// a common no-tag test file so they run on all platforms.
