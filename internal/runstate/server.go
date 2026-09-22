package runstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server serves the read-only local run-state API on loopback. It implements
// the seam contract consumed by the run viewer (Run/RunsPage/EventEnvelope/
// RunEventsPage as plain JSON); the viewer's data-source test is the
// executable spec. The only writes on this surface are the control verbs,
// which are limited to what the owning apply process can honor locally.
type Server struct {
	store *Store
	// control, when non-nil, handles the POST control verbs for runs this
	// process owns (stop cancels the engine context). Verbs it does not
	// answer come back UNIMPLEMENTED. nil (serve-ui standalone) means every
	// verb is UNIMPLEMENTED.
	control ControlHandler
	// viewer, when non-nil, serves the embedded run-viewer bundle under
	// /runview/.
	viewer http.Handler
	// srv is the http.Server created by Serve; Stop closes it.
	srvMu sync.Mutex
	srv   *http.Server
}

// ControlHandler applies a control verb to a run owned by this process. It
// returns ErrUnsupportedVerb for verbs it does not implement.
type ControlHandler func(runID, verb string) error

// ErrUnsupportedVerb is returned by a ControlHandler for verbs that are not
// locally implementable (no engine-side checkpoint control yet; CRI-255).
var ErrUnsupportedVerb = errors.New("unsupported control verb")

// ErrRunNotControlable is returned when the verb is known but the run is not
// owned (or controllable) by this process.
var ErrRunNotControlable = errors.New("run is not controllable by this process")

// NewServer returns the loopback API server over store.
func NewServer(store *Store) *Server {
	return &Server{store: store}
}

// WithControl wires the local control verbs (apply owns its run).
func (s *Server) WithControl(h ControlHandler) *Server {
	s.control = h
	return s
}

// WithViewer serves h (the embedded run-viewer bundle) under /runview/ with
// SPA fallback onto index.html.
func (s *Server) WithViewer(h http.Handler) *Server {
	s.viewer = h
	return s
}

// Handler returns the server's HTTP handler (exported for httptest).
//
// The seam API is mounted under /runview/api — the viewer bundle's default
// API base (localRunDataSource.ts RUNVIEW_API_BASE) — and the bundle itself
// is served under /runview/ (the castle standalone build's vite base), so
// the shipped consumer's requests and asset fetches resolve without an SPA
// fallback masquerading as data. / redirects to /runview/. Unknown
// /runview/api paths answer a JSON 404 — never the viewer's HTML fallback,
// which would silently mask API drift.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /runview/api/health", s.handleHealth)
	mux.HandleFunc("GET /runview/api/runs", s.handleListRuns)
	mux.HandleFunc("GET /runview/api/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /runview/api/runs/{id}/inspect", s.handleInspect)
	mux.HandleFunc("GET /runview/api/runs/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /runview/api/runs/{id}/resume", s.verbHandler("resume"))
	mux.HandleFunc("POST /runview/api/runs/{id}/pause", s.verbHandler("pause"))
	mux.HandleFunc("POST /runview/api/runs/{id}/stop", s.verbHandler("stop"))
	mux.HandleFunc("GET /runview/api/agents", s.handleAgents)
	mux.HandleFunc("GET /runview/api/agents/{criteriaId}", s.handleAgent)
	mux.HandleFunc("/runview/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such runstate API route")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/runview/", http.StatusFound)
	})
	h := http.Handler(mux)
	if s.viewer != nil {
		// The viewer is mounted with a manual dispatcher rather than a mux
		// pattern: "GET /runview/" and the method-wildcard "/runview/api/"
		// catch-all have no unambiguous precedence relationship in ServeMux
		// and panic on registration.
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// API prefix wins first (both exact routes and the JSON 404
			// catch-all live on the mux).
			if strings.HasPrefix(r.URL.Path, "/runview/api") {
				mux.ServeHTTP(w, r)
				return
			}
			if r.URL.Path == "/runview" {
				http.Redirect(w, r, "/runview/", http.StatusFound)
				return
			}
			if r.URL.Path == "/runview/" || strings.HasPrefix(r.URL.Path, "/runview/") {
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					writeError(w, http.StatusMethodNotAllowed, "viewer is read-only")
					return
				}
				http.StripPrefix("/runview", s.viewer).ServeHTTP(w, r)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}
	return logRequests(loopbackHostOnly(h))
}

// loopbackHostOnly rejects requests whose Host header is not a loopback host
// literal. The server binds loopback, but a browser on the same host can
// still be pointed at it by DNS rebinding (Host: attacker.example resolving
// to 127.0.0.1): without this gate such a page reads run data — step logs
// and adapter payloads included. Loopback IP literals and "localhost" (with
// any port) pass; everything else gets 403.
//
// Residual risk, documented and accepted for this ticket: a browser page
// served from another origin can still fire a cross-site POST at the control
// verbs without a preflight (a "simple request"). Exploiting it needs the
// run id — a UUID this server generated — and stop only cancels a run the
// local user owns; a per-process token or Origin check would change the seam
// request shape and needs consumer cooperation, which is tracked for CRI-255
// (the control-verb authority).
func loopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if !isLoopbackHost(host) {
			writeError(w, http.StatusForbidden, "loopback-only server: refusing request with non-loopback Host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether host is a loopback host literal: an IPv4 or
// IPv6 loopback IP (brackets already stripped by SplitHostPort, or stripped
// here for a bare bracketed literal) or the case-insensitive "localhost".
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if len(host) > 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Listen binds the server. Only loopback addresses are accepted; a
// non-loopback host is refused by design (the local viewer must never be
// reachable from other hosts).
func (s *Server) Listen(host string, port int) (net.Listener, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("refusing non-loopback bind %q: the local run-state server is loopback-only", host)
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return ln, nil
}

// Serve accepts connections on ln until Stop is called (or the listener
// fails). A Stop-induced return is nil, so callers can drain uniformly.
func (s *Server) Serve(ln net.Listener) error {
	s.srvMu.Lock()
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: headerTimeout}
	srv := s.srv
	s.srvMu.Unlock()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Stop closes the server's listener and connections (a no-op before Serve).
func (s *Server) Stop() {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Debug("runstate request", "method", r.Method, "path", r.URL.Path, "status", sw.status)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

const headerTimeout = 10 * time.Second

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent) // 204: alive; capability probe renders controls grayed (no control capabilities yet)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	page, err := s.store.ListRuns(q.Get("agent"), q.Get("status"), limit, q.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.PathValue("id"))
	if err != nil {
		writeNotFound(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	insp, err := s.store.Inspect(runID, r.URL.Query().Get("session"))
	if err != nil {
		writeNotFound(w, err)
		return
	}
	writeJSON(w, http.StatusOK, insp)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since int64
	if raw := q.Get("since_seq"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid since_seq")
			return
		}
		since = n
	}
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	page, err := s.store.Events(r.PathValue("id"), since, limit)
	if err != nil {
		writeNotFound(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// verbHandler implements POST /runs/{id}/resume|pause|stop. Verbs are wired
// to the engine only where locally supported (stop cancels the engine
// context); everything else answers UNIMPLEMENTED until CRI-255 adds the
// checkpoint-gated controls.
func (s *Server) verbHandler(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("id")
		if _, err := s.store.GetRun(runID); err != nil {
			writeNotFound(w, err)
			return
		}
		if s.control == nil {
			writeError(w, http.StatusNotImplemented, "control verb "+verb+" is not implemented for local runs yet (CRI-255)")
			return
		}
		if err := s.control(runID, verb); err != nil {
			switch {
			case errors.Is(err, ErrUnsupportedVerb):
				writeError(w, http.StatusNotImplemented, "control verb "+verb+" is not implemented for local runs yet (CRI-255)")
			case errors.Is(err, ErrNotFound):
				writeNotFound(w, err)
			default:
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"runId": runID, "status": verb + " accepted"})
	}
}

func (s *Server) handleAgents(w http.ResponseWriter, _ *http.Request) {
	agents, err := s.store.Agents()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Bare JSON array: listAgents() returns Agent[] (the executable spec's
	// stub expectation), not a wrapper object.
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.store.Agent(r.PathValue("criteriaId"))
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeNotFound maps the store's ErrNotFound to 404 and anything else to 500.
func writeNotFound(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
