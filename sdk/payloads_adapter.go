package criteria

import pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

// AdapterEvent wraps an arbitrary event emitted by an adapter plugin.
type AdapterEvent = pb.AdapterEvent

// AdapterLifecycleProvisionWanted is emitted when the engine requests
// provisioning of an adapter execution environment for a scope instance
// (CRI-115). The orchestrator reconciles the adapter pod on observing it
// through the orchestrator event subscription API (CRI-133).
type AdapterLifecycleProvisionWanted = pb.AdapterLifecycleProvisionWanted

// AdapterLifecycleReleased is emitted when the engine signals that the
// adapter execution environment for a scope instance is no longer needed and
// may be released (CRI-115).
type AdapterLifecycleReleased = pb.AdapterLifecycleReleased
