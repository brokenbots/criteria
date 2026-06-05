package conformance

// conformance_concurrent_stress.go — N-concurrent-session stress contract tests.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testConcurrentStress(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()
	n := opts.ConcurrentStressN
	if n <= 0 {
		t.Skipf("%s: concurrent_stress.n not configured (set >0 to enable)", name)
	}
	m := 5 // default Execute calls per session

	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sessions := openStressSessions(t, ctx, loader, name, opts, n)
	defer closeStressSessions(sessions)

	runStressExecutes(t, sessions, opts, info, name, m)
	assertStressResults(t, sessions, opts)
	assertNoCrossContamination(t, sessions)
}

func openStressSessions(t *testing.T, ctx context.Context, loader adapterhost.Loader, name string, opts *Options, n int) []*stressSession {
	t.Helper()
	sessions := make([]*stressSession, n)
	for i := 0; i < n; i++ {
		plug, err := loader.Resolve(ctx, name)
		if err != nil {
			t.Fatalf("resolve adapter %d: %v", i, err)
		}
		sessionID := newSessionID(fmt.Sprintf("stress-%d", i))
		if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), cloneConfig(opts.Secrets)); err != nil {
			plug.Kill()
			t.Fatalf("open session %d: %v", i, err)
		}
		sessions[i] = &stressSession{plug: plug, sessionID: sessionID, marker: sessionID}
	}
	return sessions
}

func closeStressSessions(sessions []*stressSession) {
	for _, s := range sessions {
		_ = s.plug.CloseSession(context.Background(), s.sessionID)
		s.plug.Kill()
	}
}

func runStressExecutes(t *testing.T, sessions []*stressSession, opts *Options, info *adapterhost.Info, name string, m int) {
	var wg sync.WaitGroup
	for i, sess := range sessions {
		for j := 0; j < m; j++ {
			wg.Add(1)
			go func(si, ej int, s *stressSession) {
				defer wg.Done()
				cfg := cloneConfig(opts.StepConfig)
				cfg["conformance_session_marker"] = s.marker
				step := baseStep(fmt.Sprintf("%s-%d-%d", name, si, ej), info.Name, cfg)
				sink := &recordingSink{}
				res, err := executeNoPanic(t, adapterSessionTarget{handle: s.plug, sessionID: s.sessionID, name: info.Name}, context.Background(), step, sink)
				s.mu.Lock()
				defer s.mu.Unlock()
				if err != nil {
					s.errs = append(s.errs, err)
				} else {
					s.results = append(s.results, res)
					s.sinks = append(s.sinks, sink)
				}
			}(i, j, sess)
		}
	}
	wg.Wait()
}

func assertStressResults(t *testing.T, sessions []*stressSession, opts *Options) {
	for i, sess := range sessions {
		if len(sess.errs) > 0 {
			t.Fatalf("session %d had %d execute errors: %v", i, len(sess.errs), sess.errs[0])
		}
		for _, res := range sess.results {
			assertValidOutcome(t, res.Outcome, opts)
		}
	}
}

func assertNoCrossContamination(t *testing.T, sessions []*stressSession) {
	for i, a := range sessions {
		for j, b := range sessions {
			if i == j {
				continue
			}
			for _, sink := range a.sinks {
				if sink.containsText(b.marker) {
					t.Fatalf("cross-contamination: session %d sink contains session %d marker %q", i, j, b.marker)
				}
			}
		}
	}
}

type stressSession struct {
	plug      adapterhost.Handle
	sessionID string
	marker    string
	mu        sync.Mutex
	errs      []error
	results   []adapter.Result
	sinks     []*recordingSink
}
