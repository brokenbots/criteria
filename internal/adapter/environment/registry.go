// Package environment defines the concrete environment-type registry and handlers.
// The Handler interface lives in workflow so that workflow/ can reference it
// without importing internal/ (import-boundary rule).
package environment

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl/v2"

	"github.com/brokenbots/criteria/workflow"
)

// Handler is the concrete type alias for workflow.EnvHandler. Concrete handlers
// in sub-packages implement this interface.
type Handler = workflow.EnvHandler

// Registry holds the set of registered environment handlers.
type Registry struct {
	handlers map[string]workflow.EnvHandler
}

// NewRegistry creates a registry seeded with the supplied handlers.
func NewRegistry(handlers ...workflow.EnvHandler) *Registry {
	r := &Registry{handlers: make(map[string]workflow.EnvHandler, len(handlers))}
	for _, h := range handlers {
		r.handlers[h.Type()] = h
	}
	return r
}

// Lookup returns the handler for envType, or nil if unregistered.
func (r *Registry) Lookup(envType string) workflow.EnvHandler {
	if r == nil {
		return nil
	}
	return r.handlers[envType]
}

// Registered returns a snapshot of all registered type names.
func (r *Registry) Registered() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	return out
}

// DefaultRegistry is the production registry populated with all known types.
// Skeleton handlers (sandbox, container, remote) accept their designated
// fields and return diagnostics for unknown fields; actual isolation logic
// lands in WS10, WS11, WS12, and WS20.
var DefaultRegistry = NewRegistry(
	&ShellHandler{},
	&SandboxHandler{},
	&ContainerHandler{},
	&RemoteHandler{},
)

// --- Shell handler ---

// ShellHandler validates shell environment blocks.
type ShellHandler struct{}

// Type returns "shell".
func (h *ShellHandler) Type() string { return "shell" }

// SupportedOSes returns nil (all OSes supported).
func (h *ShellHandler) SupportedOSes() []string { return nil }

// ValidateFields checks accepted attributes.
func (h *ShellHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
	attrs, diags := body.JustAttributes()
	for name := range attrs {
		switch name {
		case "variables", "policy_mode", "os", "config":
			// accepted; config is deprecated but tolerated with its own diagnostic path
		default:
			rng := attrs[name].Range
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("shell environment: unknown attribute %q", name),
				Detail:   "shell environments accept only variables, policy_mode, and os.",
				Subject:  &rng,
			})
		}
	}
	return diags
}

// IsolationKind returns workflow.EnvIsolationNone.
func (h *ShellHandler) IsolationKind() workflow.EnvIsolationKind { return workflow.EnvIsolationNone }

// Prepare is a no-op skeleton for the shell handler.
func (h *ShellHandler) Prepare(_ context.Context, _ hcl.Body) error { return nil }

// --- Sandbox handler (skeleton) ---

// SandboxHandler validates sandbox environment blocks.
type SandboxHandler struct{}

// Type returns "sandbox".
func (h *SandboxHandler) Type() string { return "sandbox" }

// SupportedOSes returns ["linux", "darwin"].
func (h *SandboxHandler) SupportedOSes() []string { return []string{"linux", "darwin"} }

// ValidateFields checks accepted attributes.
func (h *SandboxHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
	attrs, diags := body.JustAttributes()
	for name := range attrs {
		switch name {
		case "variables", "policy_mode", "os",
			"filesystem", "network", "resources", "secrets", "config":
			// accepted
		default:
			rng := attrs[name].Range
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("sandbox environment: unknown attribute %q", name),
				Detail:   "sandbox environments accept variables, policy_mode, os, filesystem, network, resources, and secrets.",
				Subject:  &rng,
			})
		}
	}
	return diags
}

// IsolationKind returns workflow.EnvIsolationSandbox.
func (h *SandboxHandler) IsolationKind() workflow.EnvIsolationKind {
	return workflow.EnvIsolationSandbox
}

// Prepare is a no-op skeleton for the sandbox handler.
func (h *SandboxHandler) Prepare(_ context.Context, _ hcl.Body) error { return nil }

// --- Container handler (skeleton) ---

// ContainerHandler validates container environment blocks.
type ContainerHandler struct{}

// Type returns "container".
func (h *ContainerHandler) Type() string { return "container" }

// SupportedOSes returns ["linux"] for now.
func (h *ContainerHandler) SupportedOSes() []string { return []string{"linux"} }

// ValidateFields checks accepted attributes.
func (h *ContainerHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
	attrs, diags := body.JustAttributes()
	for name := range attrs {
		switch name {
		case "variables", "policy_mode", "os",
			"runtime", "image",
			"filesystem", "network", "resources", "secrets", "config":
			// accepted
		default:
			rng := attrs[name].Range
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("container environment: unknown attribute %q", name),
				Detail:   "container environments accept variables, policy_mode, os, runtime, image, filesystem, network, resources, and secrets.",
				Subject:  &rng,
			})
		}
	}
	return diags
}

// IsolationKind returns workflow.EnvIsolationContainer.
func (h *ContainerHandler) IsolationKind() workflow.EnvIsolationKind {
	return workflow.EnvIsolationContainer
}

// Prepare is a no-op skeleton for the container handler.
func (h *ContainerHandler) Prepare(_ context.Context, _ hcl.Body) error { return nil }

// --- Remote handler (skeleton) ---

// RemoteHandler validates remote environment blocks.
type RemoteHandler struct{}

// Type returns "remote".
func (h *RemoteHandler) Type() string { return "remote" }

// SupportedOSes returns nil (all OSes supported for remote).
func (h *RemoteHandler) SupportedOSes() []string { return nil }

// ValidateFields checks accepted attributes.
func (h *RemoteHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
	attrs, diags := body.JustAttributes()
	for name := range attrs {
		switch name {
		case "variables", "policy_mode", "os",
			"listen_address", "mtls", "accept_token", "config":
			// accepted
		default:
			rng := attrs[name].Range
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("remote environment: unknown attribute %q", name),
				Detail:   "remote environments accept variables, policy_mode, os, listen_address, mtls, and accept_token.",
				Subject:  &rng,
			})
		}
	}
	return diags
}

// IsolationKind returns workflow.EnvIsolationRemote.
func (h *RemoteHandler) IsolationKind() workflow.EnvIsolationKind { return workflow.EnvIsolationRemote }

// Prepare is a no-op skeleton for the remote handler.
func (h *RemoteHandler) Prepare(_ context.Context, _ hcl.Body) error { return nil }
