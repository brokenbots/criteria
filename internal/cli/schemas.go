package cli

import (
	"context"
	"log/slog"

	"github.com/brokenbots/overlord/overseer/internal/plugin"
	"github.com/brokenbots/overlord/workflow"
)

// collectSchemas resolves Info() for every adapter referenced in spec and
// returns a schemas map suitable for workflow.Compile. Adapters that cannot be
// resolved (binary not found, network error, etc.) are silently skipped so that
// compile still runs in permissive mode for those adapters — a missing binary
// should not block validation. If log is nil, failures are suppressed silently.
func collectSchemas(ctx context.Context, loader plugin.Loader, spec *workflow.Spec, log *slog.Logger) map[string]workflow.AdapterInfo {
	if loader == nil || spec == nil {
		return nil
	}

	// Collect unique adapter names from agents and adapterless steps.
	seen := map[string]bool{}
	for _, ag := range spec.Agents {
		if ag.Adapter != "" {
			seen[ag.Adapter] = true
		}
	}
	for _, st := range spec.Steps {
		if st.Adapter != "" {
			seen[st.Adapter] = true
		}
	}

	if len(seen) == 0 {
		return nil
	}

	schemas := make(map[string]workflow.AdapterInfo, len(seen))
	for name := range seen {
		p, err := loader.Resolve(ctx, name)
		if err != nil {
			if log != nil {
				log.Debug("schema collection: could not resolve adapter", "adapter", name, "err", err)
			}
			continue
		}
		info, err := p.Info(ctx)
		p.Kill()
		if err != nil {
			if log != nil {
				log.Debug("schema collection: Info() failed", "adapter", name, "err", err)
			}
			continue
		}
		schemas[name] = info.AdapterInfo
	}
	return schemas
}
