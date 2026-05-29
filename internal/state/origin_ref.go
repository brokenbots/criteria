package state

// OriginRef tracks the provenance of a secret value in the Criteria state
// machine. It is used by the taint engine to record where a secret came from
// so that diagnostics and audit logs can provide precise source information.
type OriginRef struct {
	// Kind is the category of origin: "variable", "shared_variable",
	// "adapter_secret", "step_secret_input", or "environment_secret".
	Kind string
	// Name is the specific identifier within the kind (e.g. the variable name,
	// adapter key, or step name).
	Name string
}
