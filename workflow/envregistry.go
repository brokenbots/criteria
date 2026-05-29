package workflow

import (
	"fmt"
	"runtime"

	"github.com/hashicorp/hcl/v2"
)

// EnvIsolationKind classifies how an environment isolates adapter execution.
type EnvIsolationKind int

const (
	EnvIsolationNone     EnvIsolationKind = iota // shell — no extra isolation
	EnvIsolationSandbox                          // sandbox — OS-level sandbox
	EnvIsolationContainer                        // container — container runtime
	EnvIsolationRemote                           // remote — out-of-process shim
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
// registry is injected via CompileOpts. It only knows "shell".
type defaultEnvRegistry struct{}

func (defaultEnvRegistry) Lookup(envType string) EnvHandler {
	if envType == "shell" {
		return shellHandlerInstance
	}
	return nil
}

func (defaultEnvRegistry) Registered() []string { return []string{"shell"} }

// shellHandlerInstance is the compile-time shell handler baked into workflow.
// It validates accepted fields and returns no-op runtime info.
var shellHandlerInstance = &builtinShellHandler{}

type builtinShellHandler struct{}

func (h *builtinShellHandler) Type() string              { return "shell" }
func (h *builtinShellHandler) SupportedOSes() []string     { return nil }
func (h *builtinShellHandler) IsolationKind() EnvIsolationKind { return EnvIsolationNone }

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

// builtinEnvRegistry returns the built-in default registry instance.
func builtinEnvRegistry() EnvRegistry {
	return defaultEnvRegistry{}
}

// effectiveEnvRegistry returns opts.EnvRegistry if set; otherwise the built-in
// shell-only registry so that standalone compiles continue to work.
func effectiveEnvRegistry(opts *CompileOpts) EnvRegistry {
	if opts != nil && opts.EnvRegistry != nil {
		return opts.EnvRegistry
	}
	return builtinEnvRegistry()
}


// envRegistryHostOS returns the host operating system name used for compile-time
// gating. It is a variable so tests can override it.
var envRegistryHostOS = runtime.GOOS

// validateEnvOS checks whether hostOS is in the list of supported OSes.
// If supported is empty, any OS is accepted.
func validateEnvOS(hostOS string, supported []string, subject *hcl.Range) hcl.Diagnostics {
	if len(supported) == 0 {
		return nil
	}
	for _, os := range supported {
		if os == hostOS {
			return nil
		}
	}
	return hcl.Diagnostics{&hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("environment type requires OS %v; host is %q", supported, hostOS),
		Subject:  subject,
	}}
}
