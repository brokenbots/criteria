package peer

// host.go binds the peer's conn-agnostic supervision machinery to the
// envhost.Host contract for container environments (ADR-0014 M3). The
// remote shim owns a Host bound from remote.NewHost; this file is the
// container counterpart: a criteria-launched, in-process supervisor that
// spawns each adapter definition's child itself through a go-plugin
// RunnerFunc (the container-runner shape), journals spawn/exit and
// CrashClassified facts in the shared supervision journal, and serves
// Execute sessions on the children until they are displaced. Crash
// classification for container handles therefore comes from
// CrashClassified events in the journal, never from the transport's
// string matching.
//
// Sessions are engagements, not child lifetimes: the handle handed to the
// session manager wraps the live child, and a session's close or kill only
// ends that session in the child. With child keepalive enabled
// (CRITERIA_PEER_CHILD_KEEPALIVE, default on) the child survives the
// engagement end and dies at host shutdown, at a Control kill, or by
// crashing; with keepalive disabled the child is killed as soon as the
// definition's aggregate engagement across all scopes drains to zero.
// Children are keyed by adapter DEFINITION identity (per-definition
// children, one process per "<type>.<name>" shared across scopes), and a
// crashed child never auto-respawns from Ensure — recovery is the crash
// policy's to command through the Control EnsureFreshHandle arm, which
// displaces the stale child and spawns a fresh one from the definition's
// registered boot spec.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"sync"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin/runner"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/envhost"
	"strings"
)

// ChildRunner spawns one adapter child through a go-plugin RunnerFunc —
// the same shape the container runner builder returns for image-backed
// adapter definitions, and the lane through which test fixtures inject a
// resolved binary.
type ChildRunner = func(hclog.Logger, *exec.Cmd, string) (runner.Runner, error)

// ChildBoot is one adapter definition's spawn contract: the child identity
// facts journaled with every spawn plus the RunnerFunc that starts the
// child process.
type ChildBoot struct {
	// Version is the adapter-reported version string.
	Version string
	// Binary is the image reference (or binary path) recorded as the
	// spawned artifact identity.
	Binary string
	// Digest pins the adapter artifact (OCI digest or content hash).
	Digest string
	// Runner starts the child process: a container runner for image-backed
	// adapter definitions, or a test injection lane per the go-plugin
	// runner contract.
	Runner ChildRunner
}

// HostConfig configures an in-process container host. Zero-valued knobs
// take the peer runtime's defaults; KeepAlive carries
// CRITERIA_PEER_CHILD_KEEPALIVE's semantic default (true) and must be
// resolved explicitly by the binder.
type HostConfig struct {
	// KeepAlive: when true children survive engagement ends (killed only
	// at host shutdown, at a Control kill, or by a crash); when false a
	// definition's child is killed as soon as its aggregate engagement
	// drops to zero.
	KeepAlive bool
	// JournalLimit bounds the supervision journal ring (0 → default).
	JournalLimit int
	// ExitPoll is the child process-exit poll interval (0 → default; the
	// tests shorten it for deterministic watcher timing).
	ExitPoll time.Duration
	// ShutdownGrace bounds each child's shutdown wait and a fresh-handle
	// displacement's exit wait (0 → default).
	ShutdownGrace time.Duration
	// Log receives supervision logs (nil → slog.Default).
	Log *slog.Logger
}

// InProcessHost serves envhost.Host for one container environment on top
// of a peerRuntime: each adapter definition hosts exactly one child
// process; every (scope, definition) Ensure engagement returns a handle
// wrapping that child.
type InProcessHost struct {
	rt *peerRuntime
	// boot resolves a definition's spawn contract from the
	// workflow/lockfile context the binder closes over; the host stores
	// the first resolution per definition as its boot spec so respawns
	// reuse it verbatim.
	boot func(context.Context, envhost.Definition) (ChildBoot, error)
	log  *slog.Logger

	mu sync.Mutex
	// handles caches the supervised wrapper per (scope, definition). The
	// slot is re-bound whenever the definition's live child changes; the
	// engagement ledger below is what keeps wrapper replacement
	// idempotent.
	handles map[hostHandleKey]*supervisedHostHandle
	// engagements tracks live session ids per (scope, definition): the
	// keepalive-idle decision aggregates across scopes and engagement
	// drops stay idempotent across wrapper re-binding.
	engagements map[hostHandleKey]map[string]struct{}

	stopOnce sync.Once

	// bootSpecs is the host's first-wins cache of each definition's
	// resolved spawn contract: the first Ensure resolves through the boot
	// hook, and every later Ensure, respawn and freshen reuses it verbatim
	// — workflow/lockfile changes mid-run never re-spawn a child from a
	// different artifact.
	bootSpecs map[string]childSpec
}

// hostHandleKey keys host-owned state per (scope, definition) engagement.
type hostHandleKey struct {
	scopeKey string
	defName  string
}

// NewInProcessHost builds a container environment's host. The boot
// resolver must return a spawn contract for every definition the
// environment's adapters declare; a definition that does not run in
// container mode fails its Ensure (fail closed).
func NewInProcessHost(cfg HostConfig, boot func(context.Context, envhost.Definition) (ChildBoot, error)) *InProcessHost {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	exitPoll := cfg.ExitPoll
	if exitPoll <= 0 {
		exitPoll = defaultExitPollInterval
	}
	grace := cfg.ShutdownGrace
	if grace <= 0 {
		grace = defaultControlGrace
	}
	journalLimit := cfg.JournalLimit
	if journalLimit <= 0 {
		journalLimit = DefaultJournalLimit
	}
	rt := &peerRuntime{
		cfg:           &Config{ChildKeepAlive: cfg.KeepAlive, JournalLimit: journalLimit},
		log:           log,
		loader:        adapterhost.NewLoader(),
		journal:       NewEventJournal(journalLimit),
		children:      map[string]*childState{},
		bootSpec:      map[string]childSpec{},
		serveCounts:   map[string]int{},
		shutdownGrace: grace,
		exitPoll:      exitPoll,
		stopWatch:     make(chan struct{}),
		watchDone:     make(chan struct{}),
	}
	return &InProcessHost{
		rt:          rt,
		boot:        boot,
		log:         log,
		handles:     map[hostHandleKey]*supervisedHostHandle{},
		engagements: map[hostHandleKey]map[string]struct{}{},
		bootSpecs:   map[string]childSpec{},
	}
}

// Ensure implements envhost.Host. The first Ensure call per definition
// registers the boot spec (first resolution wins; respawns reuse it
// verbatim, no re-resolution) and arms the runtime; a later call re-adopts
// the live handle. A dead child with a crash-classified exit is NOT
// respawned here — crash recovery is the crash policy's to command through
// Control EnsureFreshHandle.
func (h *InProcessHost) Ensure(ctx context.Context, scope envhost.ScopeRef, defs []envhost.Definition) (envhost.Handles, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ensure adapter children for scope %q: %w", scope.Key(), ctx.Err())
	}
	if len(defs) == 0 {
		return envhost.Handles{}, nil
	}
	if h.boot == nil {
		return nil, errors.New("container host has no child boot resolver configured")
	}
	// The in-process host has no Boot handshake: Ensure itself is the arm
	// point, so mark the runtime booted before the first child spawn.
	h.rt.mu.Lock()
	if !h.rt.booted {
		h.rt.booted = true
	}
	h.rt.mu.Unlock()
	settled := make(envhost.Handles, len(defs))
	for _, def := range defs {
		if def.Name == "" {
			return settled, fmt.Errorf("cannot ensure an unnamed adapter definition for scope %q", scope.Key())
		}
		w, err := h.ensureDefinition(ctx, scope, def)
		if err != nil {
			return settled, err
		}
		settled[def.Name] = w
	}
	return settled, nil
}

// ensureDefinition returns the live supervised handle for one
// (scope, definition), spawn included.
func (h *InProcessHost) ensureDefinition(ctx context.Context, scope envhost.ScopeRef, def envhost.Definition) (*supervisedHostHandle, error) {
	if _, err := h.resolveBootSpec(ctx, def); err != nil {
		return nil, err
	}
	c, ok := h.rt.ensureChildHandle(ctx, def.Name)
	if !ok {
		return nil, fmt.Errorf("adapter %q: no live child and no respawn possible for scope %q", def.Name, scope.Key())
	}
	return h.bindHandle(scope, def.Name, c.handle), nil
}

// resolveBootSpec resolves a definition's spawn contract through the boot
// hook once (first-wins) and registers it in both caches: the host's
// verbatim spec ledger and the runtime's boot-spec lane, which ensure's
// respawn and the freshen arm read. A definition without a RunnerFunc (not
// a container-mode adapter) fails closed here.
func (h *InProcessHost) resolveBootSpec(ctx context.Context, def envhost.Definition) (childSpec, error) {
	h.mu.Lock()
	spec, ok := h.bootSpecs[def.Name]
	h.mu.Unlock()
	if ok {
		return spec, nil
	}
	boot, err := h.boot(ctx, def)
	if err != nil {
		return childSpec{}, fmt.Errorf("resolve adapter %q container boot: %w", def.Name, err)
	}
	if boot.Runner == nil {
		return childSpec{}, fmt.Errorf("adapter %q: no container RunnerFunc; refusing to resolve a binary outside the declared boot contract", def.Name)
	}
	spec = childSpec{
		spec: AdapterSpec{
			Name:    def.Name,
			Version: boot.Version,
			Binary:  boot.Binary,
			Digest:  boot.Digest,
		},
		// Per-definition children serve every scope that engages the
		// definition; child process facts journal without a scope
		// attribution (the journal projection surfaces them to all
		// scope queries).
		scope:  "",
		runner: boot.Runner,
	}
	h.mu.Lock()
	if existing, exists := h.bootSpecs[def.Name]; exists {
		// A concurrent Ensure won the first resolution: reuse it verbatim.
		spec = existing
	} else {
		h.bootSpecs[def.Name] = spec
		h.mu.Unlock()
		h.rt.mu.Lock()
		if _, exists := h.rt.bootSpec[def.Name]; !exists {
			h.rt.bootSpec[def.Name] = spec
		}
		h.rt.mu.Unlock()
		return spec, nil
	}
	h.mu.Unlock()
	return spec, nil
}

// bindHandle returns the cached wrapper for (scope, definition), re-binding
// when the definition's live child changed (a clean-exit respawn replaced
// the process): the session manager must hold a handle whose inner child
// is the currently live one.
func (h *InProcessHost) bindHandle(scope envhost.ScopeRef, defName string, inner adapterhost.Handle) *supervisedHostHandle {
	key := hostHandleKey{scopeKey: scope.Key(), defName: defName}
	h.mu.Lock()
	defer h.mu.Unlock()
	if w := h.handles[key]; w != nil && w.Handle == inner {
		return w
	}
	w := &supervisedHostHandle{
		Handle:   inner,
		host:     h,
		defName:  defName,
		scopeKey: scope.Key(),
	}
	h.handles[key] = w
	return w
}

// addEngagement records one live session in the (scope, definition)
// engagement set.
func (h *InProcessHost) addEngagement(w *supervisedHostHandle, sessionID string) {
	if w == nil || sessionID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := hostHandleKey{scopeKey: w.scopeKey, defName: w.defName}
	if h.engagements[key] == nil {
		h.engagements[key] = map[string]struct{}{}
	}
	h.engagements[key][sessionID] = struct{}{}
}

// dropEngagement removes one live session from the (scope, definition)
// engagement set. Idempotent: session close paths race with close paths.
// When the definition's aggregate engagement across every scope drains to
// zero with child keepalive disabled, the child is killed as
// peer-initiated so the journal classifies the death as intentional.
func (h *InProcessHost) dropEngagement(w *supervisedHostHandle, sessionID string) {
	if w == nil || sessionID == "" {
		return
	}
	key := hostHandleKey{scopeKey: w.scopeKey, defName: w.defName}
	h.mu.Lock()
	if set := h.engagements[key]; set != nil {
		delete(set, sessionID)
		if len(set) == 0 {
			delete(h.engagements, key)
		}
	}
	remaining := h.liveEngagements(w.defName)
	keepAlive := h.rt.cfg == nil || h.rt.cfg.ChildKeepAlive
	h.mu.Unlock()
	if remaining > 0 || keepAlive {
		return
	}
	h.killChildPeerInitiated(w.defName)
}

// drainEngagement releases the wrapper's whole (scope, definition)
// engagement set — the id-less Kill arm, used by teardown flows that end a
// session without closing it. The keepalive-idle decision matches the
// per-id drop.
func (h *InProcessHost) drainEngagement(w *supervisedHostHandle) {
	if w == nil {
		return
	}
	key := hostHandleKey{scopeKey: w.scopeKey, defName: w.defName}
	h.mu.Lock()
	if set := h.engagements[key]; set != nil {
		delete(h.engagements, key)
	}
	remaining := h.liveEngagements(w.defName)
	keepAlive := h.rt.cfg == nil || h.rt.cfg.ChildKeepAlive
	h.mu.Unlock()
	if remaining == 0 && !keepAlive {
		h.killChildPeerInitiated(w.defName)
	}
}

// liveEngagements counts the definition's live engagements across every
// scope (caller holds h.mu).
func (h *InProcessHost) liveEngagements(defName string) int {
	n := 0
	for otherKey, set := range h.engagements {
		if otherKey.defName == defName {
			n += len(set)
		}
	}
	return n
}

// killChildPeerInitiated kills a definition's current child as a
// peer-initiated death (graceful journal classification; no crash fact).
// The child record is dropped as the kill is issued so ensure's next call
// takes the spawn path — the signal-death gate must never read this
// deliberate death as a crash and refuse a later Ensure to respawn it.
func (h *InProcessHost) killChildPeerInitiated(defName string) {
	rt := h.rt
	rt.mu.Lock()
	c := rt.children[defName]
	if c == nil || c.handle == nil || adapterhost.ProcessExited(c.handle) {
		rt.mu.Unlock()
		return
	}
	c.peerInitiated = true
	handle := c.handle
	delete(rt.children, defName)
	for i, n := range rt.order {
		if n == defName {
			rt.order = append(rt.order[:i], rt.order[i+1:]...)
			break
		}
	}
	rt.mu.Unlock()
	handle.Kill()
}

// Release implements envhost.Host: ends a scope's engagement per the
// declared fate policy. Release closes the scope's live sessions (the
// keepalive-idle decision follows each close); detach keeps engagements
// alive. Scope accept tokens are an in-process fiction — the host serves
// no listener and scopes re-adopt their definitions on the next Ensure —
// so the token-bearing arms have no persisted effect.
func (h *InProcessHost) Release(ctx context.Context, scope envhost.ScopeRef, policy envhost.FatePolicy) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if policy.Fate == envhost.FateDetach {
		h.log.Info("container host detached scope", "scope", scope.Key())
		return nil
	}
	for _, w := range h.scopeWrappers(scope.Key()) {
		for _, id := range h.wrapperSessionIDs(scope.Key(), w.defName) {
			closeCtx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
			err := w.Handle.CloseSession(closeCtx, id)
			cancel()
			if err != nil {
				h.log.Info("container host release close failed", "scope", scope.Key(),
					"adapter", w.defName, "session", id, "error", err)
			}
			h.dropEngagement(w, id)
		}
	}
	h.log.Info("container host released scope", "scope", scope.Key(), "fate", string(policy.Fate))
	return nil
}

// scopeWrappers snapshots the host wrappers engaged for one scope key, in
// deterministic definition order.
func (h *InProcessHost) scopeWrappers(scopeKey string) []*supervisedHostHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	ws := make([]*supervisedHostHandle, 0, len(h.handles))
	for key, w := range h.handles {
		if key.scopeKey == scopeKey {
			ws = append(ws, w)
		}
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].defName < ws[j].defName })
	return ws
}

// wrapperSessionIDs snapshots the live engagement ids held for one
// (scope, definition), in deterministic order.
func (h *InProcessHost) wrapperSessionIDs(scopeKey, defName string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.engagements[hostHandleKey{scopeKey: scopeKey, defName: defName}]
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Journal implements envhost.Host: a read-only projection of the shared
// peer supervision journal, filtered to the requested scope and definition
// — the host contract's spawn/exit/crash-classified truth is exactly what
// the runtime journaled for that child.
func (h *InProcessHost) Journal(scope envhost.ScopeRef, adapter string) envhost.JournalReader {
	return &hostJournalReader{host: h, scope: scope.Key(), adapter: adapter}
}

// hostJournalReader projects journal events into host entries in causal
// order. Heartbeats, stream flushes, and child-run arms are stream- and
// run-substrate facts, not process supervision truth, and are not
// surfaced.
type hostJournalReader struct {
	host    *InProcessHost
	scope   string
	adapter string
}

// Entries projects the retained journal ring in sequence order.
func (r *hostJournalReader) Entries(ctx context.Context) ([]envhost.JournalEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	events := r.host.rt.journal.Replay(0)
	scopeName, instance, _ := strings.Cut(r.scope, "/")
	entries := make([]envhost.JournalEntry, 0, len(events))
	for _, ev := range events {
		// A per-definition container child is shared across scopes, so
		// its child facts carry an empty attribution scope and are visible
		// to every scope query; scoped facts strict-match the query.
		if ev.GetScope() != "" && ev.GetScope() != r.scope {
			continue
		}
		if r.adapter != "" && ev.GetAdapterType() != r.adapter {
			continue
		}
		entry := envhost.JournalEntry{
			Scope:   envhost.ScopeRef{Scope: scopeName, Instance: instance},
			Adapter: ev.GetAdapterType(),
			At:      ev.GetAt().AsTime(),
		}
		switch kind := ev.Kind.(type) {
		case *criteriav1.SupervisionEvent_Spawned:
			s := kind.Spawned
			entry.Kind = envhost.JournalSpawned
			entry.Detail = fmt.Sprintf("spawned %q (digest %q, version %q, pid %d)",
				s.GetBinary(), s.GetDigest(), s.GetVersion(), s.GetPid())
		case *criteriav1.SupervisionEvent_Exited:
			x := kind.Exited
			entry.Kind = envhost.JournalExited
			entry.Detail = fmt.Sprintf("exited (exit_code %d, signal %d, graceful %t, idle_ms %d)",
				x.GetExitCode(), x.GetSignal(), x.GetGraceful(), x.GetIdleMs())
		case *criteriav1.SupervisionEvent_Crash:
			c := kind.Crash
			entry.Kind = envhost.JournalCrashClassified
			entry.Detail = fmt.Sprintf("%s: %s", c.GetReason(), c.GetDetail())
		default:
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// errHostNoListen is the deterministic ListenAddress rejection: an
// in-process host serves no listener.
var errHostNoListen = errors.New("in-process container host serves no listener")

// Control implements envhost.Host: the per-(scope, definition) verbs the
// engine's control plane and crash policy drive. Arms 1/2 are accepted
// no-ops (no listener, no accept tokens); arm 3 closes the scope's live
// sessions on the addressed definition; arm 5 displaces the stale child
// and returns a fresh live handle — the in-process lever behind
// crash-policy respawn.
func (h *InProcessHost) Control(ctx context.Context, req *envhost.ControlRequest) (envhost.ControlResponse, error) {
	if req == nil {
		return envhost.ControlResponse{}, errors.New("nil control request")
	}
	switch req.Kind {
	case envhost.ControlRegisterScope:
		// Accepted with no persisted effect: in-process hosts have no
		// accept tokens to rotate.
		return envhost.ControlResponse{}, nil
	case envhost.ControlUnregisterScope:
		return envhost.ControlResponse{}, nil
	case envhost.ControlCloseHandle:
		h.closeDefSessions(req.Scope.Key(), req.Adapter)
		return envhost.ControlResponse{}, nil
	case envhost.ControlListenAddress:
		return envhost.ControlResponse{}, fmt.Errorf("%w: request from scope %q", errHostNoListen, req.Scope.Key())
	case envhost.ControlEnsureFreshHandle:
		handle, err := h.freshenHandle(ctx, req.Scope, req.Adapter, req.Stale)
		if err != nil {
			return envhost.ControlResponse{}, err
		}
		return envhost.ControlResponse{Handle: handle}, nil
	default:
		return envhost.ControlResponse{}, fmt.Errorf("unsupported control kind %d", req.Kind)
	}
}

// closeDefSessions closes the scope's live sessions on one definition (an
// empty adapter identity closes every definition the scope engaged) and
// drains the engagements with the keepalive-idle decision.
func (h *InProcessHost) closeDefSessions(scopeKey, defName string) {
	for _, w := range h.scopeWrappers(scopeKey) {
		if defName != "" && w.defName != defName {
			continue
		}
		for _, id := range h.wrapperSessionIDs(scopeKey, w.defName) {
			closeCtx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
			err := w.Handle.CloseSession(closeCtx, id)
			cancel()
			if err != nil {
				h.log.Info("container host control close failed", "scope", scopeKey,
					"adapter", w.defName, "session", id, "error", err)
			}
		}
		h.drainScopeDefEngagement(scopeKey, w.defName)
	}
}

// drainScopeDefEngagement clears one (scope, definition) engagement set
// with the same keepalive-idle decision the per-id drop takes (the caller
// has already closed the sessions).
func (h *InProcessHost) drainScopeDefEngagement(scopeKey, defName string) {
	h.mu.Lock()
	delete(h.engagements, hostHandleKey{scopeKey: scopeKey, defName: defName})
	keepAlive := h.rt.cfg == nil || h.rt.cfg.ChildKeepAlive
	remaining := h.liveEngagements(defName)
	h.mu.Unlock()
	if remaining == 0 && !keepAlive {
		h.killChildPeerInitiated(defName)
	}
}

// freshHandlePollInterval is the bounded-wait granularity while a
// displaced child's exit fact settles.
const freshHandlePollInterval = 10 * time.Millisecond

// freshenHandle implements the EnsureFreshHandle arm: it returns a handle
// bound to a live child that is NOT the stale one named in the request.
// Host-killed children fail closed; a still-live stale child is displaced
// peer-initiated (graceful journal classification) with the exit wait
// bounded; a crashed or absent child is replaced by a fresh spawn from the
// definition's registered boot spec.
func (h *InProcessHost) freshenHandle(ctx context.Context, scope envhost.ScopeRef, defName string, stale envhost.Handle) (envhost.Handle, error) {
	if defName == "" {
		return nil, fmt.Errorf("ensure fresh handle requires a definition identity (scope %q)", scope.Key())
	}
	var staleInner adapterhost.Handle
	if w, ok := stale.(*supervisedHostHandle); ok && w != nil {
		staleInner = w.Handle
	}
	spec, err := h.resolveBootSpec(ctx, envhost.Definition{Name: defName})
	if err != nil {
		return nil, err
	}

	rt := h.rt
	rt.mu.Lock()
	c := rt.children[defName]
	if c != nil && c.killRequested {
		rt.mu.Unlock()
		return nil, fmt.Errorf("adapter %q: host-initiated kill in flight; no fresh handle", defName)
	}
	alive := c != nil && c.handle != nil && !adapterhost.ProcessExited(c.handle)
	alreadyFresh := alive && (staleInner == nil || c.handle != staleInner)
	rt.mu.Unlock()
	if alreadyFresh {
		// The runtime already settled a child different from the stale
		// one: adopt it without a second spawn.
		rt.mu.Lock()
		handle := c.handle
		rt.mu.Unlock()
		return h.bindHandle(scope, defName, handle), nil
	}
	if alive {
		// Displace the stale child: peer-initiated so the exit is recorded
		// as graceful (no crash fact), then spawn fresh once the death
		// settled.
		c.peerInitiated = true
		if !adapterhost.ProcessExited(c.handle) {
			c.handle.Kill()
		}
		if err := h.waitChildExit(c, defName); err != nil {
			return nil, err
		}
	}
	fresh, ok := rt.spawnAndWatch(ctx, &spec)
	if !ok {
		return nil, fmt.Errorf("adapter %q: fresh child spawn failed", defName)
	}
	return h.bindHandle(scope, defName, fresh.handle), nil
}

// waitChildExit blocks until the child's exit fact is journaled or the
// bounded wait expires (the watcher polls for the process death; a
// not-yet-settled death makes the arm fail so the caller can retry).
func (h *InProcessHost) waitChildExit(c *childState, defName string) error {
	rt := h.rt
	deadline := time.Now().Add(rt.shutdownGrace)
	for {
		rt.mu.Lock()
		exited := c.exited
		rt.mu.Unlock()
		if exited {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("adapter %q: displaced child did not exit within %s", defName, rt.shutdownGrace)
		}
		time.Sleep(freshHandlePollInterval)
	}
}

// StopEnvHost stops the in-process supervisor: live sessions are closed
// and children killed, peer-initiated at shutdown (graceful journal
// classification, no crash facts). The session manager calls this for
// hosts it owns; borrowed hosts are stopped by their owner.
func (h *InProcessHost) StopEnvHost(ctx context.Context) {
	h.stopOnce.Do(func() {
		if err := h.rt.Shutdown(ctx); err != nil {
			h.log.Info("container host shutdown", "error", err)
		}
	})
}

// Exit-flip settle cadence. The flip itself lands within milliseconds of
// transport death; the poll interval bounds how a settled query advances.
const exitFlipSettlePoll = 10 * time.Millisecond

// SupervisionCrashReason returns the crash classification the journal
// recorded for the definition's CURRENT child generation, if any — the
// classification the wrapper serves so classifySessionCrash consumes
// journal facts verbatim. The walk over the newest facts stops at the
// current generation's spawn marker: facts recorded before it belong to a
// displaced sibling child. When the child process has exited but the
// watcher has not journaled the exit yet, this first consumer query drives
// the fact into the journal synchronously (the exited-once guard makes the
// watcher race safe); ok=false otherwise (child alive, exited cleanly).
func (h *InProcessHost) SupervisionCrashReason(defName string, inner adapterhost.Handle) (string, bool) {
	if inner == nil {
		return "", false
	}
	rt := h.rt
	rt.mu.Lock()
	c := rt.children[defName]
	current := c != nil && c.handle == inner
	unrecorded := current && !c.exited && adapterhost.ProcessExited(inner)
	rt.mu.Unlock()
	if unrecorded {
		rt.recordExit(c, false)
	}
	if !current {
		return "", false
	}
	pid, ok := adapterhost.ProcessPID(inner)
	events := rt.journal.Replay(0)
	crashReason := ""
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.GetAdapterType() != defName {
			continue
		}
		if kind, isSpawned := ev.Kind.(*criteriav1.SupervisionEvent_Spawned); isSpawned {
			spawnedPID := int(kind.Spawned.GetPid())
			// The generation marker: stop when it belongs to a different
			// child process. When either side carries no pid (the journal
			// ring dropped the marker, or the handle reports none) the
			// retained newest facts stand for this child.
			if spawnedPID != 0 && ok && spawnedPID != pid {
				break
			}
			break
		}
		if kind, isCrash := ev.Kind.(*criteriav1.SupervisionEvent_Crash); isCrash && crashReason == "" {
			crashReason = kind.Crash.GetReason()
		}
	}
	if crashReason == "" {
		return "", false
	}
	return crashReason, true
}

// supervisedHostHandle is the envhost.Handles entry: the session manager's
// adapterhost.Handle wrapping the definition's live child. The adapter
// contract delegates to the inner child's handle (embedded interface,
// value fixed at construction — wrappers are immutable); the supervision
// capabilities are re-exposed so crash classification, process teardown
// facts, and raw-client interop behave as if the session held the child
// handle directly. It is deliberately NOT a PhoneHomeBridgeHandle:
// container children are not phone-home transport endpoints.
type supervisedHostHandle struct {
	// Handle is the definition's live child handle, EMBEDDED so the
	// adapter contract (Info/Execute/Pause/Resume/Inspect/Snapshot/
	// Restore) promotes unchanged; wrappers are immutable (a respawn
	// produces a new wrapper), so reads are race-free.
	adapterhost.Handle
	host     *InProcessHost
	defName  string
	scopeKey string
}

// OpenSession delegates and records the session's engagement.
func (w *supervisedHostHandle) OpenSession(ctx context.Context, id string, config, secrets map[string]string) error {
	err := w.Handle.OpenSession(ctx, id, config, secrets)
	if err == nil {
		w.host.addEngagement(w, id)
	}
	return err
}

// CloseSession delegates and drops the engagement (also on the error path:
// a session is dead with a crashed child).
func (w *supervisedHostHandle) CloseSession(ctx context.Context, id string) error {
	err := w.Handle.CloseSession(ctx, id)
	w.host.dropEngagement(w, id)
	return err
}

// Kill ends the SESSION, never the child: the child serves sibling
// engagements and this host owns child lifetime (keepalive, Control arms,
// crash policy, shutdown). It mirrors the remote handle's kill semantics,
// where killing the connection is the session manager's lever. The id-less
// engagement drain keeps the keepalive-idle decision honest for teardown
// flows that skip CloseSession.
func (w *supervisedHostHandle) Kill() {
	w.host.drainEngagement(w)
}

// SupervisionCrashReason serves the journal-backed classification for the
// definition's current child generation.
func (w *supervisedHostHandle) SupervisionCrashReason() (string, bool) {
	return w.host.SupervisionCrashReason(w.defName, w.Handle)
}

// SettledSupervisionCrashReason is the classification query on a known
// transport failure (JournalCrashSettler seam): the journal's exit fact
// rides the child client's process-exited flip, which lands strictly
// after the transport dies, so the plain query can miss a kill in flight.
// This entry settles the flip within the ctx deadline — pushing the
// exit fact through the exited-once-guarded recordExit — and classifies
// identically to SupervisionCrashReason.
func (w *supervisedHostHandle) SettledSupervisionCrashReason(ctx context.Context) (string, bool) {
	if !w.host.settleExitFlip(w.defName, w.Handle, ctx) {
		return w.host.SupervisionCrashReason(w.defName, w.Handle)
	}
	return w.host.SupervisionCrashReason(w.defName, w.Handle)
}

// settleExitFlip bridges the window between transport death and the child
// client's process-exited flip for the definition's current generation:
// the journal's exit/crash facts ride that flip. It waits until the flip
// lands or the ctx deadline passes; the budget must belong to a caller
// that already knows the transport failed, so healthy-child queries
// classify instantly without this entry. Returns whether the flip is
// believed settled (flipped, or the runtime already pushed the fact).
func (h *InProcessHost) settleExitFlip(defName string, inner adapterhost.Handle, ctx context.Context) bool {
	if inner == nil {
		return false
	}
	rt := h.rt
	rt.mu.Lock()
	c := rt.children[defName]
	current := c != nil && c.handle == inner
	rt.mu.Unlock()
	if !current {
		return false
	}
	settled := false
	for {
		rt.mu.Lock()
		exited := c.exited
		rt.mu.Unlock()
		if exited || adapterhost.ProcessExited(inner) {
			settled = true
			break
		}
		select {
		case <-ctx.Done():
			return settled
		case <-time.After(exitFlipSettlePoll):
		}
	}
	return settled
}

// Client exposes the raw adapter v2 client of the live child for transports
// that re-expose the adapter contract (the phone-home runner and peer
// interop paths).
func (w *supervisedHostHandle) Client() adapterhost.Client {
	if c, ok := adapterhost.ClientOf(w.Handle); ok {
		return c
	}
	return nil
}

// ProcessExited reports whether the wrapped child process has exited
// (teardown-fact consumers read it through the same capability the child
// handle itself implements).
func (w *supervisedHostHandle) ProcessExited() bool {
	return adapterhost.ProcessExited(w.Handle)
}

// ProcessWaitStatus reports the wrapped child's OS wait status facts.
func (w *supervisedHostHandle) ProcessWaitStatus() (exitCode, signal int, ok bool) {
	return adapterhost.ProcessWaitStatus(w.Handle)
}

// StartLogStream delegates the per-session log stream to the live child so
// container sessions keep their log streaming after being wrapped.
func (w *supervisedHostHandle) StartLogStream(ctx context.Context, sessionID string, sink adapterhost.LogEventSink) (func(), <-chan error, error) {
	starter, ok := w.Handle.(adapterhost.LogStreamStarter)
	if !ok {
		return nil, nil, fmt.Errorf("adapter %q: child handle does not stream logs", w.defName)
	}
	return starter.StartLogStream(ctx, sessionID, sink)
}

// StartPermissionStream delegates the per-session permission stream to the
// live child.
func (w *supervisedHostHandle) StartPermissionStream(ctx context.Context, sessionID string, requests <-chan *v2.PermissionEvent) (func(), error) {
	streamer, ok := w.Handle.(adapterhost.PermissionStreamer)
	if !ok {
		return nil, fmt.Errorf("adapter %q: child handle does not stream permissions", w.defName)
	}
	return streamer.StartPermissionStream(ctx, sessionID, requests)
}

// promptCapable is the locally-declared capability the child handle's
// Prompt method is delegated through (adapterhost keeps its
// corresponding discovery interface unexported).
type promptCapable interface {
	Prompt(ctx context.Context, req *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error)
}

// Prompt delegates host-driven prompts to the live child.
func (w *supervisedHostHandle) Prompt(ctx context.Context, req *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	capable, ok := w.Handle.(promptCapable)
	if !ok {
		return nil, fmt.Errorf("adapter %q: child handle does not support prompts", w.defName)
	}
	return capable.Prompt(ctx, req)
}
