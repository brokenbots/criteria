package runstate

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// errResolver is a ConsoleResumeResolver scripted by the test.
type errResolver struct {
	accept   bool
	reason   string
	err      error
	gotRunID string
	gotSig   string
	gotBody  map[string]string
	calls    int
}

func (r *errResolver) ResolveConsoleResume(runID, signal string, payload map[string]string) (accepted bool, reason string, err error) {
	r.calls++
	r.gotRunID, r.gotSig, r.gotBody = runID, signal, payload
	return r.accept, r.reason, r.err
}

// recordedCastleRequest is one request a fake castle received.
type recordedCastleRequest struct {
	path    string
	method  string
	body    string
	headers http.Header
}

// fakeCastle serves a scripted castle.v1.ConsoleService for pass-through
// tests. Login answers a session token plus Set-Cookie noise the proxy must
// drop; Logout answers an empty JSON object; ListSessions answers a session
// list carrying protocol headers (Grpc-/Connect-*) plus a Server
// fingerprint, for the response-header whitelist; anything else answers 404.
func fakeCastle(t *testing.T, logs *[]recordedCastleRequest) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*logs = append(*logs, recordedCastleRequest{
			path: r.URL.Path, method: r.Method, body: string(body), headers: r.Header.Clone(),
		})
		switch {
		case strings.HasSuffix(r.URL.Path, "/Login"):
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Set-Cookie", "castle-cookie=1; Path=/")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"sessionToken":"st-123","username":"ops"}`))
		case strings.HasSuffix(r.URL.Path, "/Logout"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/ListSessions"):
			w.Header().Set("Content-Type", "application/json")
			// Protocol headers the response whitelist must relay, plus a
			// server fingerprint it must drop.
			w.Header().Set("Grpc-Status", "0")
			w.Header().Set("Connect-Custom-Flag", "on")
			w.Header().Set("Server", "castle-test/1")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"no such route on fake castle"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// newTestConsole wires a console surface for a test that dials the in-test
// castle above (whose requests accumulate into logs). Disabled-mode tests
// construct CastleConsole directly.
func newTestConsole(t *testing.T, logs *[]recordedCastleRequest, resolver ConsoleResumeResolver) *CastleConsole {
	t.Helper()
	castleURL := fakeCastle(t, logs)
	c, err := NewCastleConsole(castleURL, resolver, nil)
	if err != nil {
		t.Fatalf("NewCastleConsole(%q): %v", castleURL, err)
	}
	return c
}

// postConsole POSTs to the console surface the way a loopback console
// client would (the server wrapper refuses other Hosts by design): a
// loopback Host literal, the given body and headers, and Connect version 1.
func postConsole(t *testing.T, h http.Handler, proc, contentType string, reqBody []byte, hdrs map[string]string) (status int, body string, hdr http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, castleConsolePrefix+proc, bytes.NewReader(reqBody))
	req.Host = "127.0.0.1:0"
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header()
}

// readConnectErr decodes a Connect JSON error body.
func readConnectErr(t *testing.T, body string) (code, message string) {
	t.Helper()
	var ce struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &ce); err != nil {
		t.Fatalf("error body %q: %v", body, err)
	}
	return ce.Code, ce.Message
}

// TestConsoleLoginDisabled pins the honest disabled state: with no castle
// configured, the login surfaces answer UNIMPLEMENTED "console login is
// disabled" and never touch a network address.
func TestConsoleLoginDisabled(t *testing.T) {
	c, err := NewCastleConsole("", nil, nil)
	if err != nil {
		t.Fatalf("disabled ctor: %v", err)
	}
	for _, proc := range []string{"Login", "Logout", "ListSessions"} {
		code, body, _ := postConsole(t, c.Handler(), proc, "application/json", []byte(`{}`), nil)
		if code != http.StatusNotImplemented {
			t.Errorf("%s disabled: code=%d body=%s", proc, code, body)
		}
		gcode, gmsg := readConnectErr(t, body)
		if gcode != connect.CodeUnimplemented.String() || !strings.Contains(gmsg, "console login is disabled") {
			t.Errorf("%s disabled = (%q, %q)", proc, gcode, gmsg)
		}
	}
	if c.baseURL != "" || c.client != nil {
		t.Errorf("disabled console keeps runtime state: base=%q", c.baseURL)
	}
}

// TestConsoleLoginUnreachable pins the unreachable-plane mapping: Login
// keeps the disabled state (login is the procedure that decides whether a
// human console exists here), while the already-authenticated surfaces
// report the temporal failure — the session itself may still be live.
func TestConsoleLoginUnreachable(t *testing.T) {
	c, err := NewCastleConsole("http://127.0.0.1:1", nil, nil)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	code, body, _ := postConsole(t, c.Handler(), "Login", "application/json", []byte(`{}`), nil)
	if code != http.StatusNotImplemented {
		t.Fatalf("unreachable Login: code=%d body=%s", code, body)
	}
	gcode, gmsg := readConnectErr(t, body)
	if gcode != connect.CodeUnimplemented.String() ||
		!strings.Contains(gmsg, "console login is disabled") || !strings.Contains(gmsg, "cannot reach") {
		t.Errorf("unreachable Login = (%q, %q)", gcode, gmsg)
	}
	code, body, _ = postConsole(t, c.Handler(), "Logout", "application/json", []byte(`{}`), nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable Logout: code=%d body=%s", code, body)
	}
	gcode, gmsg = readConnectErr(t, body)
	if gcode != connect.CodeUnavailable.String() || !strings.Contains(gmsg, "unreachable") {
		t.Errorf("unreachable Logout = (%q, %q)", gcode, gmsg)
	}
}

// TestConsoleLoginPassThrough pins the single-source-of-truth login flow:
// Login/Logout/ListSessions are forwarded to the castle control plane as
// the caller sent them and the response is relayed verbatim — castle's
// answers, not re-shaped ones — with console credentials forwarded and
// browser-context noise dropped at the boundary.
func TestConsoleLoginPassThrough(t *testing.T) {
	logs := &[]recordedCastleRequest{}
	console := newTestConsole(t, logs, nil)
	h := console.Handler()

	code, body, hdr := postConsole(t, h, "Login", "application/json",
		[]byte(`{"username":"ops","password":"pw"}`),
		map[string]string{
			"Authorization":      "Bearer tok",
			"Cookie":             "junk=1",
			"Origin":             "http://evil.example",
			"User-Agent":         "sneaky/8",
			"Connect-Timeout-Ms": "5000",
			"X-Castle-Foo":       "v2",
		})
	if code != http.StatusOK {
		t.Fatalf("Login: code=%d body=%s", code, body)
	}
	if want := `{"sessionToken":"st-123","username":"ops"}`; body != want {
		t.Errorf("Login body = %q, want %q", body, want)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Login Content-Type = %q", ct)
	}
	if sc := hdr.Get("Set-Cookie"); sc != "" {
		t.Errorf("castle Set-Cookie relayed to the console client: %q", sc)
	}
	got := &(*logs)[len(*logs)-1]
	if got.path != castleConsolePrefix+"Login" || got.method != http.MethodPost {
		t.Errorf("castle saw %s %s, want POST %sLogin", got.method, got.path, castleConsolePrefix)
	}
	if want := `{"username":"ops","password":"pw"}`; got.body != want {
		t.Errorf("castle got body %q, want %q", got.body, want)
	}
	if auth := got.headers.Get("Authorization"); auth != "Bearer tok" {
		t.Errorf("castle Authorization = %q, want the caller's", auth)
	}
	if ct := got.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("castle Content-Type = %q", ct)
	}
	if cpv := got.headers.Get("Connect-Protocol-Version"); cpv != "1" {
		t.Errorf("Connect-Protocol-Version not forwarded: %q", cpv)
	}
	if ctm := got.headers.Get("Connect-Timeout-Ms"); ctm != "5000" {
		t.Errorf("Connect-Timeout-Ms not forwarded: %q", ctm)
	}
	for _, dropped := range []string{"Cookie", "Origin", "X-Castle-Foo"} {
		if v := got.headers.Get(dropped); v != "" {
			t.Errorf("castle got loopback header %s: %q", dropped, v)
		}
	}
	// The outbound client brands its own requests; the caller's User-Agent
	// must not leak through.
	if ua := got.headers.Get("User-Agent"); ua == "sneaky/8" {
		t.Errorf("castle got the caller's User-Agent")
	}

	// Logout is forwarded too: revocation stays castle-side (KB-325). The
	// caller's console credential is what castle identifies the session
	// by, so it must reach castle on this hop unchanged.
	logs2 := &[]recordedCastleRequest{}
	console2 := newTestConsole(t, logs2, nil)
	code, body, _ = postConsole(t, console2.Handler(), "Logout", "application/json",
		[]byte(`{"sessionToken":"gone"}`), map[string]string{"Authorization": "Bearer st-123"})
	if code != http.StatusOK {
		t.Errorf("Logout: code=%d body=%s", code, body)
	}
	logged := &(*logs2)[len(*logs2)-1]
	if !strings.HasSuffix(logged.path, "/Logout") || logged.body != `{"sessionToken":"gone"}` {
		t.Errorf("castle saw Logout %+v", logged)
	}
	if got := logged.headers.Get("Authorization"); got != "Bearer st-123" {
		t.Errorf("castle saw Logout Authorization %q, want the caller's credential", got)
	}

	// Unknown procedures answer a CastleConsole-shaped error, not a
	// castle-proxied one.
	code, body, _ = postConsole(t, h, "Nope", "application/json", []byte(`{}`), nil)
	if code != http.StatusNotImplemented {
		t.Errorf("unknown proc: code=%d body=%s", code, body)
	}
	gcode, gmsg := readConnectErr(t, body)
	if gcode != connect.CodeUnimplemented.String() || !strings.Contains(gmsg, "no such console procedure") {
		t.Errorf("unknown proc = (%q, %q)", gcode, gmsg)
	}
}

// TestCopyConsoleResponseHeadersNonCanonical pins the response whitelist's
// case-insensitive matching directly: Go's HTTP client canonicalizes
// header names on read, so a lowercase key can never arrive through a
// round trip and only a direct helper call can exercise it.
func TestCopyConsoleResponseHeadersNonCanonical(t *testing.T) {
	src := http.Header{
		"grpc-status":         []string{"0"},
		"connect-custom-flag": []string{"on"},
		"set-cookie":          []string{"castle-cookie=1"},
		"server":              []string{"castle/1"},
	}
	dst := http.Header{}
	copyConsoleResponseHeaders(dst, src)
	// The whitelist decides on the source key case-insensitively, while
	// relayed values land under Go's canonicalized spelling of the key
	// (values pass through http.Header.Add). Scan dst case-insensitively
	// to judge matching, because dst.Get would canonicalize the lookup.
	valueOfFold := func(srcKey string) string {
		for k, vs := range dst {
			if strings.EqualFold(k, srcKey) && len(vs) > 0 {
				return vs[0]
			}
		}
		return ""
	}
	for _, relayed := range [][2]string{{"grpc-status", "0"}, {"connect-custom-flag", "on"}} {
		if got := valueOfFold(relayed[0]); got != relayed[1] {
			t.Errorf("%s relayed as %q, want %q (dst=%v)", relayed[0], got, relayed[1], dst)
		}
	}
	for _, dropped := range []string{"set-cookie", "server"} {
		if got := valueOfFold(dropped); got != "" {
			t.Errorf("%s must be dropped, got %q (dst=%v)", dropped, got, dst)
		}
	}
}

// TestConsoleResolveLocal pins decision delivery: ResolveResume never
// touches castle, decodes the JSON codec, answers the local control
// vocabulary through the resolver, and maps resolver connect errors
// code-for-code.
func TestConsoleResolveLocal(t *testing.T) {
	logs := &[]recordedCastleRequest{}
	res := &errResolver{accept: true}
	console := newTestConsole(t, logs, res)

	code, body, _ := postConsole(t, console.Handler(), "ResolveResume", "application/json",
		[]byte(`{"runId":"r-1","signal":"gate","payload":{"decision":"approve"}}`), nil)
	if code != http.StatusOK {
		t.Fatalf("resolve: code=%d body=%s", code, body)
	}
	var resp struct {
		IssuedAt string `json:"issuedAt"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("resolve body %q: %v", body, err)
	}
	issued, err := time.Parse(time.RFC3339, resp.IssuedAt)
	if err != nil || time.Since(issued) > time.Minute {
		t.Fatalf("issuedAt %q: %v", resp.IssuedAt, err)
	}
	if res.gotRunID != "r-1" || res.gotSig != "gate" || res.gotBody["decision"] != "approve" {
		t.Errorf("resolver saw runID=%q sig=%q payload=%v", res.gotRunID, res.gotSig, res.gotBody)
	}
	if len(*logs) != 0 {
		t.Errorf("ResolveResume reached castle: %+v", *logs)
	}

	// Rejections surface the local control vocabulary as
	// failed_precondition with an actionable message.
	for _, tc := range []struct {
		reason, wantMsg string
	}{
		{"run_not_paused", "the run is not paused"},
		{"no_pending_signal", "no pending signal"},
		{"signal_mismatch", "does not match the run's pending signal"},
		{"weird", "resolve rejected: weird"},
		{"", "did not accept"},
	} {
		res.accept, res.reason = false, tc.reason
		code, body, _ = postConsole(t, console.Handler(), "ResolveResume", "application/json",
			[]byte(`{"runId":"r-1","signal":"gate"}`), nil)
		if code != http.StatusBadRequest {
			t.Errorf("reason %q: code=%d", tc.reason, code)
		}
		gcode, gmsg := readConnectErr(t, body)
		if gcode != connect.CodeFailedPrecondition.String() || !strings.Contains(gmsg, tc.wantMsg) {
			t.Errorf("reason %q = (%q, %q)", tc.reason, gcode, gmsg)
		}
	}
	res.accept, res.reason = true, ""

	// A resolver transport error (a foreign run id) maps code-for-code.
	res.err = connect.NewError(connect.CodeNotFound, fmt.Errorf(`run "other" is not served by this control listener`))
	code, body, _ = postConsole(t, console.Handler(), "ResolveResume", "application/json",
		[]byte(`{"runId":"other","signal":"gate"}`), nil)
	if code != http.StatusNotFound {
		t.Errorf("foreign run: code=%d body=%s", code, body)
	}
	gcode, gmsg := readConnectErr(t, body)
	if gcode != connect.CodeNotFound.String() || !strings.Contains(gmsg, "not served") {
		t.Errorf("foreign run = (%q, %q)", gcode, gmsg)
	}

	// Malformed bodies get precise invalid_argument answers.
	code, body, _ = postConsole(t, console.Handler(), "ResolveResume", "application/json", []byte(`{"runId":7}`), nil)
	if code != http.StatusBadRequest || !strings.Contains(body, "invalid runId") {
		t.Errorf("non-string runId: code=%d body=%s", code, body)
	}
	code, body, _ = postConsole(t, console.Handler(), "ResolveResume", "application/json", []byte(`{}`), nil)
	if code != http.StatusBadRequest || !strings.Contains(body, "missing runId") {
		t.Errorf("missing runId: code=%d body=%s", code, body)
	}
	code, body, _ = postConsole(t, console.Handler(), "ResolveResume", "application/json", []byte(`not json`), nil)
	if code != http.StatusBadRequest || !strings.Contains(body, "invalid JSON request body") {
		t.Errorf("non-JSON body: code=%d body=%s", code, body)
	}
	code, body, _ = postConsole(t, console.Handler(), "ResolveResume", "application/json", []byte(`{"runId":"r-1","payload":5}`), nil)
	if code != http.StatusBadRequest || !strings.Contains(body, "payload") {
		t.Errorf("non-object payload: code=%d body=%s", code, body)
	}

	// Unsupported media types get refused with Accept-Post guidance.
	code, body, hdr := postConsole(t, console.Handler(), "ResolveResume", "application/xml", []byte(`<x/>`), nil)
	if code != http.StatusNotImplemented || !strings.Contains(body, "unsupported content-type") {
		t.Errorf("xml body: code=%d body=%s", code, body)
	}
	if ap := hdr.Get("Accept-Post"); !strings.Contains(ap, "application/json") || !strings.Contains(ap, "application/proto") {
		t.Errorf("Accept-Post = %q", ap)
	}

	// GET is refused: unary procedures are POST-only.
	req := httptest.NewRequest(http.MethodGet, castleConsolePrefix+"ResolveResume", http.NoBody)
	req.Host = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	console.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET resolve: code=%d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}

	// A nil resolver answers UNIMPLEMENTED rather than panicking.
	noResolver, err := NewCastleConsole("", nil, nil)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	code, body, _ = postConsole(t, noResolver.Handler(), "ResolveResume", "application/json", []byte(`{"runId":"r"}`), nil)
	if code != http.StatusNotImplemented || !strings.Contains(body, "no local run control") {
		t.Errorf("nil resolver: code=%d body=%s", code, body)
	}
}

// appendString appends a length-delimited proto string field.
func appendString(dst []byte, field int, val string) []byte {
	return appendBytes(dst, field, []byte(val))
}

// appendBytes appends a length-delimited proto bytes field.
func appendBytes(dst []byte, field int, val []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(field)<<3|2)
	dst = binary.AppendUvarint(dst, uint64(len(val)))
	return append(dst, val...)
}

// encodeConsoleResolveReq builds a binary ResolveResumeRequest: field 1
// run_id, field 2 signal, field 3 a map entry sub-message per pair.
func encodeConsoleResolveReq(runID, signal string, payload map[string]string) []byte {
	var buf []byte
	if runID != "" {
		buf = appendString(buf, 1, runID)
	}
	if signal != "" {
		buf = appendString(buf, 2, signal)
	}
	for k, v := range payload {
		var entry []byte
		entry = appendString(entry, 1, k)
		entry = appendString(entry, 2, v)
		buf = appendBytes(buf, 3, entry)
	}
	return buf
}

// decodeTimestampProto decodes a google.protobuf.Timestamp sub-message
// (field 1 seconds varint, field 2 nanos varint).
func decodeTimestampProto(msg []byte) (time.Time, error) {
	var sec, nano uint64
	for len(msg) > 0 {
		tag, n := binary.Uvarint(msg)
		if n <= 0 {
			return time.Time{}, fmt.Errorf("bad timestamp tag")
		}
		msg = msg[n:]
		field, wire := int(tag>>3), int(tag&0x7)
		if wire != 0 {
			return time.Time{}, fmt.Errorf("timestamp field %d wire %d", field, wire)
		}
		v, n := binary.Uvarint(msg)
		if n <= 0 {
			return time.Time{}, fmt.Errorf("bad timestamp varint")
		}
		msg = msg[n:]
		switch field {
		case 1:
			sec = v
		case 2:
			nano = v
		}
	}
	return time.Unix(int64(sec), int64(nano)).UTC(), nil
}

// TestConsoleResolveProto pins the binary codec: a raw proto
// ResolveResumeRequest decodes into the resolver call and the response
// encodes back as a Timestamp-bearing proto message; malformed bodies are
// rejected and unknown fields skipped.
func TestConsoleResolveProto(t *testing.T) {
	logs := &[]recordedCastleRequest{}
	res := &errResolver{accept: true}
	console := newTestConsole(t, logs, res)

	req := encodeConsoleResolveReq("r-9", "sig", map[string]string{"a": "1"})
	code, body, hdr := postConsole(t, console.Handler(), "ResolveResume", "application/proto", req, nil)
	if code != http.StatusOK {
		t.Fatalf("proto resolve: code=%d body=%q", code, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/proto" {
		t.Errorf("Content-Type = %q", ct)
	}
	if res.gotRunID != "r-9" || res.gotSig != "sig" || res.gotBody["a"] != "1" {
		t.Fatalf("resolver saw runID=%q sig=%q payload=%v", res.gotRunID, res.gotSig, res.gotBody)
	}
	if len(*logs) != 0 {
		t.Errorf("proto resolve reached castle: %+v", *logs)
	}
	issued, err := decodeIssuedAtProto([]byte(body))
	if err != nil {
		t.Fatalf("decode response %x: %v", body, err)
	}
	if d := time.Since(issued); d < -time.Minute || d > time.Minute {
		t.Errorf("issued_at = %v, want approximately now", issued)
	}

	// Malformed bodies are rejected outright. The payload-entry cases carry
	// a valid outer length, so they previously slipped past the
	// unknown-field skip: the inner error must reach the caller, and a
	// rejected body must never invoke the resolver.
	for name, bad := range map[string][]byte{
		"empty-typed":          {0x0a},                                              // field 1 length-delim with no length
		"group-wire":           {0x0b, 0x01},                                        // schema field 1, wire type 3 (group)
		"short-fixed64":        {0x09, 0x01},                                        // schema field 1, wire type 1, <8 bytes
		"short-fixed32":        {0x0d, 0x01},                                        // schema field 1, wire type 5, <4 bytes
		"run-id-wrong-wire":    {0x08, 0x01},                                        // schema field 1 as a varint, want length-delim
		"payload-bad-entry":    {0x1a, 0x02, 0x08, 0x01},                            // inner map entry with wire type 0
		"payload-after-run-id": {0x0a, 0x03, 'r', '-', '9', 0x1a, 0x02, 0x08, 0x01}, // valid run id, broken payload entry
	} {
		before := res.calls
		res.gotBody = nil
		code, body, _ = postConsole(t, console.Handler(), "ResolveResume", "application/proto", bad, nil)
		if code != http.StatusBadRequest || !strings.Contains(body, "invalid proto request body") {
			t.Errorf("%s: code=%d body=%s", name, code, body)
		}
		if res.calls != before || res.gotBody != nil {
			t.Errorf("%s: resolver invoked (calls %d -> %d, payload %v)", name, before, res.calls, res.gotBody)
		}
	}

	// Unknown fields (here: field 10, wire 0, varint 42) are skipped so
	// forward-compatible callers keep working.
	withUnknown := append([]byte{0x50, 0x2a}, req...)
	code, body, _ = postConsole(t, console.Handler(), "ResolveResume", "application/proto", withUnknown, nil)
	if code != http.StatusOK || res.gotRunID != "r-9" {
		t.Errorf("unknown-field skip: code=%d runID=%q body=%s", code, res.gotRunID, body)
	}
}

// decodeIssuedAtProto extracts issued_at (field 1, a nested Timestamp) from
// a hand-encoded ResolveResumeResponse.
func decodeIssuedAtProto(b []byte) (time.Time, error) {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return time.Time{}, fmt.Errorf("bad tag byte")
		}
		b = b[n:]
		field, wire := int(tag>>3), int(tag&0x7)
		if wire != 2 {
			return time.Time{}, fmt.Errorf("field %d wire %d", field, wire)
		}
		length, n := binary.Uvarint(b)
		if n <= 0 || uint64(len(b)-n) < length {
			return time.Time{}, fmt.Errorf("truncated field %d", field)
		}
		msg := b[n : n+int(length)]
		b = b[n+int(length):]
		if field != 1 {
			continue
		}
		return decodeTimestampProto(msg)
	}
	return time.Time{}, fmt.Errorf("no issued_at")
}

// TestCastleConsoleCtorValidation pins the base-URL contract: http(s)
// control-plane roots are accepted with their path/query/fragment/user
// data stripped, and anything else is refused with an actionable error.
func TestCastleConsoleCtorValidation(t *testing.T) {
	for _, good := range []string{
		"http://127.0.0.1:8080",
		"https://castle.internal:8443",
		"http://localhost:9000/",
	} {
		c, err := NewCastleConsole(good, nil, nil)
		if err != nil {
			t.Errorf("good %q: %v", good, err)
			continue
		}
		u, perr := url.Parse(good)
		if perr != nil {
			t.Errorf("parse %q: %v", good, perr)
			continue
		}
		if c.baseURL != u.Scheme+"://"+u.Host {
			t.Errorf("good %q normalized to %q, want %s://%s", good, c.baseURL, u.Scheme, u.Host)
		}
	}
	for _, bad := range []string{
		"castle.internal",                      // no scheme
		"ftp://castle.internal",                // unsupported scheme
		"http://127.0.0.1:8080/console/prefix", // path prefix
	} {
		if _, err := NewCastleConsole(bad, nil, nil); err == nil {
			t.Errorf("ctor accepted %q, want refusal", bad)
		}
	}
	// A query string without a path is accepted but stripped.
	c, err := NewCastleConsole("http://127.0.0.1:8080/?q=1", nil, nil)
	if err != nil || c.baseURL != "http://127.0.0.1:8080" {
		t.Errorf("query-only castle URL: base=%q err=%v", c.baseURL, err)
	}
}

// TestServerMountsConsole pins the Server integration: the console surface
// is reachable through the loopback-gated Handler, other routes are
// unaffected, and non-loopback Hosts are refused before touching castle.
func TestServerMountsConsole(t *testing.T) {
	logs := &[]recordedCastleRequest{}
	console := newTestConsole(t, logs, nil)
	res := &errResolver{accept: true}
	console.resolver = res
	srv := newTestServer(t)
	srv.WithCastleConsole(console.Handler())
	h := srv.Handler()

	code, body, _ := postConsole(t, h, "Login", "application/json", []byte(`{"username":"o","password":"p"}`), nil)
	if code != http.StatusOK || body != `{"sessionToken":"st-123","username":"ops"}` {
		t.Errorf("mounted Login: code=%d body=%s", code, body)
	}

	// The seam API keeps working next to the mount.
	if code, body := doJSON(t, h, "GET", "/runview/api/health"); code != http.StatusNoContent {
		t.Errorf("health after mount: code=%d body=%q", code, body)
	}

	// The loopback Host gate covers the console mount too.
	req := httptest.NewRequest(http.MethodPost, castleConsolePrefix+"Login", strings.NewReader(`{}`))
	req.Host = "attacker.example:5173"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback Host console login: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*logs) > 1 {
		t.Error("rejected request reached castle")
	}
}

// TestConsoleProxyRefusesRedirects pins the credential-replay control: a
// castle answering a redirect is passed through as the status it returned —
// the caller's Authorization is never replayed at the redirect target.
func TestConsoleProxyRefusesRedirects(t *testing.T) {
	replayed := &[]recordedCastleRequest{}
	steal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*replayed = append(*replayed, recordedCastleRequest{path: r.URL.Path, headers: r.Header.Clone()})
	}))
	t.Cleanup(steal.Close)
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", steal.URL+"/take-auth")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(redir.Close)
	c, err := NewCastleConsole(redir.URL, nil, nil)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	code, body, hdr := postConsole(t, c.Handler(), "Login", "application/json", []byte(`{}`),
		map[string]string{"Authorization": "Bearer st-x"})
	if code != http.StatusFound {
		t.Fatalf("redirected Login: code=%d body=%s", code, body)
	}
	if loc := hdr.Get("Location"); loc != "" {
		t.Errorf("redirect Location relayed outside the response whitelist: %q", loc)
	}
	if got := len(*replayed); got != 0 {
		t.Errorf("Authorization was replayed: %d request(s) reached %s", got, steal.URL)
	}
}

// TestConsoleRequestBodyCap pins the abuse bound on the credential
// surfaces: bodies past maxConsoleRequestBody answer invalid_argument and
// castle and the local resolver see nothing.
func TestConsoleRequestBodyCap(t *testing.T) {
	logs := &[]recordedCastleRequest{}
	res := &errResolver{accept: true}
	console := newTestConsole(t, logs, res)
	h := console.Handler()
	oversized := bytes.Repeat([]byte("a"), maxConsoleRequestBody+1)
	for _, proc := range []string{"Login", "ResolveResume"} {
		code, body, _ := postConsole(t, h, proc, "application/json", oversized, nil)
		if code != http.StatusBadRequest {
			t.Errorf("%s capped: code=%d body=%s", proc, code, body)
		}
		gcode, gmsg := readConnectErr(t, body)
		want := fmt.Sprintf("console request body exceeds %d bytes", maxConsoleRequestBody)
		if gcode != connect.CodeInvalidArgument.String() || !strings.Contains(gmsg, want) {
			t.Errorf("%s capped = (%q, %q), want invalid_argument %q", proc, gcode, gmsg, want)
		}
	}
	if got := len(*logs); got != 0 {
		t.Errorf("castle received %d request(s) for capped bodies", got)
	}
	if res.gotRunID != "" {
		t.Errorf("resolver reached with a capped body: runID %q", res.gotRunID)
	}
}

// TestConsoleListSessionsPassThrough pins the third forwarded verb and the
// response-header whitelist's positive side: ListSessions is relayed to
// castle verbatim (it runs revocation-aware session listing, KB-325), the
// Connect and gRPC protocol headers come back, and castle's server
// fingerprint does not.
func TestConsoleListSessionsPassThrough(t *testing.T) {
	logs := &[]recordedCastleRequest{}
	console := newTestConsole(t, logs, nil)
	code, body, hdr := postConsole(t, console.Handler(), "ListSessions", "application/json",
		[]byte(`{"sessionToken":"st-123"}`), nil)
	if code != http.StatusOK {
		t.Fatalf("ListSessions: code=%d body=%s", code, body)
	}
	if want := `{"sessions":[]}`; body != want {
		t.Errorf("ListSessions body = %q, want %q", body, want)
	}
	got := &(*logs)[len(*logs)-1]
	if got.path != castleConsolePrefix+"ListSessions" || got.method != http.MethodPost {
		t.Errorf("castle saw %s %s, want POST %sListSessions", got.method, got.path, castleConsolePrefix)
	}
	if want := `{"sessionToken":"st-123"}`; got.body != want {
		t.Errorf("castle got body %q, want %q", got.body, want)
	}
	// Whitelisted protocol headers survive the hop.
	for name, want := range map[string]string{"Content-Type": "application/json", "Grpc-Status": "0", "Connect-Custom-Flag": "on"} {
		if v := hdr.Get(name); v != want {
			t.Errorf("response header %s = %q, want %q", name, v, want)
		}
	}
	// Castle's server fingerprint does not.
	if v := hdr.Get("Server"); v != "" {
		t.Errorf("castle Server fingerprint relayed: %q", v)
	}
}
