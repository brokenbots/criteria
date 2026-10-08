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
// across the environment's phone-home connections (KB-213): the child is
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
	exited        bool
	lastExit      *criteriav1.ProcessExited
	lastEventAt   time.Time

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
	// exitPoll is the process-exit poll interval (tunable in tests).
	exitPoll  time.Duration
	stopWatch chan struct{}
	// watchDone closes once every child exit watcher has stopped.
	watchDone chan struct{}
	watchWG   sync.WaitGroup
	stopOnce  sync.Once
}

// NewRuntime builds a peer runtime for the given (already resolved)
// configuration. The runtime takes ownership of cfg.
func NewRuntime(cfg *Config, log *slog.Logger) *peerRuntime {
	if log == nil {
		log = slog.Default()
	}
	return &peerRuntime{
		cfg:           cfg,
		log:           log,
		loader:        adapterhost.NewLoader(),
		journal:       NewEventJournal(cfg.JournalLimit),
		children:      map[string]*childState{},
		serveCounts:   map[string]int{},
		shutdownGrace: defaultControlGrace,
		exitPoll:      defaultExitPollInterval,
		stopWatch:     make(chan struct{}),
		watchDone:     make(chan struct{}),
	}
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

// childSpec is one boot entry: the manifest-derived adapter spec plus the
// journal attribution scope for the child's process facts.
type childSpec struct {
	spec AdapterSpec
	// scope is the journal attribution scope for the child's process facts;
	// "" when the child serves a whole scope set (per-scope dial mode).
	scope string
}

// childSpecs resolves the boot list: the multi-adapter manifest when
// declared, else the single legacy adapter. With CRITERIA_REMOTE_SCOPES_DIR
// set the hosted children still cover the adapter types of the manifest (or
// the single legacy adapter); the scope tokens arrive per (scope, adapter)
// at serve time.
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

	var spawned []*childState
	for i := range specs {
		c, err := r.spawnChild(ctx, &specs[i])
		if err != nil {
			r.teardownSpawnedChildren(ctx, spawned)
			return err
		}
		if err := r.recordSpawn(c); err != nil {
			spawned = append(spawned, c)
			r.teardownSpawnedChildren(ctx, spawned)
			return err
		}
		spawned = append(spawned, c)
		r.watchWG.Add(1)
		go r.watchChild(c)
	}
	if len(spawned) > 0 {
		go func() {
			r.watchWG.Wait()
			close(r.watchDone)
		}()
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

	c := &childState{
		name:         s.Name,
		scope:        spec.scope,
		handle:       child,
		binary:       s.Binary,
		digest:       s.Digest,
		openSessions: map[string]struct{}{},
	}
	r.mu.Lock()
	r.children[s.Name] = c
	r.order = append(r.order, s.Name)
	r.mu.Unlock()

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
		return nil, fmt.Errorf("adapter %q Info: %w", s.Name, err)
	}
	version := info.Version
	if version == "" {
		version = s.Version
	}
	c.version = version
	return c, nil
}

// watchChild polls one child's process state and journals the exit fact the
// moment it disappears. A peer-initiated shutdown stops the watcher first,
// so exits observed here are always ungraceful crashes.
func (r *peerRuntime) watchChild(c *childState) {
	defer r.watchWG.Done()
	ticker := time.NewTicker(r.exitPoll)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopWatch:
			return
		case <-ticker.C:
			if adapterhost.ProcessExited(c.handle) {
				r.recordExit(c, false)
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
	graceful := peerInitiated || r.shuttingDown || c.killRequested
	idleMS := uint64(0)
	if !c.lastEventAt.IsZero() {
		idleMS = uint64(time.Since(c.lastEventAt).Milliseconds())
	}
	c.lastEventAt = time.Now()
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

	if _, err := r.journal.Append(&criteriav1.SupervisionEvent_Exited{Exited: exit}, c.name, c.scope, ""); err != nil {
		r.log.Error("journal exited event", "error", err)
	}
	if !graceful {
		r.classifyUnexpectedExit(c, exitCode, signal)
	}
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

// Control executes a host-initiated control action (the PeerService.Control
// RPC). kill_child acknowledges immediately, waits out the requested grace
// period (default 5s, host sends 3s), then kills the addressed child; the
// watcher journals the exit fact, classified as peer-initiated via
// killRequested. The addressed child is the one named by the request's
// adapter_type; an empty adapter_type addresses the primary child (legacy
// single-adapter hosts, KB-213 compat). cancel_child_run (ADR-0008,
// workflow.v1-gated at the server) is a known arm but there is no child-run
// substrate in the peer role: nothing to execute a run-level cancellation
// against, so it is rejected with Accepted=false. Unsupported actions and
// dead-or-absent children are rejected with Accepted=false.
func (r *peerRuntime) Control(ctx context.Context, req *criteriav1.ControlRequest) *criteriav1.ControlResponse {
	if req.GetKillChild() == nil && req.GetCancelChildRun() == nil {
		return &criteriav1.ControlResponse{Accepted: false, Detail: "unsupported control action"}
	}
	if cancel := req.GetCancelChildRun(); cancel != nil {
		return &criteriav1.ControlResponse{
			Accepted: false,
			Detail:   fmt.Sprintf("no child run %q in this peer", cancel.GetRunId()),
		}
	}
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

	go func() {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		// The grace wait is detached from the Control RPC: the kill was
		// acknowledged and must still land even if the host stream ends
		// first. Peer shutdown (stopWatch closed) kills promptly.
		select {
		case <-timer.C:
		case <-r.stopWatch:
		}
		if !adapterhost.ProcessExited(handle) {
			handle.Kill()
		}
		// The exit fact is journaled by the watcher (killRequested keeps it
		// graceful); during shutdown, Shutdown's own recordExit covers it.
	}()
	return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("kill scheduled after %s grace", grace)}
}

// killChild terminates live children between reconnect attempts when child
// keepalive is disabled (legacy runner parity: no child survives a host
// disconnect). No-op when nothing is alive; safe to call repeatedly. The
// names argument selects the children; all children when empty.
func (r *peerRuntime) killChild(names ...string) {
	r.mu.Lock()
	selected := names
	if len(selected) == 0 {
		selected = append([]string(nil), r.order...)
	}
	handles := make([]adapterhost.Handle, 0, len(selected))
	for _, name := range selected {
		if c := r.children[name]; c != nil {
			handles = append(handles, c.handle)
		}
	}
	r.mu.Unlock()
	for _, h := range handles {
		if h != nil && !adapterhost.ProcessExited(h) {
			h.Kill()
		}
	}
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

		close(r.stopWatch)
		r.watchWG.Wait()

		for _, c := range children {
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
			if c.handle != nil && !adapterhost.ProcessExited(c.handle) {
				r.waitChildGrace(c.handle, r.shutdownGrace)
			}
			if c.handle != nil && !adapterhost.ProcessExited(c.handle) {
				c.handle.Kill()
			}
			r.recordExit(c, true)
		}
		if err := r.loader.Shutdown(ctx); err != nil {
			r.log.Warn("loader shutdown", "error", err)
		}
		r.log.Info("peer shutdown complete", "adapters", strings.Join(childNames(children), ","))
	})
	return nil
}

func childNames(children []*childState) []string {
	out := make([]string, 0, len(children))
	for _, c := range children {
		out = append(out, c.name)
	}
	return out
}
