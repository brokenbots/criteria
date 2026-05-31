// Package remote implements the "remote" environment handler and host-side
// phone-home shim for WS20.
package remote

import (
	"github.com/brokenbots/criteria/internal/adapterhost"
	hplugin "github.com/hashicorp/go-plugin"
)

// makeHandle wraps a Client + pluginClient into a session-manager-compatible
// Handle using the existing rpcHandle machinery in adapterhost.
func makeHandle(name string, client adapterhost.Client, pluginClient *hplugin.Client, onKill func()) adapterhost.Handle {
	h := adapterhost.NewRPCHandle(name, pluginClient, client)
	// Wrap the underlying onKill so we can also run bridge cleanup.
	if onKill != nil {
		// We need to capture the original Kill behavior. NewRPCHandle sets
		// onKill to a no-op; we can’t access the internal once.Do from here,
		// so we rely on pluginClient.Kill() being idempotent and call both.
		goKill := onKill
		onKill = func() {
			h.Kill()
			goKill()
		}
	}
	return &killWrapper{Handle: h, onKill: onKill}
}

// killWrapper delegates to an underlying Handle but overrides Kill.
type killWrapper struct {
	adapterhost.Handle
	onKill func()
}

func (w *killWrapper) Kill() {
	if w.onKill != nil {
		w.onKill()
	} else {
		w.Handle.Kill()
	}
}
