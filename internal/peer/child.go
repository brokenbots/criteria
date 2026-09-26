package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// defaultExitPollInterval is how often the peer polls the child's go-plugin
// client for process exit.
const defaultExitPollInterval = 500 * time.Millisecond

// closeSessionTimeout bounds each per-session CloseSession during shutdown so
// one stuck child cannot stall the whole sequence.
const closeSessionTimeout = 5 * time.Second

// peerRuntime is the peer's supervision state: the resolved configuration,
// the go-plugin loader that owns the child process, the child handle, the
// bounded supervision journal, and the most recent process-exit fact.
type peerRuntime struct {
	cfg     *Config
	log     *slog.Logger
	loader  *adapterhost.DefaultLoader
	journal *EventJournal

	mu           sync.Mutex
	lastExit     *criteriav1.ProcessExited
	exited       bool
	shuttingDown bool
	booted       bool
	watchStarted bool
	lastEventAt  time.Time
	child        adapterhost.Handle
	// killRequested is set the moment a host Control(kill_child) is
	// accepted, so an exit observed by the watcher before the kill lands is
	// still classified as peer-initiated (graceful, no crash event).
	killRequested bool
	// openSessions tracks the sessions the host opened through the served
	// bridge; they are closed on the child before the shutdown kill.
	openSessions map[string]struct{}
	// shutdownGrace bounds the shutdown wait before the child kill.
	shutdownGrace time.Duration
	// servedChild is the adapter client the phone-home bridge serves (the
	// local child in production; a fixture stub in tests). Shutdown closes
	// tracked sessions through it.
	servedChild adapterhost.Client

	exitPoll  time.Duration
	stopWatch chan struct{}
	watchDone chan struct{}
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
		openSessions:  map[string]struct{}{},
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

// Child returns the spawned child handle, or nil before Boot succeeds.
func (r *peerRuntime) Child() adapterhost.Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.child
}

// LastExit returns the most recent process-exit fact, or nil while the child
// is running.
func (r *peerRuntime) LastExit() *criteriav1.ProcessExited {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastExit
}

// Boot resolves and spawns the adapter child, verifies it with Info, records
// the spawn into the journal, and starts the exit watcher. The child is a
// real go-plugin subprocess launched through the adapterhost loader; its
// environment is scrubbed of every CRITERIA_REMOTE_* variable (ChildEnv) so
// it cannot sniff into phone-home mode. Boot must be called once.
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

	name := r.cfg.AdapterName
	if name == "" {
		return errors.New("adapter name not resolved; set CRITERIA_ADAPTER_NAME or CRITERIA_ADAPTER_BINARY")
	}

	child, info, err := r.spawnChild(ctx, name)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.child = child
	r.mu.Unlock()

	version := info.Version
	if version == "" {
		version = r.cfg.AdapterVersion
	}
	if err := r.recordSpawn(child, name, version); err != nil {
		return err
	}

	r.mu.Lock()
	r.watchStarted = true
	r.mu.Unlock()
	go r.watchChild()
	return nil
}

// recordSpawn journals the ProcessSpawned fact and logs the spawn line.
func (r *peerRuntime) recordSpawn(child adapterhost.Handle, name, version string) error {
	pid, _ := adapterhost.ProcessPID(child)
	spawned, appendErr := r.journal.Append(&criteriav1.SupervisionEvent_Spawned{Spawned: &criteriav1.ProcessSpawned{
		Binary:  r.cfg.Binary(),
		Digest:  r.cfg.Digest,
		Version: version,
		Pid:     int32(pid),
	}}, name, r.cfg.Scope, "")
	if appendErr != nil {
		return fmt.Errorf("record spawn: %w", appendErr)
	}
	r.mu.Lock()
	r.lastEventAt = time.Now()
	r.mu.Unlock()

	r.log.Info("peer child spawned",
		"adapter", name,
		"binary", r.cfg.Binary(),
		"digest", r.cfg.Digest,
		"version", version,
		"pid", pid,
		"scope", r.cfg.Scope,
		"child_keepalive", r.cfg.ChildKeepAlive,
		"event_seq", spawned.GetEventSeq(),
	)
	return nil
}

// spawnChild launches the adapter child through the adapterhost loader and
// verifies it with Info. On verification failure it tears the child down
// immediately so a failed boot never orphans a subprocess.
func (r *peerRuntime) spawnChild(ctx context.Context, name string) (adapterhost.Handle, adapterhost.Info, error) {
	// The discovery function pins the loader to the configured binary so the
	// resolution precedence (manifest → env → PATH → digest preference) stays
	// in Config.Resolve.
	discovery := func(string) (string, error) {
		if r.cfg.Binary() == "" {
			return "", errors.New("adapter binary not resolved; set CRITERIA_ADAPTER_BINARY")
		}
		return r.cfg.Binary(), nil
	}
	childEnv := ChildEnv(os.Environ())
	customizer := func(_ string, cmd *exec.Cmd) {
		cmd.Env = childEnv
	}
	child, err := r.loader.ResolveWithDiscovery(ctx, name, discovery, customizer)
	if err != nil {
		return nil, adapterhost.Info{}, fmt.Errorf("start adapter %q: %w", name, err)
	}

	info, err := child.Info(ctx)
	if err != nil {
		r.log.Error("adapter child failed Info", "adapter", name, "error", err)
		if _, jerr := r.journal.Append(&criteriav1.SupervisionEvent_Crash{Crash: &criteriav1.CrashClassified{
			Reason: adapterhost.CrashReasonProcessExitedEarly,
			Detail: fmt.Sprintf("Info failed right after spawn: %v", err),
		}}, name, r.cfg.Scope, ""); jerr != nil {
			r.log.Error("journal crash event", "error", jerr)
		}
		child.Kill()
		return nil, adapterhost.Info{}, fmt.Errorf("adapter %q Info: %w", name, err)
	}
	return child, info, nil
}

// watchChild polls the child's process state and journals the exit fact the
// moment it disappears. A peer-initiated shutdown stops the watcher first, so
// exits observed here are always ungraceful crashes.
func (r *peerRuntime) watchChild() {
	defer close(r.watchDone)
	child := r.Child()
	ticker := time.NewTicker(r.exitPoll)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopWatch:
			return
		case <-ticker.C:
			if adapterhost.ProcessExited(child) {
				r.recordExit(false)
				return
			}
		}
	}
}

// recordExit journals the terminal ProcessExited fact exactly once. When the
// peer did not initiate the shutdown the exit is a crash: the reason comes
// from the shared adapterhost crash taxonomy (peer.proto CrashClassified
// consumes that vocabulary). go-plugin does not expose the child's exit
// status, so the exit code is recorded as -1 (unknown), matching the
// ProcessExited contract.
func (r *peerRuntime) recordExit(peerInitiated bool) {
	r.mu.Lock()
	child := r.child
	if child == nil {
		// Nothing was ever spawned: no process-exit fact exists.
		r.mu.Unlock()
		return
	}
	if r.exited {
		r.mu.Unlock()
		return
	}
	r.exited = true
	graceful := peerInitiated || r.shuttingDown || r.killRequested
	idleMS := uint64(0)
	if !r.lastEventAt.IsZero() {
		idleMS = uint64(time.Since(r.lastEventAt).Milliseconds())
	}
	r.lastEventAt = time.Now()
	r.mu.Unlock()

	exit := &criteriav1.ProcessExited{
		ExitCode: -1,
		Signal:   0,
		IdleMs:   idleMS,
		Graceful: graceful,
	}
	r.mu.Lock()
	r.lastExit = exit
	r.mu.Unlock()

	if _, err := r.journal.Append(&criteriav1.SupervisionEvent_Exited{Exited: exit}, r.cfg.AdapterName, r.cfg.Scope, ""); err != nil {
		r.log.Error("journal exited event", "error", err)
	}
	if !graceful {
		r.log.Error("adapter child exited unexpectedly",
			"adapter", r.cfg.AdapterName,
			"reason", adapterhost.CrashReasonProcessTerminated,
			"idle_ms", idleMS,
		)
		if _, err := r.journal.Append(&criteriav1.SupervisionEvent_Crash{Crash: &criteriav1.CrashClassified{
			Reason: adapterhost.CrashReasonProcessTerminated,
			Detail: "adapter child exited while supervised; go-plugin does not expose the exit status",
		}}, r.cfg.AdapterName, r.cfg.Scope, ""); err != nil {
			r.log.Error("journal crash event", "error", err)
		}
	}
}

// defaultControlGrace is the grace period a Control(kill_child) waits before
// killing the child when the request carries no explicit grace.
const defaultControlGrace = 5 * time.Second

// Control executes a host-initiated control action (the PeerService.Control
// RPC). kill_child acknowledges immediately, waits out the requested grace
// period (default 5s, host sends 3s), then kills the child; the watcher (or
// peer shutdown) journals the exit fact, classified as peer-initiated via
// killRequested. Unsupported actions and dead-or-absent children are
// rejected with Accepted=false.
func (r *peerRuntime) Control(ctx context.Context, req *criteriav1.ControlRequest) *criteriav1.ControlResponse {
	if req.GetKillChild() == nil {
		return &criteriav1.ControlResponse{Accepted: false, Detail: "unsupported control action"}
	}
	grace := time.Duration(req.GetGraceMs()) * time.Millisecond
	if grace <= 0 {
		grace = defaultControlGrace
	}
	r.mu.Lock()
	child := r.child
	r.killRequested = true
	r.mu.Unlock()
	if child == nil || adapterhost.ProcessExited(child) {
		return &criteriav1.ControlResponse{Accepted: false, Detail: "no live adapter child"}
	}
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
		r.mu.Lock()
		child := r.child
		r.mu.Unlock()
		if child != nil && !adapterhost.ProcessExited(child) {
			child.Kill()
		}
		// The exit fact is journaled by the watcher (killRequested keeps it
		// graceful); under shutdown, Shutdown's own recordExit covers it.
	}()
	return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("kill scheduled after %s grace", grace)}
}

// killChild terminates a live child between reconnect attempts when child
// keepalive is disabled (legacy runner parity: no child survives a host
// disconnect). No-op when nothing is alive; safe to call repeatedly.
func (r *peerRuntime) killChild() {
	r.mu.Lock()
	child := r.child
	r.mu.Unlock()
	if child != nil && !adapterhost.ProcessExited(child) {
		child.Kill()
	}
}

// sessionOpened records a session the host opened through the served bridge.
func (r *peerRuntime) sessionOpened(id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openSessions[id] = struct{}{}
}

// sessionClosed forgets a session the host closed.
func (r *peerRuntime) sessionClosed(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.openSessions, id)
}

func (r *peerRuntime) openSessionIDsLocked() []string {
	ids := make([]string, 0, len(r.openSessions))
	for id := range r.openSessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// setServedChild records the adapter client the phone-home bridge serves so
// shutdown closes the host's sessions on the same surface.
func (r *peerRuntime) setServedChild(c adapterhost.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.servedChild = c
}

// journalFlushed records the StreamFlushed fact: the named stream ended and
// everything the journal has recorded up to upToSeq was delivered.
func (r *peerRuntime) journalFlushed(channel string, upToSeq uint64) {
	if _, err := r.journal.Append(&criteriav1.SupervisionEvent_Flushed{Flushed: &criteriav1.StreamFlushed{
		Channel: channel,
		UpToSeq: upToSeq,
	}}, r.cfg.AdapterName, r.cfg.Scope, ""); err != nil {
		r.log.Error("journal flushed event", "error", err)
	}
	r.mu.Lock()
	r.lastEventAt = time.Now()
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

// Shutdown runs the peer shutdown sequence (spec item 7): stop accepting
// (the watch loop), close the sessions the host opened on the child, wait
// out the grace period for a clean child exit, then kill the child and tear
// down the loader, journaling the final exit fact. Idempotent.
func (r *peerRuntime) Shutdown(ctx context.Context) error {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.shuttingDown = true
		watchStarted := r.watchStarted
		child := r.child
		sessionIDs := r.openSessionIDsLocked()
		grace := r.shutdownGrace
		servedChild := r.servedChild
		r.mu.Unlock()
		close(r.stopWatch)
		if watchStarted {
			<-r.watchDone
		}
		for _, id := range sessionIDs {
			if servedChild == nil {
				// Nothing the bridge served: no session surface to close.
				break
			}
			ctxSession, cancel := context.WithTimeout(ctx, closeSessionTimeout)
			_, err := servedChild.CloseSession(ctxSession, &v2.CloseSessionRequest{SessionId: id})
			cancel()
			if err != nil {
				r.log.Warn("shutdown close session", "session", id, "error", err)
			} else {
				r.sessionClosed(id)
			}
		}
		if child != nil && !adapterhost.ProcessExited(child) {
			r.waitChildGrace(child, grace)
			if !adapterhost.ProcessExited(child) {
				child.Kill()
			}
		}
		if err := r.loader.Shutdown(ctx); err != nil {
			r.log.Warn("loader shutdown", "error", err)
		}
		r.recordExit(true)
		r.log.Info("peer shutdown complete", "adapter", r.cfg.AdapterName)
	})
	return nil
}
