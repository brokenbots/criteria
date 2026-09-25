package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// defaultExitPollInterval is how often the peer polls the child's go-plugin
// client for process exit.
const defaultExitPollInterval = 500 * time.Millisecond

// peerRuntime is the peer's supervision state: the resolved configuration,
// the go-plugin loader that owns the child process, the child handle, the
// bounded supervision journal, and the most recent process-exit fact.
type peerRuntime struct {
	cfg     Config
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

	exitPoll  time.Duration
	stopWatch chan struct{}
	watchDone chan struct{}
	stopOnce  sync.Once
}

// NewRuntime builds a peer runtime for the given (already resolved)
// configuration.
func NewRuntime(cfg Config, log *slog.Logger) *peerRuntime {
	if log == nil {
		log = slog.Default()
	}
	return &peerRuntime{
		cfg:       cfg,
		log:       log,
		loader:    adapterhost.NewLoader(),
		journal:   NewEventJournal(cfg.JournalLimit),
		exitPoll:  defaultExitPollInterval,
		stopWatch: make(chan struct{}),
		watchDone: make(chan struct{}),
	}
}

// Config returns the runtime configuration.
func (r *peerRuntime) Config() Config { return r.cfg }

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

	// Launch through the loader so the child is a real go-plugin child with
	// stderr captured by hclog and accurate process-exit reporting. The
	// discovery function pins the loader to the configured binary so the
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
		return fmt.Errorf("start adapter %q: %w", name, err)
	}
	r.mu.Lock()
	r.child = child
	r.lastEventAt = time.Now()
	r.mu.Unlock()

	// Verify the child is a live, dispensable adapter before declaring the
	// peer ready. On failure, tear the child down immediately so the failing
	// boot never orphans a subprocess.
	info, err := child.Info(ctx)
	if err != nil {
		r.log.Error("adapter child failed Info", "adapter", name, "error", err)
		r.journal.Append(&criteriav1.SupervisionEvent_Crash{Crash: &criteriav1.CrashClassified{
			Reason: adapterhost.CrashReasonProcessExitedEarly,
			Detail: fmt.Sprintf("Info failed right after spawn: %v", err),
		}}, name, r.cfg.Scope, "")
		child.Kill()
		return fmt.Errorf("adapter %q Info: %w", name, err)
	}

	version := info.Version
	if version == "" {
		version = r.cfg.AdapterVersion
	}
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

	r.mu.Lock()
	r.watchStarted = true
	r.mu.Unlock()
	go r.watchChild()
	return nil
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
	graceful := peerInitiated || r.shuttingDown
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

// Shutdown stops the child. The child is killed only here (peer shutdown) or
// via the Control RPC once Stage A control wiring lands; with child keepalive
// on it survives host disconnects. Idempotent: later calls return nil.
func (r *peerRuntime) Shutdown(ctx context.Context) error {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.shuttingDown = true
		watchStarted := r.watchStarted
		child := r.child
		r.mu.Unlock()
		close(r.stopWatch)
		if watchStarted {
			<-r.watchDone
		}
		if child != nil && !adapterhost.ProcessExited(child) {
			child.Kill()
		}
		if err := r.loader.Shutdown(ctx); err != nil {
			r.log.Warn("loader shutdown", "error", err)
		}
		r.recordExit(true)
		r.log.Info("peer shutdown complete", "adapter", r.cfg.AdapterName)
	})
	return nil
}
