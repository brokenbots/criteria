// Package adapterhost provides the public contract for Criteria adapter authors.
// An out-of-process adapter binary that implements [Service] and calls [Serve]
// will interoperate with any Criteria host without reaching through the
// internal/ package tree.
//
// # Minimum entrypoint
//
//	package main
//
//	import (
//		"context"
//		adapterhost "github.com/brokenbots/criteria/sdk/adapterhost"
//		pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
//	)
//
//	type myAdapter struct{}
//
//	func (a *myAdapter) Info(ctx context.Context, req *pb.InfoRequest) (*pb.InfoResponse, error) { ... }
//	// ... implement remaining Service methods ...
//
//	func main() { adapterhost.Serve(&myAdapter{}) }
//
// # Package stability
//
// This package is v0. The [Service] interface and wire protocol are stable
// across minor Criteria releases; breaking changes follow the SDK bump policy
// in CONTRIBUTING.md before any external consumer depends on them.
//
// # CHANGELOG forward-pointer
//
// WS01 renamed this package from sdk/pluginhost to sdk/adapterhost. The
// CHANGELOG entry is deferred to the WS39 cleanup gate.
package adapterhost
