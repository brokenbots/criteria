// Package pluginhost provides the public contract for Overseer adapter plugin
// authors. An out-of-process plugin binary that implements [Service] and calls
// [Serve] will interoperate with any Overseer host without reaching through the
// internal/ package tree.
//
// # Minimum entrypoint
//
//	package main
//
//	import (
//		"context"
//		pluginhost "github.com/brokenbots/overseer/sdk/pluginhost"
//		pb "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
//	)
//
//	type myPlugin struct{}
//
//	func (p *myPlugin) Info(ctx context.Context, req *pb.InfoRequest) (*pb.InfoResponse, error) { ... }
//	// ... implement remaining Service methods ...
//
//	func main() { pluginhost.Serve(&myPlugin{}) }
//
// # Package stability
//
// This package is v0. The [Service] interface and wire protocol are stable
// across minor Overseer releases; breaking changes follow the SDK bump policy
// in CONTRIBUTING.md before any external consumer depends on them.
package pluginhost
