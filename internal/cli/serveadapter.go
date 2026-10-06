package cli

// ADR-0008 child role: `criteria serve-adapter --workflow <dir>` serves a
// workflow as an adapter v2 AdapterService over the peer phone-home substrate
// (internal/peer). The process loads and compiles the workflow once (this
// file), then fronts the adapter contract implemented in
// serveadapter_adapter.go with runs driven by the real engine machinery in
// serveadapter_run.go. There is no child adapter binary and no shim involved:
// this process itself is the adapter (host-of-record doctrine).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/peer"
	"github.com/brokenbots/criteria/internal/tunables"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/version"
)

const serveAdapterEnvDocs = `Serve a workflow as an adapter v2 AdapterService over the peer phone-home
(ADR-0008 child role): the process compiles the workflow once at serve time,
fronts the AdapterService contract, and executes one child run per Execute
call using the engine's real pause/resume machinery.

Peer-side configuration is environment-first and mirrors ` + "`criteria peer`" + `:
CRITERIA_REMOTE_HOST (required), CRITERIA_REMOTE_TOKEN, CRITERIA_REMOTE_SCOPE,
CRITERIA_REMOTE_DIGEST, the CRITERIA_REMOTE_TLS_* trio, and the strict peer
tunables CRITERIA_PEER_BACKOFF_MIN/_MAX and CRITERIA_PEER_JOURNAL_LIMIT —
see "criteria peer --help". Unlike "criteria peer" there is no adapter binary
to resolve: this process fronts the compiled workflow.

The workflow's own step adapters are launched in this child process per its
lockfile and environment policy (host-of-record doctrine); their shared
container budget is CRITERIA_SERVE_ADAPTER_CONCURRENCY (default 8).

Wait and approval nodes are rejected in this mode: they fail closed at serve
time with a clear error naming both node kinds, because parent-adjacent
control is an explicit ADR-0008 D6 non-goal.`

// NewServeAdapterCmd builds the `criteria serve-adapter` command.
func NewServeAdapterCmd() *cobra.Command {
	var (
		workflowPath  string
		varFiles      []string
		varOverrides  []string
		adapterName   string
		adapterVers   string
		allowUnsigned bool
	)
	cmd := &cobra.Command{
		Use:   "serve-adapter --workflow <dir>",
		Short: "Serve a workflow as an adapter v2 AdapterService over the peer phone-home (ADR-0008)",
		Long:  serveAdapterEnvDocs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeAdapter(cmd.Context(), serveAdapterOptions{
				workflowPath:  workflowPath,
				varFiles:      varFiles,
				varOverrides:  varOverrides,
				adapterName:   adapterName,
				adapterVers:   adapterVers,
				allowUnsigned: allowUnsigned,
			})
		},
	}
	cmd.Flags().StringVar(&workflowPath, "workflow", "", "path to the workflow directory (or entry file) to serve")
	_ = cmd.MarkFlagRequired("workflow")
	cmd.Flags().StringSliceVar(&varFiles, "var-file", nil, "HCL/JSON variable definitions file (repeatable)")
	cmd.Flags().StringSliceVar(&varOverrides, "var", nil, "bind a workflow variable as k=v (repeatable; engine-coerced like --var on apply)")
	cmd.Flags().StringVar(&adapterName, "adapter-name", "", "override the adapter identity reported to the peer host (default: CRITERIA_ADAPTER_NAME, then the workflow name)")
	cmd.Flags().StringVar(&adapterVers, "adapter-version", "", "override the adapter version reported to the peer host (default: CRITERIA_ADAPTER_VERSION, then the criteria build version)")
	cmd.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "allow compiling a workflow without a signature when signature verification is strict")
	return cmd
}

// serveAdapterOptions carries the resolved CLI flags for runServeAdapter.
type serveAdapterOptions struct {
	workflowPath  string
	varFiles      []string
	varOverrides  []string
	adapterName   string
	adapterVers   string
	allowUnsigned bool
}

func runServeAdapter(parent context.Context, opts serveAdapterOptions) error {
	probe := newJSONStderrLogger(slog.LevelInfo, "serve-adapter")
	log := newJSONStderrLogger(peerLogLevel(probe, os.Getenv(peer.EnvLogLevel)), "serve-adapter")
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	path := strings.TrimSpace(opts.workflowPath)
	if path == "" {
		return errors.New("--workflow is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve workflow path %q: %w", opts.workflowPath, err)
	}
	if info, statErr := os.Stat(path); statErr != nil {
		return fmt.Errorf("workflow path %q: %w", path, statErr)
	} else if !info.IsDir() && !isWorkflowSourceFile(path) {
		return fmt.Errorf("workflow path %q is neither a workflow directory nor a .hcl/.chcl source file", path)
	}

	// ADR-0008 D1: load + compile the workflow ONCE at serve time, before the
	// phone-home connection is opened. Subsequent Execute calls reuse this
	// compile.
	src, graph, loader, err := compileForExecution(ctx, path, log, false, opts.allowUnsigned)
	if err != nil {
		return err
	}
	digest := serveAdapterWorkflowDigest(src)

	if err := rejectWaitApprovalNodes(graph); err != nil {
		return err
	}

	vars, err := mergeVarSources(opts.varFiles, opts.varOverrides)
	if err != nil {
		return err
	}

	cfg, err := peer.LoadConfigFromEnv()
	if err != nil {
		return err
	}
	if err := resolvePeerIdentityForWorkflow(&cfg, graph, digest, opts.adapterName, opts.adapterVers, log); err != nil {
		return err
	}
	log.Info("serve-adapter config resolved",
		"adapter", cfg.AdapterName,
		"workflow", graph.Name,
		"digest", digest,
		"host", cfg.Host,
		"scope", cfg.Scope,
		"child_keepalive", cfg.ChildKeepAlive,
		"journal_limit", cfg.JournalLimit,
		"backoff_min", cfg.BackoffMin.String(),
		"backoff_max", cfg.BackoffMax.String(),
		"concurrency_budget", tunables.FromEnv().ServeAdapterConcurrency,
	)

	journal := peer.NewEventJournal(cfg.JournalLimit)
	impl := newServeAdapterClient(serveAdapterClientOptions{
		graph:        graph,
		loader:       loader,
		digest:       digest,
		sourceHash:   workflowSourceHash(src),
		workflowPath: path,
		vars:         vars,
		journal:      journal,
		log:          log,
		baseCtx:      ctx,
	})
	srv := peer.NewServeAdapterServer(&cfg, impl, journal, log)
	// CloseSession teardown ends the process once the host's teardown
	// round-trip is complete; RequestExit arms the peer loop's exit.
	impl.setExit(srv.RequestExit)

	// Serve runs the phone-home loop until ctx is done (SIGINT/SIGTERM), the
	// exit signal fires, or the substrate gives up. The serve-adapter flavor
	// never performs child binary shutdown: there is no child binary.
	return srv.Serve(ctx)
}

// resolvePeerIdentityForWorkflow validates the phone-home endpoint and fills
// the identity fields the peer server reports in its handshake. Unlike
// peer.Config.Resolve, there is no adapter binary to locate — the served
// adapter is this process fronting the compiled workflow.
func resolvePeerIdentityForWorkflow(cfg *peer.Config, graph *workflow.FSMGraph, digest, nameOverride, versionOverride string, log *slog.Logger) error {
	if cfg.Host == "" {
		return errors.New("CRITERIA_REMOTE_HOST is required (peer phone-home endpoint host:port or unix socket path)")
	}
	if cfg.Digest != "" && cfg.Digest != digest {
		log.Warn("workflow digest differs from the peer-declared digest",
			"declared", cfg.Digest, "workflow", digest)
	}
	if name := strings.TrimSpace(nameOverride); name != "" {
		cfg.AdapterName = name
	}
	if cfg.AdapterName == "" {
		cfg.AdapterName = strings.TrimSpace(graph.Name)
	}
	if cfg.AdapterName == "" {
		cfg.AdapterName = "workflow"
	}
	if ver := strings.TrimSpace(versionOverride); ver != "" {
		cfg.AdapterVersion = ver
	}
	if cfg.AdapterVersion == "" {
		cfg.AdapterVersion = serveAdapterVersionLabel()
	}
	tls, err := adapterhost.LoadClientTLS(cfg.TLSCertPath, cfg.TLSKeyPath, cfg.TLSCAPath)
	if err != nil {
		return err
	}
	cfg.TLS = tls
	return nil
}

// newJSONStderrLogger follows the repo's structured-logging convention: slog
// JSON on stderr, with a component tag.
func newJSONStderrLogger(level slog.Level, component string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("component", component)
}

// serveAdapterVersionLabel builds the identity version fallback: the criteria
// build version, or "dev" when the build carries no explicit version.
func serveAdapterVersionLabel() string {
	info := version.Current()
	if !info.Known {
		return "dev"
	}
	return info.Version.String()
}

// serveAdapterWorkflowDigest hashes the compiled workflow source so the peer
// host can pin the served workflow (CRITERIA_REMOTE_DIGEST stays a
// declared-identity cross-check, semantics unchanged).
func serveAdapterWorkflowDigest(src []byte) string {
	return "sha256:" + workflowSourceHash(src)
}

// rejectWaitApprovalNodes fails closed on wait/approval nodes anywhere in the
// served graph (ADR-0008 v0.6.0 non-goal): parent-adjacent control is not
// served by this mode, so the served workflow must not declare any.
// Subworkflow bodies are walked recursively — a callee cannot smuggle one in.
func rejectWaitApprovalNodes(graph *workflow.FSMGraph) error {
	var found []string
	var walk func(g *workflow.FSMGraph, root string)
	walk = func(g *workflow.FSMGraph, root string) {
		for _, w := range sortedStringKeys(g.Waits) {
			found = append(found, prefixFor(root)+"wait node \""+w+"\"")
		}
		for _, a := range sortedStringKeys(g.Approvals) {
			found = append(found, prefixFor(root)+"approval node \""+a+"\"")
		}
		for _, name := range sortedStringKeys(g.Subworkflows) {
			if body := g.Subworkflows[name].Body; body != nil {
				walk(body, name+".")
			}
		}
	}
	walk(graph, "")
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("serve-adapter requires a workflow with no wait or approval nodes (parent-adjacent control is an ADR-0008 D6 non-goal); found: %s", strings.Join(found, ", "))
}

// connectErrorStatus converts run-level failures into Connect error codes for
// the v2 wire. Unknown causes fall back to the internal code.
func connectErrorStatus(err error) error {
	var inFlight *ErrChildRunInFlight
	if errors.As(err, &inFlight) {
		return connect.NewError(connect.CodeFailedPrecondition, inFlight)
	}
	if errors.Is(err, errSessionUnknownConnect) {
		return connect.NewError(connect.CodeNotFound, errors.New(err.Error()))
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// prefixFor renders the subworkflow prefix for a rejection message; the root
// graph is unprefixed and callees are prefixed "callee.".
func prefixFor(root string) string { return root }

// sortedStringKeys returns map keys in sorted order for deterministic
// messaging and schema output.
func sortedStringKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isWorkflowSourceFile reports whether path looks like a compilable workflow
// source file (as opposed to a workflow directory).
func isWorkflowSourceFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".hcl" || ext == ".chcl"
}
