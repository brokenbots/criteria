package cli

import (
	"context"
	"log/slog"
	"strings"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// collectSchemas resolves Info() for every adapter referenced in spec and
// returns a schemas map suitable for workflow.Compile. Adapters that cannot be
// resolved (binary not found, network error, etc.) are silently skipped so that
// compile still runs in permissive mode for those adapters — a missing binary
// should not block validation. If log is nil, failures are suppressed silently.
//
//nolint:gocognit,gocyclo // inherently complex: error handling branches per adapter type with partial failure tolerance
func collectSchemas(ctx context.Context, loader adapterhost.Loader, spec *workflow.Spec, log *slog.Logger) map[string]workflow.AdapterInfo {
	if loader == nil || spec == nil {
		return nil
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
		return nil
	}

	schemas := make(map[string]workflow.AdapterInfo, len(seen))
	for typeName := range seen {
		p, err := loader.Resolve(ctx, typeName)
		if err != nil {
			if log != nil {
				log.Debug("schema collection: could not resolve adapter", "adapter_type", typeName, "err", err)
			}
			continue
		}
		info, err := p.Info(ctx)
		p.Kill()
		if err != nil {
			if log != nil {
				log.Debug("schema collection: Info() failed", "adapter_type", typeName, "err", err)
			}
			continue
		}
		schemas[typeName] = info.AdapterInfo
	}
	return schemas
}
