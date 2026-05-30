package container

import (
	"fmt"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/workflow"
)

// FailClosed is returned when an adapter bound to a container environment
// does not publish a container image. It formats the canonical D12c.2
// publisher-pointing message.
type FailClosed struct {
	Reason    string
	Adapter   string
	SourceURL string
	Runtime   string
}

// Error returns the canonical fail-closed message quoting SourceURL.
func (e FailClosed) Error() string {
	return fmt.Sprintf(
		"Error: adapter %s does not publish a container image; cannot run under environment.runtime = %q.\n"+
			"Ask the publisher to enable image publishing, or change the environment to runtime = \"none\".\n"+
			"Publisher: %s",
		e.Adapter, e.Runtime, e.SourceURL)
}

// Prepared holds the validated container execution configuration.
type Prepared struct {
	Runtime  string
	ImageRef manifest.ContainerImageRef
	Policy   PolicyArgs
}

// PolicyArgs carries docker/podman flags derived from environment policy.
type PolicyArgs struct {
	NetworkMode  string
	NetworkName  string
	VolumeMounts []VolumeMount
	CPUs         string
	Memory       string
	Timeout      string
}

// VolumeMount describes a host-to-container bind mount.
type VolumeMount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// Handler validates container environment blocks and prepares execution
// configuration.
type Handler struct{}

// PrepareContext carries the inputs for container preparation.
type PrepareContext struct {
	Environment workflow.EnvironmentNode
	Manifest    *manifest.Manifest
	AdapterRef  string
}

// Prepare validates the container environment and builds the Prepared config.
func (h *Handler) Prepare(ctx *PrepareContext) (Prepared, error) {
	runtime := "docker" // default per D12c
	if v, ok := ctx.Environment.TypeSpecific["runtime"]; ok && !v.IsNull() {
		runtime = ctyString(v)
	}

	if runtime == "none" {
		return Prepared{}, fmt.Errorf(
			"container environment %q has runtime = \"none\"; this is the subprocess path; use a sandbox or shell environment instead",
			ctx.Environment.Name)
	}

	if ctx.Manifest.ContainerImage == nil {
		return Prepared{}, FailClosed{
			Reason:    "adapter does not publish a container image",
			Adapter:   ctx.AdapterRef,
			SourceURL: ctx.Manifest.SourceURL,
			Runtime:   runtime,
		}
	}

	policy, err := buildPolicyArgs(ctx.Environment.TypeSpecific, ctx.AdapterRef)
	if err != nil {
		return Prepared{}, err
	}

	return Prepared{
		Runtime:  runtime,
		ImageRef: *ctx.Manifest.ContainerImage,
		Policy:   policy,
	}, nil
}

func buildPolicyArgs(ts map[string]cty.Value, adapterRef string) (PolicyArgs, error) {
	policy := PolicyArgs{}

	if net, ok := ts["network"]; ok && !net.IsNull() {
		allow := stringListFromObject(net, "allow")
		if len(allow) == 0 {
			policy.NetworkMode = "none"
		} else {
			// Per-session bridge network naming. Full firewall rules are
			// deferred to a future workstream, but the network name itself
			// scopes the adapter to a session-scoped bridge.
			policy.NetworkName = makeNetworkName(adapterRef)
		}
	}

	if fs, ok := ts["filesystem"]; ok && !fs.IsNull() {
		for _, p := range stringListFromObject(fs, "read") {
			if err := validatePath(p); err != nil {
				return policy, fmt.Errorf("filesystem.read: %w", err)
			}
			policy.VolumeMounts = append(policy.VolumeMounts, VolumeMount{
				HostPath:      p,
				ContainerPath: p,
				ReadOnly:      true,
			})
		}
		for _, p := range stringListFromObject(fs, "write") {
			if err := validatePath(p); err != nil {
				return policy, fmt.Errorf("filesystem.write: %w", err)
			}
			policy.VolumeMounts = append(policy.VolumeMounts, VolumeMount{
				HostPath:      p,
				ContainerPath: p,
				ReadOnly:      false,
			})
		}
	}

	if res, ok := ts["resources"]; ok && !res.IsNull() {
		policy.CPUs = stringFromObject(res, "cpu")
		policy.Memory = stringFromObject(res, "memory")
		policy.Timeout = stringFromObject(res, "timeout")
	}

	return policy, nil
}
