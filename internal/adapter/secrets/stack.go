package secrets

import (
	"context"
	"fmt"
	"sort"

	"github.com/brokenbots/criteria/workflow"
)

// Stack holds an ordered list of providers. Resolve walks the list and
// returns the first successfully resolved value.
type Stack struct {
	providers []Provider
}

// Resolve walks the stack in order. The first provider that CanResolve and
// returns a non-error value wins.
func (s *Stack) Resolve(ctx context.Context, ref OriginRef) (string, error) {
	for _, p := range s.providers {
		if !p.CanResolve(ref) {
			continue
		}
		val, err := p.Resolve(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("provider %q: %w", p.Name(), err)
		}
		return val, nil
	}
	return "", fmt.Errorf("no provider can resolve %q", ref)
}

// StackFromEnvironment builds a provider stack from the environment node's
// secrets policy. The configured primary provider is placed first; all
// other built-in providers follow in a stable order. If a fallback list is
// present in the policy it is honoured after the primary.
func StackFromEnvironment(env *workflow.EnvironmentNode) (*Stack, error) {
	if env == nil || env.Secrets == nil {
		// No secrets policy → return a default stack with all providers.
		return DefaultStack(), nil
	}

	policy := env.Secrets
	all := map[string]Provider{
		"env":      EnvProvider{},
		"file":     &FileProvider{},
		"keychain": &KeychainProvider{},
		"vault":    VaultProvider{},
		"sops":     SOPSProvider{},
	}

	ordered := make([]Provider, 0, len(all))

	// Primary provider.
	if policy.Provider != "" {
		if p, ok := all[policy.Provider]; ok {
			ordered = append(ordered, p)
			delete(all, policy.Provider)
		}
	}

	// Explicit fallbacks.
	for _, name := range policy.Fallback {
		if p, ok := all[name]; ok {
			ordered = append(ordered, p)
			delete(all, name)
		}
	}

	// Remaining providers in deterministic order.
	remaining := make([]string, 0, len(all))
	for name := range all {
		remaining = append(remaining, name)
	}
	sort.Strings(remaining)
	for _, name := range remaining {
		ordered = append(ordered, all[name])
	}

	return &Stack{providers: ordered}, nil
}

// DefaultStack returns a stack containing all built-in providers in a
// deterministic order (env → file → keychain → sops → vault).
func DefaultStack() *Stack {
	return &Stack{providers: []Provider{
		EnvProvider{},
		&FileProvider{},
		&KeychainProvider{},
		SOPSProvider{},
		VaultProvider{},
	}}
}
