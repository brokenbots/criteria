// Package bootgate implements the KB-234 runner boot gate: on every
// server-mode start (fresh or adoption), the runner resolves its operator
// identity and consults the operator's view of its CriteriaRun before any
// castle interaction. If the operator reports the CR terminal, or reports no
// CR at all while this pod is still alive (deleted under the runner), the
// boot must exit 0 without registering with castle or creating a run: a
// start the operator will never provision for would otherwise mint a
// pending-forever castle run (the KB-225 boot-1 orphan).
//
// The gate is a deterministic, mechanical check derived from the CR object
// at boot time — never from castle run history — so it also covers fresh
// starts that have no run identity to ask castle about. Rule 3 of KB-233
// (the checkpoint/reattach path in internal/cli) remains the adoption-side
// half of the contract; this package is its fresh-start half.
//
// The operator view surface is deliberately self-contained: the
// orchestrator injects three environment variables into the runner Job
// (EnvOperatorViewURL, EnvRunTicket, EnvRunJob) and serves a tiny JSON view
// document for the identity. Nothing about k8s reaches this package, and no
// credentials flow through the view request — the URL is operator-supplied
// infrastructure configuration at the same trust level as
// CRITERIA_SERVER_URL.
package bootgate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Environment variables the orchestrator injects into a runner Job. All three
// unset means the gate is disabled (standalone/local mode and legacy
// orchestrators); a partial set is a loud configuration error, because an
// operator that injects identity but no view URL (or the reverse) has a
// broken gate deployment that must not silently degrade to no gate.
const (
	// EnvOperatorViewURL is the base URL of the operator view endpoint that
	// reports this runner's CriteriaRun phase.
	EnvOperatorViewURL = "CRITERIA_OPERATOR_VIEW_URL"
	// EnvRunTicket is the ticket identity of the CR this runner serves.
	EnvRunTicket = "CRITERIA_RUN_TICKET"
	// EnvRunJob is the runner Job name the operator created for the CR.
	EnvRunJob = "CRITERIA_RUN_JOB"
)

// maxViewBodyBytes bounds the operator view payload. The document is a tiny
// JSON object, so anything larger is treated as malformed rather than read.
const maxViewBodyBytes = 64 * 1024

// Outcome is the gate's verdict about a boot start.
type Outcome int

const (
	// OutcomeLive means the operator still reports a non-terminal CR: the
	// boot may proceed and interact with castle normally.
	OutcomeLive Outcome = iota
	// OutcomeTerminal means the operator reports the CR in a terminal
	// phase: the boot must exit 0 without any castle interaction.
	OutcomeTerminal
	// OutcomeDeleted means the operator reports no CR for this runner's
	// identity (deleted while this pod is still alive): the boot must exit
	// 0 without any castle interaction.
	OutcomeDeleted
	// OutcomeUnknown means the operator view was unavailable (transport
	// error, unexpected status, malformed payload). The boot proceeds
	// fail-open with a loud warning: the view failing is an availability
	// problem, not an operator statement that the CR is dead. Residual
	// orphans are bounded by the KB-233 backstops (rule 1 deletes runner
	// Job pods on terminal CRs; castle rule 2 reaps created-never-started
	// runs).
	OutcomeUnknown
)

// Blocks reports whether the gate outcome must stop the boot from
// registering with castle: the boot exits 0 instead.
func (o Outcome) Blocks() bool {
	return o == OutcomeTerminal || o == OutcomeDeleted
}

// String renders the outcome for structured logs.
func (o Outcome) String() string {
	switch o {
	case OutcomeLive:
		return "live"
	case OutcomeTerminal:
		return "terminal"
	case OutcomeDeleted:
		return "deleted"
	case OutcomeUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// View is the operator's mechanical view of one CriteriaRun at query time.
// Terminal is the operator's own verdict derived from the CR object —
// criteria does not second-guess operator phase semantics; Phase is echoed
// only for structured logging.
type View struct {
	Phase    string `json:"phase"`
	Terminal bool   `json:"terminal"`
}

// Config is a resolved gate configuration.
type Config struct {
	// ViewURL is the base URL of the operator view endpoint.
	ViewURL string
	// Ticket is the CR ticket identity (EnvRunTicket), possibly empty when
	// the operator identifies runs by job only.
	Ticket string
	// Job is the runner Job name (EnvRunJob), possibly empty when the
	// operator identifies runs by ticket only.
	Job string
	// Timeout bounds the whole view request; always positive when
	// Configure succeeds.
	Timeout time.Duration
}

// Configure resolves the gate configuration from an environment lookup.
// It returns enabled=false when none of the gate envs are present (gate
// disabled: no behavior change for standalone runs), and a non-nil error
// on partial configuration, a malformed view URL, or a non-positive probe
// timeout — an operator that enables the gate must get a working gate or a
// loud failure, never a silently skipped check.
func Configure(lookup func(string) (string, bool), probeTimeout time.Duration) (Config, bool, error) {
	viewURL := envValue(lookup(EnvOperatorViewURL))
	ticket := envValue(lookup(EnvRunTicket))
	job := envValue(lookup(EnvRunJob))

	if viewURL == "" && ticket == "" && job == "" {
		return Config{}, false, nil
	}
	if probeTimeout <= 0 {
		return Config{}, false, fmt.Errorf("boot gate: probe timeout must be positive, got %s", probeTimeout)
	}
	if ticket == "" && job == "" {
		return Config{}, false, fmt.Errorf("boot gate: %s set but no runner identity present; set %s and/or %s", EnvOperatorViewURL, EnvRunTicket, EnvRunJob)
	}
	if viewURL == "" {
		return Config{}, false, fmt.Errorf("boot gate: runner identity present (%s and/or %s) but %s unset", EnvRunTicket, EnvRunJob, EnvOperatorViewURL)
	}
	if _, err := url.Parse(viewURL); err != nil {
		return Config{}, false, fmt.Errorf("boot gate: %s is not a valid URL %q: %w", EnvOperatorViewURL, viewURL, err)
	}
	return Config{ViewURL: viewURL, Ticket: ticket, Job: job, Timeout: probeTimeout}, true, nil
}

// ConfigureFromEnv resolves the gate configuration from the process
// environment.
func ConfigureFromEnv(probeTimeout time.Duration) (Config, bool, error) {
	return Configure(os.LookupEnv, probeTimeout)
}

// Check probes the operator view for this runner's identity and returns the
// gate outcome. The second return is the operator's phase for live and
// terminal verdicts, or a failure detail for unknown. Check never returns a
// transport error: unavailability is a gate outcome (OutcomeUnknown), not a
// caller error, so the fail-open posture stays explicit at the call site.
func (c Config) Check(ctx context.Context) (outcome Outcome, detail string) {
	viewURL, err := probeURL(c.ViewURL, c.Ticket, c.Job)
	if err != nil {
		// Unreachable for a URL Configure already accepted; kept defensive
		// so a future call-site refactor cannot panic the boot.
		return OutcomeUnknown, fmt.Sprintf("invalid view URL: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, viewURL, http.NoBody)
	if err != nil {
		return OutcomeUnknown, fmt.Sprintf("operator view request: %v", err)
	}
	client := &http.Client{Timeout: c.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return OutcomeUnknown, fmt.Sprintf("operator view request failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxViewBodyBytes+1))
	if err != nil {
		return OutcomeUnknown, fmt.Sprintf("operator view read failed: %v", err)
	}
	return classify(resp.StatusCode, body)
}

// classify is the gate predicate over one operator view response: it maps
// the HTTP status and payload onto the boot outcome.
func classify(status int, body []byte) (outcome Outcome, detail string) {
	switch status {
	case http.StatusOK:
		var v View
		if err := json.Unmarshal(body, &v); err != nil {
			return OutcomeUnknown, fmt.Sprintf("malformed operator view payload: %v", err)
		}
		if v.Terminal {
			return OutcomeTerminal, v.Phase
		}
		return OutcomeLive, v.Phase
	case http.StatusNotFound:
		return OutcomeDeleted, ""
	default:
		return OutcomeUnknown, fmt.Sprintf("operator view returned HTTP %d", status)
	}
}

// probeURL appends the known identity parameters to the view URL. Empty
// identity components are omitted so the endpoint can key on either field.
func probeURL(base, ticket, job string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", base, err)
	}
	q := u.Query()
	if ticket != "" {
		q.Set("ticket", ticket)
	}
	if job != "" {
		q.Set("job", job)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// envValue resolves one env entry, treating an absent value and a
// whitespace-only value the same way: unset.
func envValue(raw string, ok bool) string {
	if !ok {
		return ""
	}
	return strings.TrimSpace(raw)
}
