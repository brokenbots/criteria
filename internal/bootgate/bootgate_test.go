package bootgate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testProbeTimeout = 2 * time.Second

// lookupFromMap builds a Configure lookup over a fixed env map.
func lookupFromMap(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestConfigureDisabledWhenAllEnvsUnset(t *testing.T) {
	cfg, enabled, err := Configure(lookupFromMap(nil), testProbeTimeout)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if enabled {
		t.Fatal("gate must be disabled when no gate envs are present")
	}
	if cfg != (Config{}) {
		t.Fatalf("disabled gate must return a zero config, got %+v", cfg)
	}
}

func TestConfigureDisabledWhenEnvsWhitespaceOnly(t *testing.T) {
	env := map[string]string{
		EnvOperatorViewURL: "   ",
		EnvRunTicket:       "\t",
		EnvRunJob:          " ",
	}
	cfg, enabled, err := Configure(lookupFromMap(env), testProbeTimeout)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if enabled {
		t.Fatal("whitespace-only gate envs must keep the gate disabled")
	}
	if cfg != (Config{}) {
		t.Fatalf("disabled gate must return a zero config, got %+v", cfg)
	}
}

func TestConfigureIdentityValuesTrimmed(t *testing.T) {
	env := map[string]string{
		EnvOperatorViewURL: "  http://view.test/api  ",
		EnvRunTicket:       " kb-234 ",
		EnvRunJob:          "criteria-runner-job",
	}
	cfg, enabled, err := Configure(lookupFromMap(env), testProbeTimeout)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !enabled {
		t.Fatal("gate must be enabled")
	}
	if cfg.ViewURL != "http://view.test/api" {
		t.Errorf("ViewURL = %q, want trimmed value", cfg.ViewURL)
	}
	if cfg.Ticket != "kb-234" {
		t.Errorf("Ticket = %q, want trimmed value", cfg.Ticket)
	}
	if cfg.Job != "criteria-runner-job" {
		t.Errorf("Job = %q, want configured value", cfg.Job)
	}
	if cfg.Timeout != testProbeTimeout {
		t.Errorf("Timeout = %s, want %s", cfg.Timeout, testProbeTimeout)
	}
}

func TestConfigurePartialConfigIsLoudError(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"view URL without identity", map[string]string{EnvOperatorViewURL: "http://view.test"}},
		{"identity without view URL", map[string]string{EnvRunTicket: "kb-234"}},
		{"job without view URL", map[string]string{EnvRunJob: "criteria-runner-job"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, enabled, err := Configure(lookupFromMap(tc.env), testProbeTimeout)
			if err == nil {
				t.Fatal("partial gate configuration must fail loudly")
			}
			if enabled {
				t.Error("failed configuration must not enable the gate")
			}
			if !strings.Contains(err.Error(), "boot gate") {
				t.Errorf("error %q must attribute the failure to the boot gate", err)
			}
		})
	}
}

func TestConfigureMalformedViewURLAndTimeout(t *testing.T) {
	if _, _, err := Configure(lookupFromMap(map[string]string{
		EnvOperatorViewURL: "http://view.test",
		EnvRunTicket:       "kb-234",
	}), -1); err == nil {
		t.Error("non-positive probe timeout must fail loudly")
	}
	if _, _, err := Configure(lookupFromMap(map[string]string{
		EnvOperatorViewURL: "://no-scheme.test",
		EnvRunTicket:       "kb-234",
	}), testProbeTimeout); err == nil {
		t.Error("malformed view URL must fail loudly")
	}
}

func TestOutcomeBlocksIsTheGatePredicate(t *testing.T) {
	cases := []struct {
		outcome Outcome
		blocks  bool
	}{
		{OutcomeLive, false},
		{OutcomeUnknown, false},
		{OutcomeTerminal, true},
		{OutcomeDeleted, true},
	}
	for _, tc := range cases {
		if got := tc.outcome.Blocks(); got != tc.blocks {
			t.Errorf("Outcome(%s).Blocks() = %v, want %v", tc.outcome, got, tc.blocks)
		}
	}
}

func TestClassifyMapsOperatorViewResponses(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		want       Outcome
		wantDetail string
	}{
		{"non-terminal phase", http.StatusOK, `{"phase":"Provisioning","terminal":false}`, OutcomeLive, "Provisioning"},
		{"terminal phase", http.StatusOK, `{"phase":"Succeeded","terminal":true}`, OutcomeTerminal, "Succeeded"},
		{"malformed payload", http.StatusOK, `{"phase":`, OutcomeUnknown, ""},
		{"wrong payload shape", http.StatusOK, `[1,2,3]`, OutcomeUnknown, ""},
		{"deleted CR", http.StatusNotFound, "not found", OutcomeDeleted, ""},
		{"server error", http.StatusInternalServerError, "boom", OutcomeUnknown, ""},
		{"unauthorized", http.StatusForbidden, "denied", OutcomeUnknown, ""},
		{"teapot", http.StatusTeapot, "no", OutcomeUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := classify(tc.status, []byte(tc.body))
			if got != tc.want {
				t.Fatalf("classify(%d, %q) = %s, want %s", tc.status, tc.body, got, tc.want)
			}
			if tc.want != OutcomeUnknown {
				if detail != tc.wantDetail {
					t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
				}
			} else if tc.name == "server error" && detail != "operator view returned HTTP 500" {
				t.Errorf("unknown detail = %q, want status summary", detail)
			}
		})
	}
}

func TestCheckAgainstLiveOperatorView(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		want       Outcome
		wantDetail string
	}{
		{"live", http.StatusOK, `{"phase":"Provisioning","terminal":false}`, OutcomeLive, "Provisioning"},
		{"terminal", http.StatusOK, `{"phase":"Failed","terminal":true}`, OutcomeTerminal, "Failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			cfg := Config{ViewURL: srv.URL, Ticket: "kb-234", Job: "criteria-runner-job", Timeout: testProbeTimeout}
			outcome, detail := cfg.Check(context.Background())
			if outcome != tc.want {
				t.Fatalf("Check() = %s, want %s", outcome, tc.want)
			}
			if detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
			}
			wantQuery := "job=criteria-runner-job&ticket=kb-234"
			if gotQuery != wantQuery {
				t.Errorf("probe sent query %q, want %q", gotQuery, wantQuery)
			}
		})
	}
}

func TestCheckOmitsAbsentIdentityParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"phase":"Provisioning","terminal":false}`))
	}))
	defer srv.Close()

	cfg := Config{ViewURL: srv.URL, Ticket: "kb-234", Timeout: testProbeTimeout}
	if outcome, _ := cfg.Check(context.Background()); outcome != OutcomeLive {
		t.Fatalf("Check() = %s, want live", outcome)
	}
	if gotQuery != "ticket=kb-234" {
		t.Errorf("probe sent query %q, want %q", gotQuery, "ticket=kb-234")
	}
}

func TestCheckDeletedCRIsBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no CriteriaRun for identity"}`))
	}))
	defer srv.Close()

	cfg := Config{ViewURL: srv.URL, Ticket: "kb-234", Job: "criteria-runner-job", Timeout: testProbeTimeout}
	outcome, _ := cfg.Check(context.Background())
	if !outcome.Blocks() {
		t.Fatalf("Check() = %s, want a blocked outcome (deleted while pod alive)", outcome)
	}
}

func TestCheckUnreachableViewIsUnknownNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close() // nothing will listen on the port anymore

	cfg := Config{ViewURL: closedURL, Ticket: "kb-234", Job: "criteria-runner-job", Timeout: testProbeTimeout}
	outcome, detail := cfg.Check(context.Background())
	if outcome != OutcomeUnknown {
		t.Fatalf("Check() = %s, want unknown for transport failure", outcome)
	}
	if detail == "" {
		t.Fatal("unknown outcome must carry a failure detail for the log")
	}
}

func TestCheckSlowViewTimesOutToUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"phase":"Provisioning","terminal":false}`))
	}))
	defer srv.Close()

	cfg := Config{ViewURL: srv.URL, Ticket: "kb-234", Timeout: 10 * time.Millisecond}
	outcome, detail := cfg.Check(context.Background())
	if outcome != OutcomeUnknown {
		t.Fatalf("Check() = %s, want unknown for a view that exceeds the probe timeout", outcome)
	}
	if detail == "" {
		t.Fatal("unknown outcome must carry a failure detail for the log")
	}
}

func TestCheckOversizedViewPayloadIsMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"phase":"%s","terminal":false}`, strings.Repeat("x", maxViewBodyBytes+1024))
	}))
	defer srv.Close()

	cfg := Config{ViewURL: srv.URL, Ticket: "kb-234", Timeout: testProbeTimeout}
	outcome, detail := cfg.Check(context.Background())
	if outcome != OutcomeUnknown {
		t.Fatalf("Check() = %s, want unknown for an oversized payload", outcome)
	}
	if detail == "" {
		t.Fatal("unknown outcome must carry a failure detail for the log")
	}
}

func TestCheckOversizedPaddingAfterValidJSONIsMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"phase":"Provisioning","terminal":false}%s`, strings.Repeat(" ", maxViewBodyBytes+1024))
	}))
	defer srv.Close()

	cfg := Config{ViewURL: srv.URL, Ticket: "kb-234", Timeout: testProbeTimeout}
	outcome, _ := cfg.Check(context.Background())
	if outcome != OutcomeUnknown {
		t.Fatalf("Check() = %s, want unknown for a payload padded past the size cap", outcome)
	}
}

func TestOutcomeStringRendersAllOutcomes(t *testing.T) {
	cases := map[Outcome]string{
		OutcomeLive:     "live",
		OutcomeTerminal: "terminal",
		OutcomeDeleted:  "deleted",
		OutcomeUnknown:  "unknown",
	}
	for outcome, want := range cases {
		if got := outcome.String(); got != want {
			t.Errorf("Outcome.String() = %q, want %q", got, want)
		}
	}
	if got := Outcome(42).String(); got == "" {
		t.Error("unknown outcome values must still render for logs")
	}
}
