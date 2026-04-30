// Package localresume implements local-mode resolution for approval and
// signal-wait nodes. It provides four modes controlled by the
// CRITERIA_LOCAL_APPROVAL environment variable:
//
//   - stdin       — interactive TTY prompt; reads y/yes → approved, n/no → rejected.
//   - file        — operator writes a JSON decision file out-of-band; engine polls.
//   - env         — reads CRITERIA_APPROVAL_<NODE> / CRITERIA_SIGNAL_<NODE>.
//   - auto-approve — auto-approves with a warning log; for unattended pipelines.
//
// Decisions are persisted under ~/.criteria/runs/<runID>/approvals/<node>.json
// for reattach safety. On reattach, the persisted decision is reused without
// re-prompting the operator.
package localresume

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Mode is the local approval resolution mode selected by CRITERIA_LOCAL_APPROVAL.
type Mode string

const (
	ModeStdin       Mode = "stdin"
	ModeFile        Mode = "file"
	ModeEnv         Mode = "env"
	ModeAutoApprove Mode = "auto-approve"
)

// ParseMode parses the CRITERIA_LOCAL_APPROVAL env var value.
// Returns an error if the value is not one of the four valid modes.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeStdin, ModeFile, ModeEnv, ModeAutoApprove:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("unrecognised CRITERIA_LOCAL_APPROVAL=%q (want stdin|file|env|auto-approve)", s)
	}
}

// LocalResumer resolves approval and signal-wait pauses in local mode.
type LocalResumer interface {
	// ResumeApproval blocks until a decision is available for the approval node
	// identified by name within run runID, or returns an error. The returned
	// map has key "decision" set to "approved" or "rejected", matching the
	// payload shape the engine expects from an orchestrator-delivered ResumePayload.
	ResumeApproval(ctx context.Context, runID, name string, approvers []string, reason string) (map[string]string, error)

	// ResumeSignal blocks until a signal payload for the wait node nodeName is
	// available. The returned map has key "outcome" set to the selected outcome
	// name, which the engine uses to look up the transition target.
	// validOutcomes is the set of outcome names declared in the wait node's HCL
	// on{} blocks; an unknown non-empty outcome is rejected before the engine
	// can fall back to the first declared transition.
	ResumeSignal(ctx context.Context, runID, nodeName, signalName string, validOutcomes []string) (map[string]string, error)
}

// Options configures the resumer. All fields are optional; zero values produce
// production-appropriate defaults.
type Options struct {
	// Stdin is the reader for interactive prompts (stdin mode). Defaults to os.Stdin.
	Stdin io.Reader
	// Stderr is the writer for interactive prompts (stdin mode). Defaults to os.Stderr.
	Stderr io.Writer
	// FilePollingInterval controls how often the file-mode poller checks for
	// the decision file. Defaults to 2s. Set to a short value in tests.
	FilePollingInterval time.Duration
	// FileTimeout is the maximum time file-mode will wait for the operator to
	// write the decision file before failing the run. Defaults to 1h.
	// Configurable via CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT.
	FileTimeout time.Duration
	// Log is the structured logger. Defaults to slog.Default().
	Log *slog.Logger

	// DecisionPathFn returns the persisted-decision file path for a given runID
	// and node. When non-nil it takes precedence over StateDir-based derivation.
	// buildLocalResumer in apply.go injects ApprovalDecisionPath from local_state.go.
	DecisionPathFn func(runID, nodeName string) (string, error)
	// RequestPathFn returns the operator-written request file path for a given
	// runID and node. When non-nil it takes precedence over StateDir-based
	// derivation. buildLocalResumer injects ApprovalRequestPath from local_state.go.
	RequestPathFn func(runID, nodeName string) (string, error)
	// StateDir overrides the state directory used when DecisionPathFn/RequestPathFn
	// are not provided (unit tests only). Defaults to CRITERIA_STATE_DIR or ~/.criteria.
	StateDir string
}

func (o *Options) applyDefaults() {
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.FilePollingInterval == 0 {
		o.FilePollingInterval = 2 * time.Second
	}
	if o.FileTimeout == 0 {
		o.FileTimeout = time.Hour
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
}

// New creates a LocalResumer for the given mode and options.
func New(mode Mode, opts Options) LocalResumer { //nolint:gocritic // Options is a config struct; callers pass by value intentionally
	opts.applyDefaults()
	return &resumer{mode: mode, opts: opts}
}

// persistedDecision is the on-disk format for a persisted approval or signal decision.
type persistedDecision struct {
	Decision  string `json:"decision,omitempty"` // for approvals
	Outcome   string `json:"outcome,omitempty"`  // for signal waits
	DecidedAt string `json:"decided_at"`
}

type resumer struct {
	mode Mode
	opts Options
}

// ResumeApproval resolves an approval node using the configured mode.
// It checks for a persisted decision first (reattach safety).
func (r *resumer) ResumeApproval(ctx context.Context, runID, name string, approvers []string, reason string) (map[string]string, error) {
	// Check for a persisted decision from a previous attempt (reattach safety).
	if payload, ok := r.loadPersistedApproval(runID, name); ok {
		r.opts.Log.Info("local-approval: using persisted decision", "node", name, "decision", payload["decision"])
		return payload, nil
	}

	var payload map[string]string
	var err error

	switch r.mode {
	case ModeStdin:
		payload, err = r.resolveApprovalStdin(ctx, name, approvers, reason)
	case ModeFile:
		payload, err = r.resolveApprovalFile(ctx, runID, name)
	case ModeEnv:
		payload, err = r.resolveApprovalEnv(name)
	case ModeAutoApprove:
		payload = r.resolveApprovalAutoApprove(name)
	default:
		return nil, fmt.Errorf("unknown local approval mode %q", r.mode)
	}
	if err != nil {
		return nil, err
	}

	if persistErr := r.persistDecision(runID, name, &persistedDecision{
		Decision:  payload["decision"],
		DecidedAt: time.Now().UTC().Format(time.RFC3339),
	}); persistErr != nil {
		r.opts.Log.Warn("local-approval: failed to persist decision; reattach safety degraded",
			"node", name, "error", persistErr)
	}
	return payload, nil
}

// ResumeSignal resolves a signal-wait node using the configured mode.
// It checks for a persisted outcome first (reattach safety).
func (r *resumer) ResumeSignal(ctx context.Context, runID, nodeName, signalName string, validOutcomes []string) (map[string]string, error) {
	// Check for a persisted outcome from a previous attempt.
	if payload, ok := r.loadPersistedSignal(runID, nodeName); ok {
		r.opts.Log.Info("local-approval: using persisted signal outcome", "node", nodeName, "outcome", payload["outcome"])
		return payload, nil
	}

	var payload map[string]string
	var err error

	switch r.mode {
	case ModeStdin:
		payload, err = r.resolveSignalStdin(ctx, nodeName, signalName)
	case ModeFile:
		payload, err = r.resolveSignalFile(ctx, runID, nodeName, signalName)
	case ModeEnv:
		payload, err = r.resolveSignalEnv(nodeName)
	case ModeAutoApprove:
		payload = r.resolveSignalAutoApprove(nodeName, signalName)
	default:
		return nil, fmt.Errorf("unknown local approval mode %q", r.mode)
	}
	if err != nil {
		return nil, err
	}

	// Validate the resolved outcome against the wait node's declared outcomes.
	// This prevents arbitrary non-empty outcomes from triggering the engine's
	// first-outcome fallback behaviour.
	if len(validOutcomes) > 0 {
		if err := validateOutcome(nodeName, payload["outcome"], validOutcomes); err != nil {
			return nil, err
		}
	}

	if persistErr := r.persistDecision(runID, nodeName, &persistedDecision{
		Outcome:   payload["outcome"],
		DecidedAt: time.Now().UTC().Format(time.RFC3339),
	}); persistErr != nil {
		r.opts.Log.Warn("local-approval: failed to persist signal outcome; reattach safety degraded",
			"node", nodeName, "error", persistErr)
	}
	return payload, nil
}

// validateOutcome returns an error if outcome is not among validOutcomes.
func validateOutcome(nodeName, outcome string, validOutcomes []string) error {
	for _, v := range validOutcomes {
		if v == outcome {
			return nil
		}
	}
	return fmt.Errorf("signal wait %q: outcome %q is not declared (declared: %s)",
		nodeName, outcome, strings.Join(validOutcomes, ", "))
}

// --- stdin mode ---

func (r *resumer) resolveApprovalStdin(ctx context.Context, name string, approvers []string, reason string) (map[string]string, error) {
	fmt.Fprintf(r.opts.Stderr, "\n[criteria] Approval required for node %q\n", name)
	if len(approvers) > 0 {
		fmt.Fprintf(r.opts.Stderr, "  Approvers: %s\n", strings.Join(approvers, ", "))
	}
	if reason != "" {
		fmt.Fprintf(r.opts.Stderr, "  Reason: %s\n", reason)
	}
	fmt.Fprintf(r.opts.Stderr, "Approve? (y/n) ")

	decision, err := readLineWithContext(ctx, r.opts.Stdin)
	if err != nil {
		// Context cancellation/deadline: propagate as an error so the run
		// aborts cleanly and no rejection is persisted.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		// EOF or other read errors mean non-interactive input → rejected, not an error.
		return map[string]string{"decision": "rejected", "reason": "non-interactive input"}, nil
	}
	return parseApprovalInput(decision), nil
}

func (r *resumer) resolveSignalStdin(ctx context.Context, nodeName, signalName string) (map[string]string, error) {
	fmt.Fprintf(r.opts.Stderr, "\n[criteria] Signal wait for node %q (signal=%q)\n", nodeName, signalName)
	fmt.Fprintf(r.opts.Stderr, "Enter JSON payload (e.g. {\"outcome\":\"received\"}): ")

	line, err := readLineWithContext(ctx, r.opts.Stdin)
	if err != nil {
		return nil, fmt.Errorf("signal %q: %w", signalName, err)
	}
	return parseSignalInput(line)
}

// readLineWithContext reads one line from r, returning an error on EOF or context cancellation.
func readLineWithContext(ctx context.Context, r io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		if scanner.Scan() {
			ch <- result{line: scanner.Text()}
		} else {
			ch <- result{err: io.EOF}
		}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-ch:
		return res.line, res.err
	}
}

func parseApprovalInput(input string) map[string]string {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "y", "yes":
		return map[string]string{"decision": "approved"}
	case "n", "no":
		return map[string]string{"decision": "rejected"}
	default:
		return map[string]string{"decision": "rejected", "reason": "non-interactive input"}
	}
}

func parseSignalInput(input string) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &m); err != nil {
		return nil, fmt.Errorf("parse signal input JSON: %w (input was %q)", err, input)
	}
	if strings.TrimSpace(m["outcome"]) == "" {
		return nil, fmt.Errorf("signal input must include a non-empty %q key, got %v", "outcome", m)
	}
	return m, nil
}

// --- file mode ---

// approvalRequestFileName returns the filename under runs/<runID>/ where the
// operator writes their response: approval-<node>.json.
func approvalRequestFileName(nodeName string) string {
	return "approval-" + nodeName + ".json"
}

func (r *resumer) resolveApprovalFile(ctx context.Context, runID, name string) (map[string]string, error) {
	reqPath, err := r.requestPath(runID, name)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(r.opts.Stderr, "[criteria] Waiting for approval decision file:\n  %s\n", reqPath)
	fmt.Fprintf(r.opts.Stderr, `  Write {"decision":"approved"} or {"decision":"rejected","reason":"..."} to that path.`+"\n")

	payload, err := r.pollForFile(ctx, reqPath)
	if err != nil {
		return nil, fmt.Errorf("approval %q file mode: %w", name, err)
	}
	if err := os.Remove(reqPath); err != nil && !os.IsNotExist(err) {
		r.opts.Log.Warn("local-approval: failed to remove approval request file", "path", reqPath, "error", err)
	}
	decision, ok := payload["decision"]
	if !ok || (decision != "approved" && decision != "rejected") {
		return nil, fmt.Errorf("approval %q: decision file must contain \"decision\":\"approved\" or \"decision\":\"rejected\", got %v", name, payload)
	}
	return payload, nil
}

func (r *resumer) resolveSignalFile(ctx context.Context, runID, nodeName, signalName string) (map[string]string, error) {
	reqPath, err := r.requestPath(runID, nodeName)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(r.opts.Stderr, "[criteria] Waiting for signal file for node %q (signal=%q):\n  %s\n", nodeName, signalName, reqPath)
	fmt.Fprintf(r.opts.Stderr, `  Write {"outcome":"<outcome_name>"} to that path.`+"\n")

	payload, err := r.pollForFile(ctx, reqPath)
	if err != nil {
		return nil, fmt.Errorf("signal wait %q file mode: %w", nodeName, err)
	}
	if err := os.Remove(reqPath); err != nil && !os.IsNotExist(err) {
		r.opts.Log.Warn("local-approval: failed to remove signal request file", "path", reqPath, "error", err)
	}
	if _, ok := payload["outcome"]; !ok {
		return nil, fmt.Errorf("signal wait %q: response file must contain \"outcome\" key, got %v", nodeName, payload)
	}
	return payload, nil
}

// pollForFile polls reqPath until it appears or the timeout/context expires.
// Returns the decoded JSON map on success.
func (r *resumer) pollForFile(ctx context.Context, reqPath string) (map[string]string, error) {
	// Ensure the parent directory exists so the operator has somewhere to write.
	if err := os.MkdirAll(filepath.Dir(reqPath), 0o700); err != nil {
		return nil, fmt.Errorf("create request dir: %w", err)
	}

	deadline := time.Now().Add(r.opts.FileTimeout)
	ticker := time.NewTicker(r.opts.FilePollingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case t := <-ticker.C:
			if t.After(deadline) {
				return nil, fmt.Errorf("timed out waiting for decision file %q (timeout=%v)", reqPath, r.opts.FileTimeout)
			}
			data, err := os.ReadFile(reqPath)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read decision file: %w", err)
			}
			var m map[string]string
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("decode decision file: %w", err)
			}
			return m, nil
		}
	}
}

// requestPath returns the path for the operator-written decision request file:
// <stateDir>/runs/<runID>/approval-<node>.json.
// Uses RequestPathFn if injected (production), otherwise falls back to StateDir derivation (tests).
func (r *resumer) requestPath(runID, nodeName string) (string, error) {
	if r.opts.RequestPathFn != nil {
		return r.opts.RequestPathFn(runID, nodeName)
	}
	sd, err := r.stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(sd, "runs", runID, approvalRequestFileName(nodeName)), nil
}

// --- env mode ---

func (r *resumer) resolveApprovalEnv(name string) (map[string]string, error) {
	envKey := "CRITERIA_APPROVAL_" + nodeNameToEnvSuffix(name)
	val := os.Getenv(envKey)
	switch val {
	case "approved", "rejected":
		return map[string]string{"decision": val}, nil
	case "":
		return nil, fmt.Errorf("approval %q: env mode requires %s=approved|rejected (variable is unset)", name, envKey)
	default:
		return nil, fmt.Errorf("approval %q: %s=%q is invalid (want approved|rejected)", name, envKey, val)
	}
}

func (r *resumer) resolveSignalEnv(nodeName string) (map[string]string, error) {
	envKey := "CRITERIA_SIGNAL_" + nodeNameToEnvSuffix(nodeName)
	val := os.Getenv(envKey)
	if val == "" {
		return nil, fmt.Errorf("signal wait %q: env mode requires %s=<outcome> (variable is unset)", nodeName, envKey)
	}
	return map[string]string{"outcome": val}, nil
}

// nodeNameToEnvSuffix uppercases the node name and replaces dots and hyphens
// with underscores, producing a valid env var suffix.
func nodeNameToEnvSuffix(name string) string {
	return strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(name))
}

// --- auto-approve mode ---

func (r *resumer) resolveApprovalAutoApprove(name string) map[string]string {
	r.opts.Log.Warn("local-approval: auto-approving approval node — CRITERIA_LOCAL_APPROVAL=auto-approve is set; do not use in production",
		"node", name)
	return map[string]string{"decision": "approved"}
}

func (r *resumer) resolveSignalAutoApprove(nodeName, signalName string) map[string]string {
	r.opts.Log.Warn("local-approval: auto-approving signal wait node — CRITERIA_LOCAL_APPROVAL=auto-approve is set; do not use in production",
		"node", nodeName, "signal", signalName)
	return map[string]string{"outcome": "success"}
}

// --- decision persistence ---

func (r *resumer) persistDecision(runID, nodeName string, d *persistedDecision) error {
	path, err := r.decisionPath(runID, nodeName)
	if err != nil {
		return err
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return mkErr
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (r *resumer) loadPersistedApproval(runID, nodeName string) (map[string]string, bool) {
	path, err := r.decisionPath(runID, nodeName)
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var d persistedDecision
	if err := json.Unmarshal(data, &d); err != nil || d.Decision == "" {
		return nil, false
	}
	return map[string]string{"decision": d.Decision}, true
}

func (r *resumer) loadPersistedSignal(runID, nodeName string) (map[string]string, bool) {
	path, err := r.decisionPath(runID, nodeName)
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var d persistedDecision
	if err := json.Unmarshal(data, &d); err != nil || d.Outcome == "" {
		return nil, false
	}
	return map[string]string{"outcome": d.Outcome}, true
}

func (r *resumer) decisionPath(runID, nodeName string) (string, error) {
	if r.opts.DecisionPathFn != nil {
		return r.opts.DecisionPathFn(runID, nodeName)
	}
	sd, err := r.stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(sd, "runs", runID, "approvals", nodeName+".json"), nil
}

func (r *resumer) stateDir() (string, error) {
	if r.opts.StateDir != "" {
		return r.opts.StateDir, nil
	}
	if d := os.Getenv("CRITERIA_STATE_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".criteria"), nil
}
