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
//		v2 "github.com/brokenbots/criteria/proto/criteria/v2"
//	)
//
//	type myAdapter struct{ adapterhost.UnimplementedPermissions }
//
//	func (a *myAdapter) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) { ... }
//	// ... implement remaining Service methods ...
//
//	func main() { adapterhost.Serve(&myAdapter{}) }
//
// # v1 → v2 protocol break (WS03)
//
// WS03 migrated the host wire layer to v2. Adapter binaries compiled against
// the v1 SDK will fail the go-plugin handshake with a protocol version
// mismatch. The v1 adapter-plugin protocol (proto/criteria/v1/adapter_plugin.proto)
// was removed from the host but reverted in WS03 round 4 because the adapter
// binaries (copilot, mcp, noop) and greeter example have not yet been migrated
// to the v2 Service interface. Those binaries are being migrated in WS30–WS36.
// Until migration completes, adapter binaries continue to reference the v1
// generated types and will not compile against the v2 Service interface.
//
// # Package stability
//
// This package is v0. The [Service] interface and v2 wire protocol are the
// stable surface for adapter authors; breaking changes follow the SDK bump
// policy in CONTRIBUTING.md.
//
// # CHANGELOG forward-pointer
//
// WS01 renamed this package from sdk/pluginhost to sdk/adapterhost. The
// CHANGELOG entry is deferred to the WS39 cleanup gate.
package adapterhost
