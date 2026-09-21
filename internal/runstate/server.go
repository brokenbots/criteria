package runstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
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
	// viewer, when non-nil, serves the embedded run-viewer bundle at /.
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

// WithViewer serves h (the embedded run-viewer bundle) at the server root
// with SPA fallback onto index.html.
func (s *Server) WithViewer(h http.Handler) *Server {
	s.viewer = h
	return s
}

// Handler returns the server's HTTP handler (exported for httptest).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /runs", s.handleListRuns)
	mux.HandleFunc("GET /runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /runs/{id}/inspect", s.handleInspect)
	mux.HandleFunc("GET /runs/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /runs/{id}/resume", s.verbHandler("resume"))
	mux.HandleFunc("POST /runs/{id}/pause", s.verbHandler("pause"))
	mux.HandleFunc("POST /runs/{id}/stop", s.verbHandler("stop"))
	mux.HandleFunc("GET /agents", s.handleAgents)
	mux.HandleFunc("GET /agents/{criteriaId}", s.handleAgent)
	if s.viewer != nil {
		mux.Handle("GET /{$}", s.viewer)
		mux.Handle("GET /", s.viewer) // SPA fallback
	}
	return logRequests(mux)
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
	writeJSON(w, http.StatusOK, map[string][]Agent{"agents": agents})
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
