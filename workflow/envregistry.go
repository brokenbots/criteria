package workflow

import (
	"context"
	"fmt"
	"runtime"

	"github.com/hashicorp/hcl/v2"
)

// EnvIsolationKind classifies how an environment isolates adapter execution.
type EnvIsolationKind int

const (
	EnvIsolationNone      EnvIsolationKind = iota // shell — no extra isolation
	EnvIsolationSandbox                           // sandbox — OS-level sandbox
	EnvIsolationContainer                         // container — container runtime
	EnvIsolationRemote                            // remote — out-of-process shim
)

// String returns the human-readable isolation kind name.
func (k EnvIsolationKind) String() string {
	switch k {
	case EnvIsolationNone:
		return "none"
	case EnvIsolationSandbox:
		return "sandbox"
	case EnvIsolationContainer:
		return "container"
	case EnvIsolationRemote:
		return "remote"
	default:
		return "unknown"
	}
}

// EnvHandler is the per-type contract implemented by every registered environment
// type. The compiler uses it to validate HCL bodies and the runtime uses it to
// prepare the execution context.
type EnvHandler interface {
	// Type returns the environment type identifier, e.g. "shell".
	Type() string

	// SupportedOSes returns the GOOS values this environment supports.
	// An empty slice means "all OSes".
	SupportedOSes() []string

	// ValidateFields checks the body of an environment "<type>" "<name>" block
	// and returns diagnostics for unknown or malformed fields.
	ValidateFields(body hcl.Body) hcl.Diagnostics

	// Prepare is called at session-open to prepare the execution context.
	// Skeleton implementations in WS09 return nil, nil; WS10/11/12/20 fill in.
	Prepare(ctx context.Context, body hcl.Body) error

	// IsolationKind reports the isolation class for D40-compat reporting.
	IsolationKind() EnvIsolationKind
}

// EnvRegistry holds the set of registered environment handlers.
type EnvRegistry interface {
	// Lookup returns the handler for envType, or nil if unregistered.
	Lookup(envType string) EnvHandler

	// Registered returns a snapshot of all registered type names.
	Registered() []string
}

// defaultEnvRegistry is the built-in fallback registry used when no external
// registry is injected via CompileOpts. It knows shell, sandbox, container,
// and remote so that the compiler can reference all four types.
type defaultEnvRegistry struct{}

func (defaultEnvRegistry) Lookup(envType string) EnvHandler {
	switch envType {
	case "shell":
		return shellHandlerInstance
	case "sandbox":
		return sandboxHandlerInstance
	case "container":
		return containerHandlerInstance
	case "remote":
		return remoteHandlerInstance
	}
	return nil
}

func (defaultEnvRegistry) Registered() []string {
	return []string{"shell", "sandbox", "container", "remote"}
}

// shellHandlerInstance is the compile-time shell handler baked into workflow.
var shellHandlerInstance = &builtinShellHandler{}

type builtinShellHandler struct{}

func (h *builtinShellHandler) Type() string                                { return "shell" }
func (h *builtinShellHandler) SupportedOSes() []string                     { return nil }
func (h *builtinShellHandler) IsolationKind() EnvIsolationKind             { return EnvIsolationNone }
func (h *builtinShellHandler) Prepare(_ context.Context, _ hcl.Body) error { return nil }

func (h *builtinShellHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
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

// sandboxHandlerInstance is the compile-time sandbox skeleton handler.
var sandboxHandlerInstance = &builtinSandboxHandler{}

type builtinSandboxHandler struct{}

func (h *builtinSandboxHandler) Type() string                                { return "sandbox" }
func (h *builtinSandboxHandler) SupportedOSes() []string                     { return []string{"linux"} }
func (h *builtinSandboxHandler) IsolationKind() EnvIsolationKind             { return EnvIsolationSandbox }
func (h *builtinSandboxHandler) Prepare(_ context.Context, _ hcl.Body) error { return nil }

func (h *builtinSandboxHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
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

// containerHandlerInstance is the compile-time container skeleton handler.
var containerHandlerInstance = &builtinContainerHandler{}

type builtinContainerHandler struct{}

func (h *builtinContainerHandler) Type() string                                { return "container" }
func (h *builtinContainerHandler) SupportedOSes() []string                     { return []string{"linux"} }
func (h *builtinContainerHandler) IsolationKind() EnvIsolationKind             { return EnvIsolationContainer }
func (h *builtinContainerHandler) Prepare(_ context.Context, _ hcl.Body) error { return nil }

func (h *builtinContainerHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
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

// remoteHandlerInstance is the compile-time remote skeleton handler.
var remoteHandlerInstance = &builtinRemoteHandler{}

type builtinRemoteHandler struct{}

func (h *builtinRemoteHandler) Type() string                                { return "remote" }
func (h *builtinRemoteHandler) SupportedOSes() []string                     { return nil }
func (h *builtinRemoteHandler) IsolationKind() EnvIsolationKind             { return EnvIsolationRemote }
func (h *builtinRemoteHandler) Prepare(_ context.Context, _ hcl.Body) error { return nil }

func (h *builtinRemoteHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
	// Remote environments allow mtls { ... } blocks; tolerate them while
	// validating attributes. mtls may also appear as a boolean attribute
	// (mtls = true) for simple on/off configuration.
	attrs, diags := BodyJustAttributesToleratingBlocks(body, HandlerAllowedBlocks(h.Type()))
	for name := range attrs {
		switch name {
		case "variables", "policy_mode", "os",
			"listen_address", "mtls", "accept_token", "accept_digest_from", "config":
			// accepted
		default:
			rng := attrs[name].Range
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("remote environment: unknown attribute %q", name),
				Detail:   "remote environments accept variables, policy_mode, os, listen_address, mtls, accept_token, accept_digest_from, and config.",
				Subject:  &rng,
			})
		}
	}
	return diags
}

// builtinEnvRegistry returns the built-in default registry instance.
func builtinEnvRegistry() EnvRegistry {
	return defaultEnvRegistry{}
}

// envRegistryHostOS returns the host operating system name used for compile-time
// gating. It is a variable so tests can override it.
var envRegistryHostOS = runtime.GOOS
