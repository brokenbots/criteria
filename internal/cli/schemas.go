package cli

import (
	"context"
	"log/slog"
	"strings"

	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// collectSchemas resolves Info() for every adapter referenced in spec and
// returns a schemas map suitable for workflow.Compile. Adapters that cannot be
// resolved (binary not found, network error, etc.) are silently skipped so that
// compile still runs in permissive mode for those adapters — a missing binary
// should not block validation. If log is nil, failures are suppressed silently.
//
//nolint:gocognit // W11: function is inherently complex due to error handling for multiple adapter types
func collectSchemas(ctx context.Context, loader plugin.Loader, spec *workflow.Spec, log *slog.Logger) map[string]workflow.AdapterInfo {
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
		// Steps reference adapters as "<type>.<name>"; extract the type.
		if st.Adapter != "" {
			parts := strings.Split(st.Adapter, ".")
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
