// Package diagutil holds shared diagnostic helpers used by the CLI and langserver.
package diagutil

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hashicorp/hcl/v2"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// CollectSchemas resolves Info() for every adapter referenced in spec and
// returns a schemas map suitable for workflow.Compile, plus warning diagnostics
// for any adapter whose schema could not be resolved. Adapters that cannot be
// resolved (binary not found, network error, Info() failure) are skipped so
// that compile still runs in permissive mode for those adapters — a missing
// binary should not block validation — but each such skip yields a DiagWarning
// so callers can surface it (and promote it to an error via
// --warnings-as-errors). If log is non-nil, failures are also logged at debug.
//
//nolint:gocognit,gocyclo // inherently complex: error handling branches per adapter type with partial failure tolerance
func CollectSchemas(ctx context.Context, loader adapterhost.Loader, spec *workflow.Spec, log *slog.Logger) (map[string]workflow.AdapterInfo, hcl.Diagnostics) {
	if loader == nil || spec == nil {
		return nil, nil
	}

	// Collect unique adapter types from declared adapters and step references.
	seen := map[string]bool{}
	for _, ad := range spec.Adapters {
		if ad.Type != "" {
			seen[ad.Type] = true
		}
	}
	for i := range spec.Steps {
		st := &spec.Steps[i]
		// Steps reference adapters via traversal expressions in the Remain body.
		// Extract the adapter type from the resolved reference.
		if adapterRef, present, _ := workflow.ResolveStepAdapterRef(st.Remain); present && adapterRef != "" {
			parts := strings.Split(adapterRef, ".")
			if len(parts) == 2 && parts[0] != "" {
				seen[parts[0]] = true
			}
		}
	}

	if len(seen) == 0 {
		return nil, nil
	}

	schemas := make(map[string]workflow.AdapterInfo, len(seen))
	var diags hcl.Diagnostics
	for typeName := range seen {
		p, err := loader.Resolve(ctx, typeName)
		if err != nil {
			if log != nil {
				log.Debug("schema collection: could not resolve adapter", "adapter_type", typeName, "err", err)
			}
			diags = append(diags, unverifiedAdapterDiag(typeName, err))
			continue
		}
		info, err := p.Info(ctx)
		p.Kill()
		if err != nil {
			if log != nil {
				log.Debug("schema collection: Info() failed", "adapter_type", typeName, "err", err)
			}
			diags = append(diags, unverifiedAdapterDiag(typeName, err))
			continue
		}
		schemas[typeName] = info.AdapterInfo
	}
	return schemas, diags
}

// unverifiedAdapterDiag builds the warning emitted when an adapter's schema
// cannot be resolved at compile time, so the graph is only permissively
// validated for that adapter.
func unverifiedAdapterDiag(typeName string, err error) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  fmt.Sprintf("adapter %q schema unverified", typeName),
		Detail: fmt.Sprintf(
			"Could not resolve adapter %q (%v). Its config/input/output schemas are unknown, "+
				"so the workflow graph was validated permissively for this adapter and errors "+
				"may only surface at runtime. Ensure the adapter binary is installed (run "+
				"`criteria adapter lock`) to verify it, or pass --warnings-as-errors to fail fast.",
			typeName, err),
	}
}
