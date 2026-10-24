package runstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// The loopback run-viewer exposes a castle.v1.ConsoleService surface
// (KB-328) so the same console front-end that talks to castle's Parapet can
// log a human into a criteria-local viewer. Castle is the single source of
// truth for console login: Login, Logout, and ListSessions are a bounded
// byte-level pass-through to the configured castle control plane, so every
// server-side consequence — credential checks, UNAUTHENTICATED and
// PERMISSION_DENIED semantics, session revocation on Logout (KB-325),
// ledger listing — is exactly castle's. Only login rides the proxy:
// ResolveResume stays local, because decision delivery addresses a run this
// process itself owns, so the loopback resolves it through the injected
// ConsoleResumeResolver (the same control wire the
// criteria.v1.LocalControlService verbs exercise), never through castle.
//
// When castle is not configured for this viewer, or cannot be reached,
// Login answers Connect UNIMPLEMENTED with an honest
// "console login is disabled" message — the same state a console front-end
// lands on when castle-side login is not configured — and Logout and
// ListSessions answer UNAVAILABLE, since a live console session can be
// revoked once the plane is back.
//
// The surface serves the Connect unary and gRPC-Web protocols (the console
// client's protocols): gRPC's HTTP/2 trailer status encoding does not
// survive a pass-through hop, and the loopback has no gRPC-native console
// callers.

// CastleConsole is the loopback server's castle.v1.ConsoleService surface.
type CastleConsole struct {
	baseURL  string // "" → console login is disabled for this viewer
	client   *http.Client
	resolver ConsoleResumeResolver
	log      *slog.Logger
}

// castleConsolePrefix is the Connect prefix the console surface mounts at
// under the loopback run-viewer server. It is the exact path the console
// client derives from its page origin (window.location), same as the
// criteria.v1.LocalControlService mount.
const castleConsolePrefix = "/castle.v1.ConsoleService/"

// ConsoleResumeResolver resolves a console ResolveResume decision against
// the run control this process owns. Decision delivery stays local on the
// loopback: castle is the source of truth for console login, never for the
// control state of a locally owned run. Implementations answer the local
// control vocabulary — ("ok", accepted) on delivery, or ("run_not_paused" |
// "no_pending_signal" | "signal_mismatch", false) on rejection, exactly like
// criteria.v1.LocalControlService.ResolveResume. err carries
// transport-class answers such as a foreign run id; a *connect.Error is
// mapped code-for-code.
type ConsoleResumeResolver interface {
	ResolveConsoleResume(runID, signal string, payload map[string]string) (accepted bool, reason string, err error)
}

const (
	// consoleProxyTimeout bounds one pass-through exchange, response body
	// reads included, so a wedged castle cannot hang a console request
	// (Client.Timeout covers the whole round trip).
	consoleProxyTimeout = 10 * time.Second
	// maxConsoleRequestBody bounds the request body the surface buffers for
	// pass-through or decoding: console procedures are credential and
	// decision envelopes (a few hundred bytes), so this is an abuse bound,
	// not a data-capacity limit.
	maxConsoleRequestBody = 64 << 10
)

// NewCastleConsole builds the console surface. castleURL (--castle-addr,
// CRITERIA_CASTLE_ADDR) is the castle control plane's base URL. An empty
// castleURL keeps the surface mounted but answers the disabled mapping —
// the honest state of a viewer whose operator never pointed it at castle.
func NewCastleConsole(castleURL string, resolver ConsoleResumeResolver, log *slog.Logger) (*CastleConsole, error) {
	if log == nil {
		log = slog.Default()
	}
	c := &CastleConsole{resolver: resolver, log: log}
	if castleURL == "" {
		return c, nil
	}
	u, err := url.Parse(castleURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid castle address %q: want a URL like http://127.0.0.1:8080", castleURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid castle address %q: scheme must be http or https", castleURL)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("invalid castle address %q: the console proxy targets the castle control-plane root (no path)", castleURL)
	}
	u.Path, u.RawQuery, u.Fragment, u.User = "", "", "", nil
	c.baseURL = u.String()
	log.Info("castle console login proxied to the castle control plane", "castle", c.baseURL, "resolve_source", "local control wire")

	tr := http.DefaultTransport.(*http.Transport).Clone()
	// The control plane is dialed directly: an environment-specified HTTP
	// proxy must never sit in front of the caller's console credentials.
	tr.Proxy = nil
	c.client = &http.Client{Transport: tr, Timeout: consoleProxyTimeout}
	// Redirects are never followed: a 3xx is passed back verbatim and the
	// request (Authorization header included) is never replayed elsewhere.
	c.client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return c, nil
}

// Handler returns the HTTP surface for the loopback server to mount at
// castleConsolePrefix.
func (c *CastleConsole) Handler() http.Handler {
	return http.HandlerFunc(c.serve)
}

func (c *CastleConsole) serve(w http.ResponseWriter, r *http.Request) {
	proc := strings.TrimPrefix(r.URL.Path, castleConsolePrefix)
	switch proc {
	case "Login", "Logout", "ListSessions":
		c.forward(w, r)
	case "ResolveResume":
		c.serveResolve(w, r)
	default:
		c.writeConnectError(w, connect.CodeUnimplemented,
			fmt.Sprintf("no such console procedure: %s", r.URL.Path))
	}
}

// consoleForwardRequestHeaders is the whitelist of request headers the
// pass-through forwards: the caller's console credential plus the Connect
// unary knobs the protocol defines. Everything else — cookies, browser
// fingerprints, the page Origin castle would run its own CORS posture
// against — is dropped at the loopback boundary.
var consoleForwardRequestHeaders = []string{
	"Authorization",
	"Content-Type",
	"Connect-Accept-Encoding",
	"Connect-Content-Encoding",
	"Connect-Protocol-Version",
	"Connect-Timeout-Ms",
}

func (c *CastleConsole) forward(w http.ResponseWriter, r *http.Request) {
	proc := strings.TrimPrefix(r.URL.Path, castleConsolePrefix)
	if c.baseURL == "" {
		c.consoleDisabled(w)
		return
	}
	body, capped, err := readConsoleBody(r)
	if capped {
		c.writeConnectError(w, connect.CodeInvalidArgument,
			fmt.Sprintf("console request body exceeds %d bytes", maxConsoleRequestBody))
		return
	}
	if err != nil {
		c.writeConnectError(w, connect.CodeInvalidArgument,
			"read console request body: "+err.Error())
		return
	}
	target := c.baseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		c.writeConnectError(w, connect.CodeInternal, "build castle request: "+err.Error())
		return
	}
	for _, h := range consoleForwardRequestHeaders {
		if v := r.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
	out.ContentLength = int64(len(body))
	resp, err := c.client.Do(out)
	if err != nil {
		// The request target is the caller's own context: an error here is
		// a transport failure against castle, not a client abort.
		if r.Context().Err() != nil {
			return
		}
		c.proxyDown(w, proc, err)
		return
	}
	defer resp.Body.Close()
	copyConsoleResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// proxyDown maps a forwarding failure to the console front-end's state
// machine honestly: for Login — the procedure that decides whether a human
// console exists here at all — an unreachable or unconfigured castle stays
// the "console login is disabled" state (UNIMPLEMENTED), exactly as the task
// mapping requires; for the already-authenticated surfaces the failure is
// temporal (UNAVAILABLE), since the session itself may still be live and
// revocable once the plane is back.
func (c *CastleConsole) proxyDown(w http.ResponseWriter, proc string, err error) {
	c.log.Warn("castle console proxy: forwarding failed",
		"procedure", proc, "castle", c.baseURL, "error", err)
	if proc == "Login" {
		c.writeConnectError(w, connect.CodeUnimplemented,
			"console login is disabled: cannot reach the castle control plane at "+c.baseURL+": "+err.Error())
		return
	}
	c.writeConnectError(w, connect.CodeUnavailable,
		"castle control plane at "+c.baseURL+" is unreachable: "+err.Error())
}

// consoleDisabled answers the no-castle-configured state: the console
// front-end classifies UNIMPLEMENTED as the disabled login state, and no
// console session can exist where no console login is configured.
func (c *CastleConsole) consoleDisabled(w http.ResponseWriter) {
	c.log.Warn("castle console proxy: no castle control plane configured for this run-viewer")
	c.writeConnectError(w, connect.CodeUnimplemented,
		"console login is disabled: no castle control plane is configured for this run-viewer (set --castle-addr or CRITERIA_CASTLE_ADDR to enable console login)")
}

// copyConsoleResponseHeaders relays the response headers the console client
// needs: the media headers that decode the body plus the Connect and gRPC
// protocol-status headers. Castle's server fingerprint, dates, and cookies
// are dropped at the boundary.
func copyConsoleResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch {
		case strings.EqualFold(k, "Content-Type"),
			strings.EqualFold(k, "Content-Encoding"),
			strings.HasPrefix(k, "Connect-"),
			strings.HasPrefix(k, "Grpc-"):
		default:
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// readConsoleBody reads the request body whole, bounding it. capped reports
// an over-limit body so callers reject instead of truncating.
func readConsoleBody(r *http.Request) (body []byte, capped bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r.Body, maxConsoleRequestBody+1))
	if err != nil {
		return nil, false, fmt.Errorf("read request body: %w", err)
	}
	if len(body) > maxConsoleRequestBody {
		return nil, true, nil
	}
	return body, false, nil
}

// writeConnectError answers a Connect unary error exactly as connect-go
// renders one for castle's own answers: {code,message} JSON over the mapped
// HTTP status, so console clients decode the precise Connect code the
// surface means.
func (c *CastleConsole) writeConnectError(w http.ResponseWriter, code connect.Code, msg string) {
	c.writeConnectErrorAt(w, connectHTTPStatus(code), code, msg)
}

// writeConnectErrorAt answers a Connect unary error with an explicit HTTP
// status: the {code,message} JSON body connect-go renders for castle's own
// answers, over the exact transport status the protocol or the caller's
// method error requires.
func (c *CastleConsole) writeConnectErrorAt(w http.ResponseWriter, status int, code connect.Code, msg string) {
	body, err := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code.String(), Message: msg})
	if err != nil {
		body = []byte(`{"code":"internal","message":"encode console error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// connectHTTPStatus is the Connect protocol's error→HTTP-status mapping
// (connect-go applies the same table to castle's own answers).
func connectHTTPStatus(code connect.Code) int {
	switch code {
	case connect.CodeCanceled:
		return 499
	case connect.CodeInvalidArgument,
		connect.CodeFailedPrecondition,
		connect.CodeOutOfRange:
		return http.StatusBadRequest
	case connect.CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodeAlreadyExists,
		connect.CodeAborted:
		return http.StatusConflict
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeUnimplemented:
		return http.StatusNotImplemented
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

// serveResolve answers the one non-forwarded procedure: decision delivery
// for a run this process owns, resolved through the local control wire.
func (c *CastleConsole) serveResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		c.writeConnectErrorAt(w, http.StatusMethodNotAllowed, connect.CodeUnimplemented,
			"method "+r.Method+" is not supported for castle.v1.ConsoleService/ResolveResume: POST is required")
		return
	}
	if c.resolver == nil {
		c.writeConnectError(w, connect.CodeUnimplemented,
			"console resolve is not available on this viewer: no local run control is wired to it")
		return
	}
	codec, req, code, emsg := c.decodeResolveRequest(r)
	if emsg != "" {
		w.Header().Set("Accept-Post", "application/json, application/proto")
		c.writeConnectErrorAt(w, connectHTTPStatus(code), code, emsg)
		return
	}
	accepted, reason, rerr := c.resolver.ResolveConsoleResume(req.runID, req.signal, req.payload)
	if rerr != nil {
		var ce *connect.Error
		if errors.As(rerr, &ce) {
			c.writeConnectError(w, ce.Code(), ce.Message())
			return
		}
		c.writeConnectError(w, connect.CodeInternal, "console resolve: "+rerr.Error())
		return
	}
	if !accepted {
		c.writeConnectError(w, connect.CodeFailedPrecondition, consoleRejectMessage(reason))
		return
	}
	resp, eerr := encodeConsoleResolve(time.Now().UTC(), codec)
	if eerr != "" {
		c.writeConnectError(w, connect.CodeInternal, eerr)
		return
	}
	w.Header().Set("Content-Type", codec.contentType())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// decodeResolveRequest negotiates the codec, drains the request body and
// decodes the ResolveResumeRequest. A non-empty error message carries the
// connect code to report: unimplemented for an unsupported content type,
// invalid_argument for a capped, unreadable or undecodable body.
func (c *CastleConsole) decodeResolveRequest(r *http.Request) (consoleCodec, consoleResolveRequest, connect.Code, string) {
	codec, ok := consoleCodecName(r.Header.Get("Content-Type"))
	if !ok {
		return codec, consoleResolveRequest{}, connect.CodeUnimplemented,
			fmt.Sprintf("unsupported content-type %q: the console surface speaks application/json or application/proto", r.Header.Get("Content-Type"))
	}
	body, capped, err := readConsoleBody(r)
	if capped {
		return codec, consoleResolveRequest{}, connect.CodeInvalidArgument,
			fmt.Sprintf("console request body exceeds %d bytes", maxConsoleRequestBody)
	}
	if err != nil {
		return codec, consoleResolveRequest{}, connect.CodeInvalidArgument,
			"read console request body: " + err.Error()
	}
	req, derr := decodeConsoleResolve(body, codec)
	if derr != "" {
		return codec, consoleResolveRequest{}, connect.CodeInvalidArgument, derr
	}
	return codec, req, connect.Code(0), ""
}

// consoleRejectMessage renders the local control vocabulary's rejection
// reasons for the console front-end.
func consoleRejectMessage(reason string) string {
	switch reason {
	case "":
		return "resolve rejected: the run did not accept the decision"
	case "run_not_paused":
		return "resolve rejected: the run is not paused"
	case "no_pending_signal":
		return "resolve rejected: the run has no pending signal to resolve"
	case "signal_mismatch":
		return "resolve rejected: signal does not match the run's pending signal"
	}
	return "resolve rejected: " + reason
}
