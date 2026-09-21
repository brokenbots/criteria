package adapterhost

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// Transport liveness policy for remote adapter connections (CRI-276).
//
// A remote adapter session may legitimately sit idle for very long stretches
// while other sessions work: the adapter's log stream can end early (the
// engine disarms its stall detector when that happens), leaving the gRPC
// channel with no active RPCs. With go-plugin/grpc-go defaults that channel
// idles out after 30 minutes and closes its transport, which tears down the
// shim bridge and the phone-home connection; the pod then restarts the
// adapter process while the engine still holds the dead handle, and the next
// step fails with "gRPC client transport closed (adapter or shim closed the
// connection)" — exactly the CRI-272 run 225a8c72 signature at 32-33 minutes
// of idleness.
//
// The policy below makes both ends of the phone-home chain behave like the
// local go-plugin path: idleness alone never closes the connection, and
// periodic keepalives keep intermediaries (NATs, load balancers, conntrack)
// from silently dropping the path and let a dead peer be detected promptly.
//
// Compatibility: the client cadence deliberately stays at or above grpc-go's
// default server enforcement floor (MinTime = 5 minutes, idle pings
// forbidden), so a pod running the SDK's own default-grpc.NewServer (e.g. an
// adapter serving ServeRemote directly) still tolerates our pings. Pods built
// from this repository's remote runner use the tolerant server policy below.

const (
	// RemoteServerPingInterval is how often the remote runner's gRPC server
	// pings a phone-home connection, including when no RPCs are active. This
	// is what keeps the connection warm while a session is fully idle.
	RemoteServerPingInterval = time.Minute

	// RemoteServerPingTimeout is how long the server waits for a keepalive
	// acknowledgment before declaring the connection dead.
	RemoteServerPingTimeout = 20 * time.Second

	// RemoteServerMinClientPing is the fastest client ping cadence the remote
	// runner's server tolerates before declaring too_many_pings. Well below
	// grpc-go's default 5-minute floor so host clients may ping freely.
	RemoteServerMinClientPing = 30 * time.Second

	// RemoteClientPingInterval is how often the host-side client pings after
	// inactivity. grpc-go's default server enforcement forbids idle pings and
	// rejects pings faster than 5 minutes apart, so this stays above that
	// floor; idleness is handled by disabling the client idle timeout instead.
	RemoteClientPingInterval = 10 * time.Minute

	// RemoteClientPingTimeout is how long the client waits for a keepalive
	// acknowledgment before considering the connection dead.
	RemoteClientPingTimeout = 20 * time.Second
)

// RemoteKeepaliveServerOptions returns the gRPC server options for the remote
// runner's phone-home adapter server (serve_remote). The server pings idle
// connections every RemoteServerPingInterval even with no active streams, and
// tolerates client pings at any cadence of RemoteServerMinClientPing or
// slower, including when the connection carries no streams. No
// MaxConnectionIdle or MaxConnectionAge is set: idleness alone must never
// close a live session's connection.
func RemoteKeepaliveServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    RemoteServerPingInterval,
			Timeout: RemoteServerPingTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             RemoteServerMinClientPing,
			PermitWithoutStream: true,
		}),
	}
}

// RemoteKeepaliveDialOptions returns the gRPC dial options for host-side
// clients of a remote adapter transport (the shim's reattach client, and any
// other client bridged across a phone-home connection). The client idle
// timeout is disabled so an idle session never closes its own transport, and
// keepalive pings (only while streams are active, at a cadence safe against
// grpc-go's default server enforcement) keep long-lived streams warm.
func RemoteKeepaliveDialOptions() []grpc.DialOption {
	return KeepaliveDialOptionsFor(keepalive.ClientParameters{
		Time:                RemoteClientPingInterval,
		Timeout:             RemoteClientPingTimeout,
		PermitWithoutStream: false,
	})
}

// KeepaliveDialOptionsFor returns client dial options that disable the gRPC
// idle timeout and apply the given keepalive parameters. Splitting out the
// parameterized form lets tests compress the keepalive clock without
// weakening production's conservative cadence.
func KeepaliveDialOptionsFor(params keepalive.ClientParameters) []grpc.DialOption {
	return []grpc.DialOption{
		// An idle remote session is healthy, not stale: the client must never
		// close its transport just because no RPCs are in flight.
		grpc.WithIdleTimeout(0),
		grpc.WithKeepaliveParams(params),
	}
}