package engine

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// dialScopeRegistrar implements remote.ScopeRegistrar (KB-25): when a shim
// receives a digest-verified dial for a scope it has no token registered
// for, the presented identity is validated here against the run data
// directory's surviving rotated token files for that scope and adapter type.
// A match is re-registered with the shim so the dialing pod's re-handshake
// is accepted instead of looping on accept-fail rejections until an
// operator kills the run.
//
// Only UNCLAIMED instances may be re-registered:
// filterUnclaimedScopeInstances drops candidates referenced by a readable
// current record, which covers both another adapter's active claim and a
// deliberately released tombstone (CRI-115), so a torn-down pod's token can
// never be resurrected and two adapters can never share one scope instance.
type dialScopeRegistrar struct {
	dataDir  string
	envKey   string
	sessions *adapterhost.SessionManager
}

// RegisterScopeOnDial validates a rejected dial's presented token against
// the run's persisted rotated tokens and re-registers a match. It returns a
// non-nil error when nothing matches so the shim keeps the typed
// unregistered-scope rejection.
func (r *dialScopeRegistrar) RegisterScopeOnDial(adapterType, scope, presentedToken string) error {
	if r == nil || r.dataDir == "" || r.sessions == nil || adapterType == "" || scope == "" {
		return errors.New("scope re-registration is unavailable")
	}
	scopeName, instanceID, ok := splitScopeKey(scope)
	if !ok {
		return fmt.Errorf("scope %q is not a scopeName/instanceID key", scope)
	}
	token, err := survivingScopeToken(r.dataDir, scopeName, instanceID, adapterType, presentedToken)
	if err != nil {
		return err
	}
	if err := r.sessions.RegisterRemoteScopeForEnv(r.envKey, scope, token); err != nil {
		return fmt.Errorf("register surviving scope token: %w", err)
	}
	slog.Info("re-registered surviving scope token at dial time (KB-25)",
		"environment", r.envKey, "scope", scope, "adapter_type", adapterType)
	return nil
}

// splitScopeKey splits "<scopeName>/<scopeInstanceID>". Scope labels never
// contain "/" (checkPathLabel), so the first slash separates the two halves.
func splitScopeKey(scope string) (scopeName, instanceID string, ok bool) {
	scopeName, instanceID, ok = strings.Cut(scope, "/")
	if !ok || scopeName == "" || instanceID == "" {
		return "", "", false
	}
	return scopeName, instanceID, true
}

// survivingScopeToken matches presentedToken (constant-time) against the
// unclaimed rotated token files persisted for (scopeName, adapterType) and
// returns the token to register, or an error when nothing matches.
func survivingScopeToken(dataDir, scopeName, instanceID, adapterType, presentedToken string) (string, error) {
	candidates, err := scanScopeTokenFiles(dataDir, scopeName, adapterType)
	if err != nil {
		return "", fmt.Errorf("scan surviving tokens for scope %q: %w", scopeName, err)
	}
	for _, cand := range candidates {
		if cand.instanceID != instanceID {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(presentedToken), []byte(cand.token)) != 1 {
			return "", fmt.Errorf("presented token does not match the surviving token for instance %q", instanceID)
		}
		unclaimed := filterUnclaimedScopeInstances(dataDir, scopeName, []scannedScopeToken{cand})
		if len(unclaimed) == 0 {
			return "", fmt.Errorf("instance %q is claimed or released; refusing to re-register", instanceID)
		}
		return cand.token, nil
	}
	return "", fmt.Errorf("no surviving token file for instance %q", instanceID)
}
