package main

// header_conformance_test.go — the echo fixture's HTTP Server Validation:
// the Mcp-Method/Mcp-Name/Mcp-Param-* rules checked against the request
// body (name-bearing methods mirror params.name/params.uri; everything
// else carries no name), and the -32020 JSON-RPC error envelope a
// mismatch answers with instead of a plain-text 400.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
)

// newHeaderTestServer drives the fixture's HTTP face in-process: the same
// handleHTTPRequest the binary listener serves, so the header rules run
// without spawning the fixture process.
func newHeaderTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handleHTTPRequest))
	t.Cleanup(srv.Close)
	return srv
}

// postEnvelope POSTs one envelope body with the given headers and returns
// the status and body the fixture answered.
func postEnvelope(t *testing.T, srv *httptest.Server, payload string, headers map[string]string) (status int, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read reply: %v", readErr)
	}
	respBody := string(raw)
	return resp.StatusCode, respBody
}

// assertViolation pins the Server Validation failure shape: a 400 whose
// body is the JSON-RPC error envelope carrying the mismatch code.
func assertViolation(t *testing.T, status int, body string) {
	t.Helper()
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s, want 400", status, body)
	}
	var envelope struct {
		Error struct {
			Code int    `json:"code"`
			Msg  string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("body %s not a JSON-RPC envelope: %v", body, err)
	}
	if envelope.Error.Code != headerViolationCode {
		t.Fatalf("error code = %d, want %d (body %s)", envelope.Error.Code, headerViolationCode, body)
	}
}

func TestHTTPHeaderValidation_NameBearingMethods(t *testing.T) {
	srv := newHeaderTestServer(t)

	toolsCall := func(extra map[string]string, arguments string) (int, string) {
		payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":%s}}`, arguments)
		headers := map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "echo"}
		for name, value := range extra {
			headers[name] = value
		}
		return postEnvelope(t, srv, payload, headers)
	}

	if status, body := toolsCall(nil, `{}`); status >= 400 {
		t.Fatalf("conformant tools/call rejected: %d %s", status, body)
	}

	// A header/body name desync answers the -32020 envelope.
	status, body := toolsCall(map[string]string{"Mcp-Name": "other"}, `{}`)
	assertViolation(t, status, body)

	// A name-bearing method without the name is a violation.
	payload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`
	status, body = postEnvelope(t, srv, payload, map[string]string{"Mcp-Method": "tools/call"})
	assertViolation(t, status, body)

	// Mcp-Method vs body method desync is a violation too.
	status, body = postEnvelope(t, srv, payload, map[string]string{"Mcp-Method": "tools/list", "Mcp-Name": "echo"})
	assertViolation(t, status, body)

	// resources/read mirrors params.uri.
	status, body = postEnvelope(t, srv, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"memory://a"}}`,
		map[string]string{"Mcp-Method": "resources/read", "Mcp-Name": "memory://a"})
	if status >= 400 {
		t.Fatalf("resources/read with the matching uri rejected: %d %s", status, body)
	}
	status, body = postEnvelope(t, srv, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"memory://a"}}`,
		map[string]string{"Mcp-Method": "resources/read", "Mcp-Name": "memory://b"})
	assertViolation(t, status, body)
}

func TestHTTPHeaderValidation_NonNameBearingMethods(t *testing.T) {
	srv := newHeaderTestServer(t)

	// Every non-name-bearing method must carry no Mcp-Name.
	cases := []struct{ method, payload string }{
		{"server/discover", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`},
		{"tools/list", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`},
		{"notifications/initialized", `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`},
	}
	for _, tc := range cases {
		status, body := postEnvelope(t, srv, tc.payload, map[string]string{"Mcp-Method": tc.method, "Mcp-Name": "criteria-adapter-mcp"})
		assertViolation(t, status, body)
	}
	// The same methods pass without the name (the routing core answers the
	// method, not the header gate).
	status, body := postEnvelope(t, srv, cases[1].payload, map[string]string{"Mcp-Method": "tools/list"})
	if status >= 400 {
		t.Fatalf("tools/list without a name rejected: %d %s", status, body)
	}
}

func TestHTTPHeaderValidation_ParamMirrors(t *testing.T) {
	// The annotation knob drives both the advertised schema and the
	// validation table; restore it for sibling tests.
	saved := headerSchemaKind
	headerSchemaKind = headerSchemaMarker
	t.Cleanup(func() { headerSchemaKind = saved })

	srv := newHeaderTestServer(t)
	post := func(headers map[string]string, arguments string) (int, string) {
		payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":%s}}`, arguments)
		set := map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "echo"}
		for name, value := range headers {
			set[name] = value
		}
		return postEnvelope(t, srv, payload, set)
	}

	// The required mirror, matching the argument value, passes.
	if status, body := post(map[string]string{"Mcp-Param-X-Trace": "v1"}, `{"trace":{"X-Trace":"v1"}}`); status >= 400 {
		t.Fatalf("mirrored call rejected: %d %s", status, body)
	}

	// A value mismatching the argument is a violation.
	status, body := post(map[string]string{"Mcp-Param-X-Trace": "other"}, `{"trace":{"X-Trace":"v1"}}`)
	assertViolation(t, status, body)

	// A missing required mirror is a violation.
	status, body = post(nil, `{"trace":{"X-Trace":"v1"}}`)
	assertViolation(t, status, body)

	// A mirror outside the declared set is a violation.
	status, body = post(map[string]string{"Mcp-Param-X-Trace": "v1", "Mcp-Param-X-Other": "x"}, `{"trace":{"X-Trace":"v1"}}`)
	assertViolation(t, status, body)

	// A mirror whose argument is absent is a violation.
	status, body = post(map[string]string{"Mcp-Param-X-Trace": "v1"}, `{}`)
	assertViolation(t, status, body)

	// A non-header-safe argument value travels in the Base64 sentinel and
	// still matches after decoding (é is outside the header-safe set).
	value := "caf\u00e9"
	sentinel := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
	if got := mcpclient.DecodeHeaderValue(sentinel); got != value {
		t.Fatalf("sentinel decode = %q, want %q", got, value)
	}
	if status, body := post(map[string]string{"Mcp-Param-X-Trace": sentinel}, fmt.Sprintf(`{"trace":{"X-Trace":%q}}`, value)); status >= 400 {
		t.Fatalf("sentinel-mirrored call rejected: %d %s", status, body)
	}

	// With the annotation knob off the declared set is empty: any mirror
	// is rejected.
	headerSchemaKind = headerSchemaNone
	status, body = post(map[string]string{"Mcp-Param-X-Trace": "v1"}, `{"trace":{"X-Trace":"v1"}}`)
	assertViolation(t, status, body)
}

func TestHTTPHeaderValidation_ParamMirrors_PropertyShape(t *testing.T) {
	// The property-name spelling: the arguments object carries a property
	// NAMED x-mcp-header whose value table the mirror set reads from; the
	// same Server Validation rules answer with the same vectors.
	saved := headerSchemaKind
	headerSchemaKind = headerSchemaProperty
	t.Cleanup(func() { headerSchemaKind = saved })

	srv := newHeaderTestServer(t)
	post := func(headers map[string]string, arguments string) (int, string) {
		payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":%s}}`, arguments)
		set := map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "echo"}
		for name, value := range headers {
			set[name] = value
		}
		return postEnvelope(t, srv, payload, set)
	}

	// The required mirror matches the value at the property-named path.
	if status, body := post(map[string]string{"Mcp-Param-X-Trace": "v1"}, `{"x-mcp-header":{"X-Trace":"v1"}}`); status >= 400 {
		t.Fatalf("property-shape mirrored call rejected: %d %s", status, body)
	}

	// Mismatch against the property-named value.
	status, body := post(map[string]string{"Mcp-Param-X-Trace": "other"}, `{"x-mcp-header":{"X-Trace":"v1"}}`)
	assertViolation(t, status, body)

	// Missing required mirror.
	status, body = post(nil, `{"x-mcp-header":{"X-Trace":"v1"}}`)
	assertViolation(t, status, body)

	// Mirror outside the declared name set.
	status, body = post(map[string]string{"Mcp-Param-X-Trace": "v1", "Mcp-Param-X-Other": "x"}, `{"x-mcp-header":{"X-Trace":"v1"}}`)
	assertViolation(t, status, body)

	// Mirror with no argument value at the property-named path.
	status, body = post(map[string]string{"Mcp-Param-X-Trace": "v1"}, `{}`)
	assertViolation(t, status, body)

	// A non-header-safe value rides the Base64 sentinel and still matches.
	value := "caf\u00e9"
	sentinel := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
	if status, body := post(map[string]string{"Mcp-Param-X-Trace": sentinel}, fmt.Sprintf(`{"x-mcp-header":{"X-Trace":%q}}`, value)); status >= 400 {
		t.Fatalf("property-shape sentinel-mirrored call rejected: %d %s", status, body)
	}
}

func TestHTTPHeaderValidation_HeaderDesyncOnAnnotatedTool(t *testing.T) {
	saved := headerSchemaKind
	headerSchemaKind = headerSchemaMarker
	t.Cleanup(func() { headerSchemaKind = saved })
	srv := newHeaderTestServer(t)

	// The method/value desync keeps the -32020 answer with the annotated
	// tool too (no plaintext-400 path).
	status, body := postEnvelope(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"trace":{"X-Trace":"v1"}}}}`,
		map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "criteria-adapter-mcp"})
	assertViolation(t, status, body)
}
