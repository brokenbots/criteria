package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// defaultExitPollInterval is how often the peer polls a child's go-plugin
// client for process exit.
const defaultExitPollInterval = 500 * time.Millisecond

// closeSessionTimeout bounds each per-session CloseSession during shutdown so
// one stuck child cannot stall the whole sequence.
const closeSessionTimeout = 5 * time.Second

// defaultControlGrace is the grace period a Control(kill_child) waits before
// killing the child when the request carries no explicit grace.
const defaultControlGrace = 5 * time.Second

// childState is one supervised adapter child: a go-plugin subprocess hosting
// one adapter type. Every scope session of that adapter type rides inside it
// across the environment's phone-home connections: the child is
// keyed by adapter type, not by (scope, adapter) dial.
type childState struct {
	name string
	// scope is the journal attribution scope for the child's process facts:
	// the dial scope of the manifest that declared it, or "" when the child
	// serves a whole scope set.
	scope string

	handle adapterhost.Handle

	binary  string
	version string
	digest  string

	// killRequested is set the moment a host Control(kill_child) is accepted
	// for this child, so an exit observed by the watcher before the kill
	// lands is still classified as peer-initiated (graceful, no crash).
	killRequested bool
	// peerInitiated is set when THIS runtime decides the child dies
	// deliberately outside a Control arm — the host binding's
	// keepalive-idle kill and a fresh-handle displacement. The watcher
	// classifies those deaths as intentional (graceful, no crash fact).
	peerInitiated bool
	exited        bool
	// exitFactsJournaled is the "exit facts journaled" settle signal: set
	// under the runtime lock only AFTER the Exited append and — for an
	// ungraceful exit — after the CrashClassified append. Waiters keyed on
	// `exited` alone could observe a settled death before the journal held
	// its terminal facts, letting a successor's Spawned marker interleave
	// ahead of the stale generation's facts.
	exitFactsJournaled bool
	lastExit           *criteriav1.ProcessExited
	lastEventAt        time.Time

	// exitObserved wakes the child's exit watcher out of its poll tick: a
	// child's death first surfaces to a consumer of its client (a failed
	// Execute or Log call) rather than to the watcher's next tick, and the
	// terminal journal facts must be on the phone-home stream before the
	// host's failing call classifies. Nil in tests that build states
	// directly — a nil channel select arm simply never fires, leaving the
	// poll as the only driver.
	exitObserved chan struct{}

	// exitJournalGate is a test seam: when set (by tests, under the runtime
	// lock), recordExit calls it after releasing the lock and BEFORE the
	// Exited append, letting a test hold the exit facts unjournaled while
	// exercising a successor-spawn arm deterministically.
	exitJournalGate func(*childState)

	// openSessions tracks the sessions the host opened through the served
	// bridge of this child; they are closed before the shutdown kill.
	openSessions map[string]struct{}
	// servedChild is the adapter client the phone-home bridge serves for
	// this child (the local child in production; a fixture stub in tests).
	// Shutdown closes tracked sessions through it.
	servedChild adapterhost.Client
}

// peerRuntime is the peer's supervision state: the resolved configuration,
// the go-plugin loader owning the children, the per-adapter child table, the
// bounded supervision journal, and the most recent process-exit facts.
type peerRuntime struct {
	cfg     *Config
	log     *slog.Logger
	loader  *adapterhost.DefaultLoader
	journal *EventJournal

	mu            sync.Mutex
	shuttingDown  bool
	booted        bool
	children      map[string]*childState
	order         []string
	serveCounts   map[string]int
	shutdownGrace time.Duration
	// bootSpec preserves each child's resolved boot spec so ensureChild can
	// respawn an exited child verbatim (no re-resolution, same digest pin).
	bootSpec map[string]childSpec
	// idleReapPolicy records the Control IdleReap declarations per child
	// name (ADR-0014 M6): the keepalive/idle window reap consumers apply for
	// that child. Keyed by name, the policy survives respawns. A missing
	// entry (or a zero window) means no declared policy.
	idleReapPolicy map[string]time.Duration
	// respawnMu serializes respawns per definition name.
	respawnMu sync.Map
	// exitPoll is the process-exit poll interval (tunable in tests).
	exitPoll  time.Duration
	stopWatch chan struct{}
	// watchDone closes once every child exit watcher has stopped: the
	// first drain of all registered watchers closes it, and when no
	// watcher was ever registered it stays open (nothing waits on it).
	// The channel is never swapped — consumers read it directly.
	watchDone chan struct{}
	watchMu   sync.Mutex
	// The watcher bookkeeping is lock- and condition-guarded because a
	// watcher may be registered from the respawn path at any time, even
	// while a drain is in flight: registration is refused once shutdown
	// begins, and the drain waits for watchers registered before that.
	// (A WaitGroup cannot accept a counter increment concurrent with a
	// Wait, which is what the respawn path would need.)
	watchCond       *sync.Cond
	watchActive     int
	watchClosed     bool
	watchDoneClosed bool
	stopOnce        sync.Once
}

// NewRuntime builds a peer runtime for the given (already resolved)
// configuration. The runtime takes ownership of cfg.
func NewRuntime(cfg *Config, log *slog.Logger) *peerRuntime {
	if log == nil {
		log = slog.Default()
	}
	rt := &peerRuntime{
		cfg:            cfg,
		log:            log,
		loader:         adapterhost.NewLoader(),
		journal:        NewEventJournal(cfg.JournalLimit),
		children:       map[string]*childState{},
		bootSpec:       map[string]childSpec{},
		idleReapPolicy: map[string]time.Duration{},
		serveCounts:    map[string]int{},
		shutdownGrace:  defaultControlGrace,
		exitPoll:       defaultExitPollInterval,
		stopWatch:      make(chan struct{}),
		watchDone:      make(chan struct{}),
	}
	rt.watchCond = sync.NewCond(&rt.watchMu)
	return rt
}

// registerExitWatcher reserves a live-watcher slot and reports whether the
// reservation was accepted: once shutdown has begun no watcher may
// register, so the caller must not spawn an unwatched child on refusal.
func (r *peerRuntime) registerExitWatcher() bool {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if r.watchClosed {
		return false
	}
	r.watchActive++
	return true
}

// releaseExitWatcher drops a finished watcher's slot; every transition to
// a zero count wakes a shutdown drain in flight — the drain's predicate is
// the zero count, and later watcher generations must wake it too, not
// just the one that first closed watchDone.
func (r *peerRuntime) releaseExitWatcher() {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	r.watchActive--
	if r.watchActive == 0 {
		if !r.watchDoneClosed {
			r.watchDoneClosed = true
			close(r.watchDone)
		}
		r.watchCond.Broadcast()
	}
}

// drainExitWatchers blocks until every registered watcher has stopped.
// Callers invoke it under watchClosed — set before the drain begins — so
// no watcher can register after the count reaches zero.
func (r *peerRuntime) drainExitWatchers() {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	for r.watchActive > 0 {
		r.watchCond.Wait()
	}
}

// closeWatcherRegistration refuses all future watcher registrations; it is
// the drain's precondition. Watchers registered before this point are
// awaited by drainExitWatchers.
func (r *peerRuntime) closeWatcherRegistration() {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	r.watchClosed = true
}

// Config returns the runtime configuration.
func (r *peerRuntime) Config() *Config { return r.cfg }

// Journal returns the supervision journal for the Supervise stream.
func (r *peerRuntime) Journal() *EventJournal { return r.journal }

// Child returns the primary (first spawned) child handle, or nil before Boot
// succeeds. The legacy single-adapter shape has exactly one child.
func (r *peerRuntime) Child() adapterhost.Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.childLocked()
}

// childLocked is Child without taking the runtime lock (caller holds it).
func (r *peerRuntime) childLocked() adapterhost.Handle {
	if c := r.primaryChildLocked(); c != nil {
		return c.handle
	}
	return nil
}

// primaryChildLocked returns the first spawned child state.
func (r *peerRuntime) primaryChildLocked() *childState {
	if len(r.order) == 0 {
		return nil
	}
	return r.children[r.order[0]]
}

// LastExit returns the primary child's most recent process-exit fact, or nil
// while it is running.
func (r *peerRuntime) LastExit() *criteriav1.ProcessExited {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.primaryChildLocked(); c != nil {
		return c.lastExit
	}
	return nil
}

// ChildNames returns the hosted adapter names in spawn order.
func (r *peerRuntime) ChildNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// ChildHandle returns the handle of the child hosting one adapter type, or
// nil when the peer does not host it.
func (r *peerRuntime) ChildHandle(name string) adapterhost.Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.children[name]; c != nil {
		return c.handle
	}
	return nil
}

// childClientFor resolves the phone-home bridge client for one hosted
// adapter child (in-memory handles carry no client).
func (r *peerRuntime) childClientFor(name string) (adapterhost.Client, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.children[name]
	if c == nil || c.handle == nil {
		return nil, false
	}
	return adapterhost.ClientOf(c.handle)
}

// ensureChild returns a live client for one hosted adapter child, spawning
// it on demand from its boot spec when the child process has exited but its
// definition is still hosted. Copilot children idle-exit once their serving
// sessions close (canary evidence: exit_code=0 after a session
// close), and a boot-complete per-environment manifest hosts definitions
// whose first dial lands long after boot: exit-only semantics turned "an
// idle period" into a permanent loss of the child. The respawn reuses the
// boot spec verbatim — same binary path (the staged per-kind copy inside
// the pod's own emptyDir), same per-definition digest pin, no new
// resolution, no shared filesystem. Fail-closed: only boot-manifest (or
// legacy single-adapter) definitions respawn, never while a shutdown or a
// host-initiated kill is in flight, and the dialing host digest-verifies
// every identity frame at accept time exactly as it did for the boot spawn.
func (r *peerRuntime) ensureChild(ctx context.Context, name string) (adapterhost.Client, bool) {
	c, ok := r.ensureChildHandle(ctx, name)
	if !ok {
		return nil, false
	}
	return adapterhost.ClientOf(c.handle)
}

// ensureChildHandle is ensureChild's state-shape twin: the host binding
// needs the live child state (its raw handle, exit facts, kill bookkeeping)
// rather than the adapter client, so dial-mode Ensure keeps its client
// contract by wrapping this.
func (r *peerRuntime) ensureChildHandle(ctx context.Context, name string) (*childState, bool) {
	// Serialize respawns per definition name: two dialers racing on the same
	// dead child must not spawn two processes.
	if m, _ := r.respawnMu.LoadOrStore(name, &sync.Mutex{}); m != nil {
		m.(*sync.Mutex).Lock()
		defer m.(*sync.Mutex).Unlock()
	}

	r.mu.Lock()
	if r.shuttingDown || !r.booted {
		r.mu.Unlock()
		return nil, false
	}
	if c := r.children[name]; c != nil {
		if c.killRequested {
			// A host Command(kill_child) decided this child dies; it stays
			// dead — no client for new sessions, no respawn, even while the
			// grace-window process technically lingers.
			r.mu.Unlock()
			return nil, false
		}
		if c.handle != nil && !adapterhost.ProcessExited(c.handle) {
			r.mu.Unlock()
			return c, true
		}
		// Clean exits respawn (adapter lifecycle choice: copilot idle-exit
		// exit_code=0/signal=0). A crash — signal death or nonzero code —
		// never respawns: the step's crash policy owns recovery.
		if c.lastExit == nil || c.lastExit.GetSignal() != 0 || c.lastExit.GetExitCode() != 0 {
			r.mu.Unlock()
			return nil, false
		}
	}
	spec, ok := r.bootSpec[name]
	r.mu.Unlock()
	if !ok {
		return nil, false
	}

	// Lock-free spawn: recordChild/verifyChild/recordSpawn manage r.mu
	// internally (Boot's spawn loop runs unlocked the same way).
	return r.spawnAndWatch(ctx, &spec)
}

// spawnAndWatch spawns one child from a spec and starts its exit watcher.
// The ensure path (dial peers and the in-process container host) and the
// boot loop share it.
func (r *peerRuntime) spawnAndWatch(ctx context.Context, spec *childSpec) (*childState, bool) {
	// The watcher slot is reserved before the spawn: a running child's
	// exit must be journaled, a failed spawn must not leak the slot, and a
	// refused reservation (shutdown in flight) must not leave an unwatched
	// child behind.
	if !r.registerExitWatcher() {
		return nil, false
	}
	c, err := r.spawnChild(ctx, spec)
	if err != nil {
		r.releaseExitWatcher()
		r.log.Error("peer child respawn failed", "adapter", spec.spec.Name, "error", err)
		return nil, false
	}
	if err := r.recordSpawn(c); err != nil {
		r.log.Error("peer child respawn journal append failed", "adapter", spec.spec.Name, "error", err)
	}
	go r.watchChild(c)
	r.log.Info("peer child respawned", "adapter", spec.spec.Name,
		"binary", spec.spec.Binary, "digest", spec.spec.Digest)
	return c, true
}

// childSpec is one boot entry: the manifest-derived adapter spec plus the
// journal attribution scope for the child's process facts.
type childSpec struct {
	spec AdapterSpec
	// scope is the journal attribution scope for the child's process facts;
	// "" when the child serves a whole scope set (per-scope dial mode).
	scope string
	// runner overrides binary discovery: when set, spawnChild hands the
	// go-plugin RunnerFunc that starts the child (the in-process container
	// host's container runner or a test injection lane) instead of
	// resolving the adapter binary under CRITERIA_ADAPTERS.
	runner ChildRunner
}

// childSpecs resolves the boot list: the multi-adapter manifest when
// declared, else the single legacy adapter. Manifest entries are one per
// adapter DEFINITION: each definition name spawns its own child
// process even when several definitions share an adapter type (the old
// kind-deduped grammar collapsed them into one child). With
// CRITERIA_REMOTE_SCOPES_DIR set the hosted children still cover the
// adapter set of the manifest (or the single legacy adapter); the scope
// tokens arrive per (scope, adapter) at serve time.
func (r *peerRuntime) childSpecs() ([]childSpec, error) {
	if r.cfg.ManifestMode() {
		if len(r.cfg.Adapters) == 0 {
			return nil, errors.New("no adapter declared in the multi-adapter manifest")
		}
		out := make([]childSpec, 0, len(r.cfg.Adapters))
		for i := range r.cfg.Adapters {
			out = append(out, childSpec{spec: r.cfg.Adapters[i], scope: r.cfg.Scope})
		}
		return out, nil
	}
	if r.cfg.ScopesDir != "" {
		// Scope-token dial mode without a manifest: the hosted child set
		// comes from the legacy single-adapter resolution. The binary is
		// resolved by Config.Resolve / the loader, name-presence is the
		// fail-closed gate here.
		if r.cfg.AdapterName == "" {
			return nil, fmt.Errorf("%s requires a hosted adapter set (set %s, or the legacy CRITERIA_ADAPTER_NAME/CRITERIA_ADAPTER_BINARY)",
				EnvRemoteScopesDir, EnvAdapters)
		}
		return []childSpec{{
			spec: AdapterSpec{
				Name:     r.cfg.AdapterName,
				Version:  r.cfg.AdapterVersion,
				Binary:   r.cfg.AdapterBinary,
				Digest:   r.cfg.Digest,
				Manifest: r.cfg.AdapterManifest,
			},
		}}, nil
	}
	if r.cfg.AdapterName == "" {
		return nil, errors.New("adapter name not resolved; set CRITERIA_ADAPTER_NAME or CRITERIA_ADAPTER_BINARY")
	}
	return []childSpec{{
		spec: AdapterSpec{
			Name:     r.cfg.AdapterName,
			Version:  r.cfg.AdapterVersion,
			Binary:   r.cfg.AdapterBinary,
			Digest:   r.cfg.Digest,
			Manifest: r.cfg.AdapterManifest,
		},
		scope: r.cfg.Scope,
	}}, nil
}

// Boot spawns every adapter child of the manifest, verifies each with Info,
// records the spawns into the journal, and starts the exit watchers.
// Children are real go-plugin subprocesses launched through the adapterhost
// loader; their environment is scrubbed of every CRITERIA_REMOTE_* variable
// (ChildEnv) plus the multi-adapter manifest variables so they cannot sniff
// into phone-home mode. A failed spawn tears down the children already
// spawned — a multi-child peer never half-boots. Boot must be called once.
func (r *peerRuntime) Boot(ctx context.Context) error {
	r.mu.Lock()
	if r.booted {
		r.mu.Unlock()
		return errors.New("peer already booted")
	}
	if r.shuttingDown {
		r.mu.Unlock()
		return errors.New("peer already shut down")
	}
	r.booted = true
	r.mu.Unlock()

	specs, err := r.childSpecs()
	if err != nil {
		return err
	}
	// Preserve the resolved boot specs: ensureChild respawns exited children
	// from these verbatim (no re-resolution, same digest pin).
	for i := range specs {
		r.bootSpec[specs[i].spec.Name] = specs[i]
	}

	spawned := make([]*childState, 0, len(specs))
	for i := range specs {
		// The watcher slot is reserved before the spawn: a refused slot
		// (shutdown in flight) must not leave an unwatched child, so the
		// boot fails through the normal teardown path.
		if !r.registerExitWatcher() {
			r.teardownSpawnedChildren(ctx, spawned)
			return fmt.Errorf("adapter %q: refusing unwatched boot spawn during shutdown", specs[i].spec.Name)
		}
		c, err := r.spawnChild(ctx, &specs[i])
		if err != nil {
			r.releaseExitWatcher()
			r.teardownSpawnedChildren(ctx, spawned)
			return err
		}
		if err := r.recordSpawn(c); err != nil {
			spawned = append(spawned, c)
			r.releaseExitWatcher()
			r.teardownSpawnedChildren(ctx, spawned)
			return err
		}
		spawned = append(spawned, c)
		go r.watchChild(c)
	}
	return nil
}

// teardownSpawnedChildren kills the subprocesses spawned so far during a
// failed boot so a failed boot never orphans them.
func (r *peerRuntime) teardownSpawnedChildren(ctx context.Context, spawned []*childState) {
	for _, c := range spawned {
		if c.handle != nil && !adapterhost.ProcessExited(c.handle) {
			c.handle.Kill()
		}
	}
	if len(spawned) > 0 {
		if err := r.loader.Shutdown(ctx); err != nil {
			r.log.Warn("loader shutdown after failed boot", "error", err)
		}
	}
}

// recordSpawn journals the ProcessSpawned fact and logs the spawn line.
func (r *peerRuntime) recordSpawn(c *childState) error {
	pid, _ := adapterhost.ProcessPID(c.handle)
	spawned, appendErr := r.journal.Append(&criteriav1.SupervisionEvent_Spawned{Spawned: &criteriav1.ProcessSpawned{
		Binary:  c.binary,
		Digest:  c.digest,
		Version: c.version,
		Pid:     int32(pid),
	}}, c.name, c.scope, "")
	if appendErr != nil {
		return appendErr
	}

	c.lastEventAt = time.Now()

	r.log.Info("peer child spawned",
		"adapter", c.name,
		"binary", c.binary,
		"digest", c.digest,
		"version", c.version,
		"pid", pid,
		"scope", c.scope,
		"child_keepalive", r.cfg.ChildKeepAlive,
		"event_seq", spawned.GetEventSeq(),
	)
	return nil
}

// spawnChild launches the adapter child through the adapterhost loader and
// verifies it with Info. On verification failure it journals the crash,
// tears the child down immediately, and removes the registration so a failed
// boot never leaves a half-registered child behind.
func (r *peerRuntime) spawnChild(ctx context.Context, spec *childSpec) (*childState, error) {
	s := &spec.spec
	if spec.runner != nil {
		// Runner-injected spawn (in-process container host / tests): the
		// RunnerFunc owns binary+env+identity; discovery is skipped so the
		// CRITERIA_ADAPTERS lookup cannot leak a wrong-identity binary in.
		child, err := r.loader.ResolveWithRunnerFunc(ctx, s.Name, spec.runner)
		if err != nil {
			return nil, fmt.Errorf("start adapter %q: %w", s.Name, err)
		}
		return r.spawnVerified(ctx, spec, child)
	}
	// The discovery function pins the loader to the resolved spec binary so
	// the resolution precedence (manifest → env → PATH → digest preference)
	// stays in Config.Resolve and ResolveAdapterSpec.
	discovery := func(string) (string, error) {
		if s.Binary == "" {
			return "", errors.New("adapter binary not resolved; set CRITERIA_ADAPTER_BINARY")
		}
		return s.Binary, nil
	}
	childEnv := ChildEnv(os.Environ())
	customizer := func(_ string, cmd *exec.Cmd) {
		cmd.Env = childEnv
	}
	child, err := r.loader.ResolveWithDiscovery(ctx, s.Name, discovery, customizer)
	if err != nil {
		return nil, fmt.Errorf("start adapter %q: %w", s.Name, err)
	}
	return r.spawnVerified(ctx, spec, child)
}

// spawnVerified registers and Info-verifies a freshly started child.
func (r *peerRuntime) spawnVerified(ctx context.Context, spec *childSpec, child adapterhost.Handle) (*childState, error) {
	c := r.recordChild(spec, child)
	if err := r.verifyChild(ctx, spec, c, child); err != nil {
		return nil, err
	}
	return c, nil
}

// recordChild registers the freshly started child in the runtime registry
// and spawn order (deduplicated: a respawned child re-registers its state
// without duplicating the spawn order).
func (r *peerRuntime) recordChild(spec *childSpec, child adapterhost.Handle) *childState {
	s := &spec.spec
	c := &childState{
		name:         s.Name,
		scope:        spec.scope,
		handle:       child,
		binary:       s.Binary,
		digest:       s.Digest,
		openSessions: map[string]struct{}{},
		exitObserved: make(chan struct{}, 1),
	}
	r.mu.Lock()
	existed := false
	for _, n := range r.order {
		if n == s.Name {
			existed = true
			break
		}
	}
	r.children[s.Name] = c
	if !existed {
		r.order = append(r.order, s.Name)
	}
	r.mu.Unlock()
	return c
}

// verifyChild confirms the child answers Info right after spawn; anything
// else journals a crash, deregisters the child, and kills the process.
func (r *peerRuntime) verifyChild(ctx context.Context, spec *childSpec, c *childState, child adapterhost.Handle) error {
	s := &spec.spec
	info, err := child.Info(ctx)
	if err != nil {
		r.log.Error("adapter child failed Info", "adapter", s.Name, "error", err)
		if _, jerr := r.journal.Append(&criteriav1.SupervisionEvent_Crash{Crash: &criteriav1.CrashClassified{
			Reason: adapterhost.CrashReasonProcessExitedEarly,
			Detail: fmt.Sprintf("Info failed right after spawn: %v", err),
		}}, s.Name, spec.scope, ""); jerr != nil {
			r.log.Error("journal crash event", "error", jerr)
		}
		r.mu.Lock()
		delete(r.children, s.Name)
		for i, n := range r.order {
			if n == s.Name {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
		r.mu.Unlock()
		child.Kill()
		return fmt.Errorf("adapter %q Info: %w", s.Name, err)
	}
	version := info.Version
	if version == "" {
		version = s.Version
	}
	c.version = version
	return nil
}

// childExitNudge wakes the named child's exit watcher out of its poll
// tick: the child's death surfaces first as the transport failure of the
// call riding its client, and the watcher otherwise observes it up to
// one poll tick later. Harmless when the child is alive (the watcher
// re-checks the process state before recording anything) and when no
// matching child is hosted; the nudge is lossy by design (a wake that
// races the poll tick costs an interval, not a fact).
func (r *peerRuntime) childExitNudge(name string) {
	r.mu.Lock()
	c, ok := r.children[name]
	r.mu.Unlock()
	if !ok || c.exitObserved == nil {
		return
	}
	select {
	case c.exitObserved <- struct{}{}:
	default:
	}
}

// observeChildDeath records the child's exit fact the moment the child is
// observed exited. c.handle is swapped to nil under r.mu by the respawn
// path (ensureChild) and by tests; the watcher must read it under the
// same lock. Reports whether the child was recorded as exited (the
// watcher's terminal step).
func (r *peerRuntime) observeChildDeath(c *childState) bool {
	r.mu.Lock()
	handle := c.handle
	r.mu.Unlock()
	if handle != nil && adapterhost.ProcessExited(handle) {
		r.recordExit(c, false)
		return true
	}
	return false
}

// watchChild watches one child's process state and journals the exit fact
// the moment it disappears. The observation is event-driven where the
// child's transport surfaces it (exitObserved nudges) and poll-driven as
// the fallback, so a kill that races an in-flight call never waits out a
// whole poll tick. A peer-initiated shutdown stops the watcher first, so
// exits observed here are always ungraceful crashes.
func (r *peerRuntime) watchChild(c *childState) {
	defer r.releaseExitWatcher()
	ticker := time.NewTicker(r.exitPoll)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopWatch:
			return
		case <-c.exitObserved:
			if r.observeChildDeath(c) {
				return
			}
		case <-ticker.C:
			if r.observeChildDeath(c) {
				return
			}
		}
	}
}

// recordExit journals the terminal ProcessExited fact exactly once per
// child. When the peer did not initiate the shutdown the exit is a crash:
// the reason comes from the shared adapterhost crash taxonomy (peer.proto
// CrashClassified consumes that vocabulary). The exit code and signal are
// the child's real OS wait status once it has been reaped
// (adapterhost.ProcessWaitStatus); when no wait status is available the
// unknown-exit fallback (-1, 0) matches the ProcessExited contract.
func (r *peerRuntime) recordExit(c *childState, peerInitiated bool) {
	if c.handle == nil {
		// Nothing was ever spawned: no process-exit fact exists.
		return
	}
	r.mu.Lock()
	if c.exited {
		r.mu.Unlock()
		return
	}
	c.exited = true
	graceful := peerInitiated || c.peerInitiated || r.shuttingDown || c.killRequested
	idleMS := uint64(0)
	if !c.lastEventAt.IsZero() {
		idleMS = uint64(time.Since(c.lastEventAt).Milliseconds())
	}
	c.lastEventAt = time.Now()
	var exitGate func(*childState)
	if c.exitJournalGate != nil {
		exitGate = c.exitJournalGate
	}
	r.mu.Unlock()

	exitCode, signal, ok := adapterhost.ProcessWaitStatus(c.handle)
	if !ok {
		exitCode, signal = -1, 0
	}
	exit := &criteriav1.ProcessExited{
		ExitCode: int32(exitCode),
		Signal:   int32(signal),
		IdleMs:   idleMS,
		Graceful: graceful,
	}
	r.mu.Lock()
	c.lastExit = exit
	r.mu.Unlock()

	if exitGate != nil {
		exitGate(c)
	}
	if _, err := r.journal.Append(&criteriav1.SupervisionEvent_Exited{Exited: exit}, c.name, c.scope, ""); err != nil {
		r.log.Error("journal exited event", "error", err)
	}
	if !graceful {
		r.classifyUnexpectedExit(c, exitCode, signal)
	}
	r.mu.Lock()
	c.exitFactsJournaled = true
	r.mu.Unlock()
}

// classifyUnexpectedExit journals the CrashClassified event for an
// ungraceful child exit, classifying against the shared adapterhost crash
// taxonomy with the real wait-status facts in the detail.
func (r *peerRuntime) classifyUnexpectedExit(c *childState, exitCode, signal int) {
	r.log.Error("adapter child exited unexpectedly",
		"adapter", c.name,
		"reason", adapterhost.CrashReasonProcessTerminated,
		"exit_code", exitCode,
		"signal", signal,
	)
	if _, err := r.journal.Append(&criteriav1.SupervisionEvent_Crash{Crash: &criteriav1.CrashClassified{
		Reason: adapterhost.CrashReasonProcessTerminated,
		Detail: fmt.Sprintf("adapter child exited while supervised (exit code %d, signal %d)", exitCode, signal),
	}}, c.name, c.scope, ""); err != nil {
		r.log.Error("journal crash event", "error", err)
	}
}

// hostedChildName resolves the child name the Control verbs address: the
// child named in the request, or the primary child for legacy (empty)
// addressing. It reads the child table without touching process state.
// ok=false when the peer hosts nothing the request matches.
func (r *peerRuntime) hostedChildName(name string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" {
		if p := r.primaryChildLocked(); p != nil {
			return p.name, true
		}
		return "", false
	}
	if c := r.children[name]; c != nil {
		return name, true
	}
	return "", false
}

// Control executes a host-initiated control action (the PeerService.Control
// RPC). kill_child acknowledges immediately, waits out the requested grace
// period (default 5s, host sends 3s), then kills the addressed child; the
// watcher journals the exit fact, classified as peer-initiated via
// killRequested. The addressed child is the one named by the request's
// adapter_type — the host addresses it by adapter DEFINITION identity;
// an empty adapter_type addresses the primary child (legacy
// single-adapter hosts). cancel_child_run (ADR-0008,
// workflow.v1-gated at the server) is a known arm but there is no child-run
// substrate in the peer role: nothing to execute a run-level cancellation
// against, so it is rejected with Accepted=false. The ADR-0014 M6
// session-control arms (RespawnChild, IdleReap — session.v1-gated at the
// server) execute here (the ledger verbs OpenSession/CloseSession are
// answered at the server, before the runtime is reached). Unsupported
// actions and dead-or-absent children are rejected with Accepted=false.
func (r *peerRuntime) Control(ctx context.Context, req *criteriav1.ControlRequest) *criteriav1.ControlResponse {
	if req.GetKillChild() == nil && req.GetCancelChildRun() == nil &&
		req.GetRespawnChild() == nil && req.GetIdleReap() == nil {
		return &criteriav1.ControlResponse{Accepted: false, Detail: "unsupported control action"}
	}
	if cancel := req.GetCancelChildRun(); cancel != nil {
		return &criteriav1.ControlResponse{
			Accepted: false,
			Detail:   fmt.Sprintf("no child run %q in this peer", cancel.GetRunId()),
		}
	}
	if respawn := req.GetRespawnChild(); respawn != nil {
		return r.controlRespawnChild(ctx, req, respawn)
	}
	if idle := req.GetIdleReap(); idle != nil {
		return r.controlIdleReap(req, idle)
	}
	return r.controlKillChild(req)
}

// controlKillChild executes Control kill_child (ADR-0008): an empty
// adapter_type addresses the primary child (legacy single-adapter hosts).
// The mark-before-decree ordering keeps an exit the watcher observes
// before the kill lands peer-initiated (graceful, no crash fact).
func (r *peerRuntime) controlKillChild(req *criteriav1.ControlRequest) *criteriav1.ControlResponse {
	name := req.GetAdapterType()
	r.mu.Lock()
	if name == "" {
		// Legacy host: the request predates multi-adapter keying; the
		// primary child is the addressed one.
		if p := r.primaryChildLocked(); p != nil {
			name = p.name
		}
	}
	c := r.children[name]
	r.mu.Unlock()
	if c == nil {
		if req.GetAdapterType() == "" {
			return &criteriav1.ControlResponse{Accepted: false, Detail: "no live adapter child"}
		}
		return &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("no adapter child %q", name)}
	}
	grace := time.Duration(req.GetGraceMs()) * time.Millisecond
	if grace <= 0 {
		grace = defaultControlGrace
	}
	r.mu.Lock()
	if adapterhost.ProcessExited(c.handle) {
		r.mu.Unlock()
		return &criteriav1.ControlResponse{Accepted: false, Detail: "no live adapter child"}
	}
	// Accepted from here on: mark before the kill is issued so an exit the
	// watcher observes before the kill lands still classifies as
	// peer-initiated.
	c.killRequested = true
	handle := c.handle
	r.mu.Unlock()

	go r.killAfterGrace(handle, grace)
	return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("kill scheduled after %s grace", grace)}
}

// killAfterGrace waits out the declared grace and kills the child unless it
// already exited. The grace wait is detached from the Control RPC: the kill
// was acknowledged and must still land even if the host stream ends first.
// Peer shutdown (stopWatch closed) kills promptly.
func (r *peerRuntime) killAfterGrace(handle adapterhost.Handle, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.stopWatch:
	}
	if !adapterhost.ProcessExited(handle) {
		handle.Kill()
	}
	// The exit fact is journaled by the watcher (killRequested keeps it
	// graceful); during shutdown, Shutdown's own recordExit covers it.
}

// serveOpened counts the live phone-home conn serving each named child.
func (r *peerRuntime) serveOpened(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		r.serveCounts[name]++
	}
}

// serveClosed drops the live-conn count and, when child keepalive is
// disabled, kills children whose last serving conn just ended (no child
// survives a host disconnect, legacy or multi).
func (r *peerRuntime) serveClosed(names []string) {
	r.mu.Lock()
	kills := make([]adapterhost.Handle, 0, len(names))
	for _, name := range names {
		n := r.serveCounts[name]
		if n > 0 {
			n--
		}
		r.serveCounts[name] = n
		if r.cfg != nil && !r.cfg.ChildKeepAlive && n == 0 {
			if c := r.children[name]; c != nil && c.handle != nil && !adapterhost.ProcessExited(c.handle) {
				kills = append(kills, c.handle)
			}
		}
	}
	r.mu.Unlock()
	for _, h := range kills {
		if h != nil && !adapterhost.ProcessExited(h) {
			h.Kill()
		}
	}
}

// sessionOpened records a session the host opened through the served bridge
// of one child.
func (r *peerRuntime) sessionOpened(name, id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.children[name]; c != nil {
		c.openSessions[id] = struct{}{}
	}
}

// sessionClosed forgets a session the host closed.
func (r *peerRuntime) sessionClosed(name, id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.children[name]; c != nil {
		delete(c.openSessions, id)
	}
}

// openSessionIDs returns the open session IDs of one child, sorted for
// deterministic shutdown.
func (r *peerRuntime) openSessionIDs(c *childState) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(c.openSessions))
	for id := range c.openSessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// setServedChild records the adapter client the phone-home bridge serves for
// one child so shutdown closes the host's sessions on the same surface.
func (r *peerRuntime) setServedChild(name string, c adapterhost.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.children[name]; st != nil {
		st.servedChild = c
	}
}

// journalFlushed records the StreamFlushed fact: the named stream ended and
// everything the journal has recorded up to upToSeq was delivered.
func (r *peerRuntime) journalFlushed(name, scope, channel string, upToSeq uint64) {
	if _, err := r.journal.Append(&criteriav1.SupervisionEvent_Flushed{Flushed: &criteriav1.StreamFlushed{
		Channel: channel,
		UpToSeq: upToSeq,
	}}, name, scope, ""); err != nil {
		r.log.Error("journal flushed event", "error", err)
	}
	r.mu.Lock()
	if c := r.children[name]; c != nil {
		c.lastEventAt = time.Now()
	}
	r.mu.Unlock()
}

// waitChildGrace waits up to grace for the child to exit on its own, polling
// at the exit-watcher interval. It returns early when the child dies during
// the window; otherwise the caller's Kill ends the wait's purpose.
func (r *peerRuntime) waitChildGrace(child adapterhost.Handle, grace time.Duration) {
	if grace <= 0 {
		return
	}
	poll := r.exitPoll
	if poll <= 0 {
		poll = defaultExitPollInterval
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			return
		case <-ticker.C:
			if adapterhost.ProcessExited(child) {
				return
			}
		}
	}
}

// Shutdown runs the peer shutdown sequence: stop accepting (the watch
// loops), close the sessions the host opened on each child in spawn order,
// wait out the grace period for a clean child exit, then kill the children
// and tear down the loader, journaling the final exit facts. Idempotent.
func (r *peerRuntime) Shutdown(ctx context.Context) error {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.shuttingDown = true
		children := make([]*childState, 0, len(r.order))
		for _, name := range r.order {
			children = append(children, r.children[name])
		}
		r.mu.Unlock()

		// Registration is refused from here on, so the drain below can
		// only be waiting for watchers that registered before it.
		r.closeWatcherRegistration()
		close(r.stopWatch)
		r.drainExitWatchers()

		r.shutdownChildren(ctx, children)
		if err := r.loader.Shutdown(ctx); err != nil {
			r.log.Warn("loader shutdown", "error", err)
		}
		r.log.Info("peer shutdown complete", "adapters", strings.Join(childNames(children), ","))
	})
	return nil
}

// shutdownChildren closes the host-opened sessions of every child in spawn
// order, grants each a clean-exit grace window, then kills survivors.
func (r *peerRuntime) shutdownChildren(ctx context.Context, children []*childState) {
	for _, c := range children {
		r.closeChildSessions(ctx, c)
		if c.handle != nil && !adapterhost.ProcessExited(c.handle) {
			r.waitChildGrace(c.handle, r.shutdownGrace)
		}
		if c.handle != nil && !adapterhost.ProcessExited(c.handle) {
			c.handle.Kill()
		}
		r.recordExit(c, true)
	}
}

// closeChildSessions asks the served child adapter to close every session
// the host opened on it, journaling close failures without failing the
// shutdown.
func (r *peerRuntime) closeChildSessions(ctx context.Context, c *childState) {
	sessionIDs := r.openSessionIDs(c)
	for _, id := range sessionIDs {
		if c.servedChild == nil {
			// Nothing the bridge served: no session surface to close.
			break
		}
		ctxSession, cancel := context.WithTimeout(ctx, closeSessionTimeout)
		_, err := c.servedChild.CloseSession(ctxSession, &v2.CloseSessionRequest{SessionId: id})
		cancel()
		if err != nil {
			r.log.Warn("shutdown close session", "session", id, "error", err)
		} else {
			r.sessionClosed(c.name, id)
		}
	}
}

func childNames(children []*childState) []string {
	out := make([]string, 0, len(children))
	for _, c := range children {
		out = append(out, c.name)
	}
	return out
}
