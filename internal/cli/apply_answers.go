package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/brokenbots/criteria/internal/cli/localresume"
	"github.com/brokenbots/criteria/internal/run"
	"github.com/brokenbots/criteria/workflow"
)

// localApprovalConfig names the inputs the approval/signal resolution paths
// (CRI-256) need: the --answers file, the prompt streams, and the TTY probe.
// The fresh path takes it from the apply flags; the reattach path inherits it
// from the re-invoking command.
type localApprovalConfig struct {
	answersPath string
	stdin       io.Reader
	stderr      io.Writer
	tty         func() bool
}

func localApprovalConfigFrom(opts *applyOptions) localApprovalConfig {
	return localApprovalConfig{
		answersPath: opts.answersPath,
		stdin:       opts.stdin,
		stderr:      opts.stderr,
		tty:         opts.tty,
	}
}

// interactive reports whether an operator can answer an interactive prompt:
// the explicit test override when set, else whether real stdin is attached to
// a terminal.
func (c localApprovalConfig) interactive() bool {
	if c.tty != nil {
		return c.tty()
	}
	return run.IsTerminal(os.Stdin)
}

func (c localApprovalConfig) promptStderr() io.Writer {
	if c.stderr != nil {
		return c.stderr
	}
	return os.Stderr
}

// approvalResolution is the selected resolution posture for a local run's
// approval and signal-wait pauses (CRI-256's two designed paths plus the
// scripted out-of-band modes):
//
//   - answers mode (--answers): decisions come from the pre-populated file;
//     non-interactive by definition.
//   - explicit env mode (CRITERIA_LOCAL_APPROVAL): stdin | file | env |
//     auto-approve (auto-approve stays strictly opt-in with its loud warning).
//   - context default: an interactive TTY gets the stdin prompt (path 1);
//     otherwise the run relies on the control listener (CRI-255 headless
//     flow) and warns loudly at the pause.
//
// resumer nil means control-RPC-only resolution (no pause-blocking resumer).
type approvalResolution struct {
	resumer       localresume.LocalResumer
	answersActive bool
	answersPath   string
	interactive   bool
	// ttyOK is the session's interactivity at selection time; answers-mode
	// pauses consult it when an entry is missing from the file (prompt
	// fallback vs loud failure).
	ttyOK bool
	cfg   localApprovalConfig
	opts  localresume.Options
}

// selectApprovalResolution resolves the posture from the invocation config and
// the compiled graph. The graph is required: answers entries are validated
// against the workflow's approval and signal-wait nodes before the run starts.
func selectApprovalResolution(log *slog.Logger, cfg localApprovalConfig, graph *workflow.FSMGraph) (*approvalResolution, error) {
	res := &approvalResolution{cfg: cfg, ttyOK: cfg.interactive()}
	if path := strings.TrimSpace(cfg.answersPath); path != "" {
		entries, err := parseAnswersFile(path, graph)
		if err != nil {
			return res, err
		}
		opts, err := localResumerOptions(log, cfg)
		if err != nil {
			return res, err
		}
		// The two designed paths are mutually exclusive per run: the answers
		// file wins and the run stays non-interactive even on a TTY.
		if raw := os.Getenv("CRITERIA_LOCAL_APPROVAL"); raw != "" {
			log.Warn("local-approval: --answers overrides CRITERIA_LOCAL_APPROVAL; the env mode is ignored for this run",
				"file", path, "env_mode", raw)
		} else {
			log.Info("local-approval: --answers resolves approval and signal pauses non-interactively",
				"file", path, "nodes", len(entries))
		}
		res.resumer = localresume.NewAnswers(entries, opts)
		res.answersActive = true
		res.answersPath = path
		res.opts = opts
		return res, nil
	}
	if raw := os.Getenv("CRITERIA_LOCAL_APPROVAL"); raw != "" {
		opts, err := localResumerOptions(log, cfg)
		if err != nil {
			return res, err
		}
		m, err := localresume.ParseMode(raw)
		if err != nil {
			return res, err
		}
		res.resumer = localresume.New(m, opts)
		res.opts = opts
		return res, nil
	}
	if !res.ttyOK {
		// CRI-256 default without a TTY: the run is resolved exclusively via
		// its control listener (CRI-255 headless flow). A pause that can never
		// resolve (listener down) fails loudly in the drain loop.
		return res, nil
	}
	opts, err := localResumerOptions(log, cfg)
	if err != nil {
		return res, err
	}
	log.Info("local-approval: no CRITERIA_LOCAL_APPROVAL set and stdin is a TTY; approval pauses prompt interactively",
		"hint", "pass --answers <file> for a non-interactive run")
	res.resumer = localresume.New(localresume.ModeStdin, opts)
	res.interactive = true
	res.opts = opts
	return res, nil
}

// localResumerOptions builds the shared resumer options (prompt streams,
// persistence paths, file-mode timeout). The file-timeout env value must fail
// loudly here: a mistyped duration would otherwise silently default to 1h.
func localResumerOptions(log *slog.Logger, cfg localApprovalConfig) (localresume.Options, error) {
	opts := localresume.Options{
		Log:            log,
		Stdin:          cfg.stdin,
		Stderr:         cfg.promptStderr(),
		DecisionPathFn: ApprovalDecisionPath,
		RequestPathFn:  ApprovalRequestPath,
	}
	if rawTimeout := os.Getenv("CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT"); rawTimeout != "" {
		d, err := time.ParseDuration(rawTimeout)
		if err != nil {
			return opts, fmt.Errorf("invalid CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT=%q: %w", rawTimeout, err)
		}
		opts.FileTimeout = d
	}
	return opts, nil
}

// promptFallbackResumer builds the interactive resumer for an answers-mode
// pause whose node is missing from the file, falling back to the prompt path
// when the session is interactive.
func (res *approvalResolution) promptFallbackResumer() localresume.LocalResumer {
	return localresume.New(localresume.ModeStdin, res.opts)
}

// parseAnswersFile reads --answers <file> and graph-validates every entry
// before the run starts: unknown node names, ambiguous decision/outcome
// pairs, and undeclared outcomes must fail loudly with all issues reported
// together so the operator gets a complete picture.
func parseAnswersFile(path string, graph *workflow.FSMGraph) (map[string]localresume.AnswerEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read answers file: %w", err)
	}
	entries, err := localresume.ParseAnswers(data)
	if err != nil {
		return nil, err
	}
	kind := map[string]string{}
	for name := range graph.Approvals {
		kind[name] = "approval"
	}
	for name, wait := range graph.Waits {
		if wait.Signal != "" {
			kind[name] = "signal wait"
		}
	}
	var unknown []string
	for name := range entries {
		if _, ok := kind[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		declared := make([]string, 0, len(kind))
		for name, k := range kind {
			declared = append(declared, name+" ("+k+")")
		}
		sort.Strings(declared)
		return nil, fmt.Errorf("answers file %s: unknown node(s) %s; workflow declares: %s",
			path, strings.Join(unknown, ", "), strings.Join(declared, ", "))
	}
	if err := validateAnswersEntries(path, graph, entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// validateAnswersEntries checks per-entry semantics against the compiled
// graph. Approval entries must carry a decision that is one of the node's
// declared decision names ("approved"/"rejected" by convention); signal-wait
// entries must carry a declared outcome.
func validateAnswersEntries(path string, graph *workflow.FSMGraph, entries map[string]localresume.AnswerEntry) error {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		entry := entries[name]
		if approval, ok := graph.Approvals[name]; ok {
			errs = append(errs, validateApprovalEntry(path, name, approval, entry)...)
			continue
		}
		if wait, ok := graph.Waits[name]; ok && wait.Signal != "" {
			errs = append(errs, validateWaitEntry(path, name, wait, entry)...)
		}
	}
	return errors.Join(errs...)
}

func validateApprovalEntry(path, name string, approval *workflow.ApprovalNode, entry localresume.AnswerEntry) []error {
	var errs []error
	if entry.Decision != "" && entry.Outcome != "" {
		return []error{fmt.Errorf("answers file %s: node %q is ambiguous: set decision or outcome, not both", path, name)}
	}
	if entry.Decision == "" && entry.Outcome != "" {
		return []error{fmt.Errorf("answers file %s: node %q is an approval node: set \"decision\", got \"outcome\" %q", path, name, entry.Outcome)}
	}
	if entry.Decision != "" && len(approval.Outcomes) > 0 {
		if _, ok := approval.Outcomes[entry.Decision]; !ok {
			declared := make([]string, 0, len(approval.Outcomes))
			for d := range approval.Outcomes {
				declared = append(declared, d)
			}
			sort.Strings(declared)
			errs = append(errs, fmt.Errorf("answers file %s: node %q: decision %q is not declared on the approval node (declared: %s)",
				path, name, entry.Decision, strings.Join(declared, ", ")))
		}
	}
	return errs
}

func validateWaitEntry(path, name string, wait *workflow.WaitNode, entry localresume.AnswerEntry) []error {
	var errs []error
	if entry.Decision != "" && entry.Outcome != "" {
		return []error{fmt.Errorf("answers file %s: node %q is ambiguous: set decision or outcome, not both", path, name)}
	}
	if entry.Decision != "" {
		return []error{fmt.Errorf("answers file %s: node %q is a signal wait: set \"outcome\", got \"decision\" %q", path, name, entry.Decision)}
	}
	declared := make([]string, 0, len(wait.Outcomes))
	for o := range wait.Outcomes {
		declared = append(declared, o)
	}
	if len(declared) == 0 {
		if entry.Outcome != "" {
			errs = append(errs, fmt.Errorf("answers file %s: node %q: outcome %q is not declared (the wait declares no outcomes)", path, name, entry.Outcome))
		}
		return errs
	}
	sort.Strings(declared)
	if _, ok := wait.Outcomes[entry.Outcome]; !ok {
		errs = append(errs, fmt.Errorf("answers file %s: node %q: outcome %q is not declared (declared: %s)",
			path, name, entry.Outcome, strings.Join(declared, ", ")))
	}
	return errs
}
