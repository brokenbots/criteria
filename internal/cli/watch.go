package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

const watchListPageSize = 1000

// watchOptions holds the flags for the criteria watch command.
type watchOptions struct {
	runID  string
	output string
	serverClientFlags
}

// NewWatchCmd returns the criteria watch command.
func NewWatchCmd() *cobra.Command {
	var opts watchOptions

	cmd := &cobra.Command{
		Use:   "watch [--run-id <id>] [run-id]",
		Short: "Watch a server run: replay historical events and tail live ones",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if len(args) > 1 {
				return errors.New("accepts at most one run-id argument")
			}
			if opts.runID == "" && len(args) == 1 {
				opts.runID = args[0]
			}
			if opts.runID == "" {
				return errors.New("--run-id is required")
			}
			if len(args) == 1 && args[0] != opts.runID {
				return errors.New("run-id argument and --run-id must match")
			}

			mode, err := resolveOutputMode(opts.output, os.Stdout)
			if err != nil {
				return err
			}

			client, err := opts.serverClientFlags.client()
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return runWatch(ctx, newRunEventClient(client), opts.runID, mode, cmd.OutOrStdout())
		},
	}

	opts.serverClientFlags.bind(cmd)
	cmd.Flags().StringVar(&opts.runID, "run-id", "", "Run ID to watch")
	cmd.Flags().StringVar(&opts.output, "output", envOrDefault("CRITERIA_OUTPUT", "auto"), "Output format: auto|concise|json")
	return cmd
}

// eventStream is the subset of connect.ServerStreamForClient used by watch.
type eventStream interface {
	Receive() bool
	Msg() *pb.Envelope
	Err() error
	Close() error
}

// runEventClient is the subset of ServerServiceClient used by watch.
type runEventClient interface {
	ListRunEvents(context.Context, *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error)
	WatchRun(context.Context, *connect.Request[pb.WatchRunRequest]) (eventStream, error)
}

// runEventClientAdapter adapts the generated ServerServiceClient to the
// narrower runEventClient interface so tests can supply lightweight fakes.
type runEventClientAdapter struct {
	c criteriav1connect.ServerServiceClient
}

func newRunEventClient(c criteriav1connect.ServerServiceClient) runEventClient {
	return &runEventClientAdapter{c: c}
}

func (a *runEventClientAdapter) ListRunEvents(ctx context.Context, req *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error) {
	return a.c.ListRunEvents(ctx, req)
}

func (a *runEventClientAdapter) WatchRun(ctx context.Context, req *connect.Request[pb.WatchRunRequest]) (eventStream, error) {
	stream, err := a.c.WatchRun(ctx, req)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// runWatch replays persisted events for runID, then tails live events until a
// terminal event is observed or the stream ends.
func runWatch(ctx context.Context, client runEventClient, runID string, mode outputMode, out io.Writer) error {
	lastSeq, done, err := replayHistoricalEvents(ctx, client, runID, mode, out)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	return tailLiveEvents(ctx, client, runID, lastSeq, mode, out)
}

func replayHistoricalEvents(ctx context.Context, client runEventClient, runID string, mode outputMode, out io.Writer) (lastSeq uint64, done bool, err error) {
	var sinceSeq uint64
	for {
		resp, err := client.ListRunEvents(ctx, connect.NewRequest(&pb.ListRunEventsRequest{
			RunId:    runID,
			SinceSeq: sinceSeq,
			Limit:    watchListPageSize,
		}))
		if err != nil {
			return 0, false, fmt.Errorf("list events: %w", err)
		}
		if resp == nil || resp.Msg == nil {
			return 0, false, errors.New("list events: nil response from server")
		}

		for _, env := range resp.Msg.Events {
			if env.Seq > lastSeq {
				lastSeq = env.Seq
			}
			isTerminal, err := printEvent(env, mode, out)
			if err != nil {
				return 0, false, err
			}
			if isTerminal {
				return 0, true, terminalOutcome(env, mode, out)
			}
		}

		if resp.Msg.NextSinceSeq == 0 || resp.Msg.NextSinceSeq == sinceSeq {
			break
		}
		sinceSeq = resp.Msg.NextSinceSeq
	}
	return lastSeq, false, nil
}

func tailLiveEvents(ctx context.Context, client runEventClient, runID string, sinceSeq uint64, mode outputMode, out io.Writer) error {
	stream, err := client.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{
		RunId:    runID,
		SinceSeq: sinceSeq,
	}))
	if err != nil {
		return fmt.Errorf("watch run: %w", err)
	}
	defer stream.Close()

	for stream.Receive() {
		env := stream.Msg()
		if env.GetWatchReady() != nil {
			continue
		}
		isTerminal, err := printEvent(env, mode, out)
		if err != nil {
			return err
		}
		if isTerminal {
			return terminalOutcome(env, mode, out)
		}
	}

	if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("watch stream: %w", err)
	}
	return errors.New("watch stream ended without terminal event")
}

// printEvent writes env to out in the requested mode and reports whether the
// event is terminal. Terminal events are only summarised by terminalOutcome in
// concise mode so that the final status is emitted exactly once; in JSON mode
// the full envelope is always written.
func printEvent(env *pb.Envelope, mode outputMode, out io.Writer) (bool, error) {
	if env.GetWatchReady() != nil {
		return false, nil
	}

	isTerminal := isTerminalEvent(env)

	switch mode {
	case outputModeJSON:
		b, err := protojson.Marshal(env)
		if err != nil {
			return false, fmt.Errorf("marshal event: %w", err)
		}
		if _, err := fmt.Fprintln(out, string(b)); err != nil {
			return false, err
		}
	case outputModeConcise:
		if isTerminal {
			return true, nil
		}
		line := formatEventConcise(env)
		if line != "" {
			if _, err := fmt.Fprintln(out, line); err != nil {
				return false, err
			}
		}
	}
	return isTerminal, nil
}

func isTerminalEvent(env *pb.Envelope) bool {
	return env.GetRunCompleted() != nil || env.GetRunFailed() != nil
}

func terminalOutcome(env *pb.Envelope, mode outputMode, out io.Writer) error {
	if rc := env.GetRunCompleted(); rc != nil {
		if mode == outputModeConcise {
			if rc.Success {
				fmt.Fprintf(out, "✔ run completed: %s\n", rc.FinalState)
			} else {
				fmt.Fprintf(out, "✗ run completed: %s (success=false)\n", rc.FinalState)
			}
		}
		if rc.Success {
			return nil
		}
		return fmt.Errorf("run completed with success=false: %s", rc.FinalState)
	}

	if rf := env.GetRunFailed(); rf != nil {
		if mode == outputModeConcise {
			if rf.Step != "" {
				fmt.Fprintf(out, "✗ run failed at %s: %s\n", rf.Step, rf.Reason)
			} else {
				fmt.Fprintf(out, "✗ run failed: %s\n", rf.Reason)
			}
		}
		if rf.Step != "" {
			return fmt.Errorf("run failed at %s: %s", rf.Step, rf.Reason)
		}
		return fmt.Errorf("run failed: %s", rf.Reason)
	}

	return errors.New("unknown terminal event")
}

func formatStepEntered(p *pb.Envelope_StepEntered) string {
	line := fmt.Sprintf("▶ step %s (adapter=%s", p.StepEntered.GetStep(), p.StepEntered.GetAdapter())
	if p.StepEntered.GetAttempt() > 1 {
		line += fmt.Sprintf(", attempt=%d", p.StepEntered.GetAttempt())
	}
	return line + ")"
}

func formatStepOutcome(p *pb.Envelope_StepOutcome) string {
	outcome := p.StepOutcome.GetOutcome()
	duration := time.Duration(p.StepOutcome.GetDurationMs()) * time.Millisecond
	marker := "✗"
	if outcome == "success" || outcome == "ok" {
		marker = "✓"
	}
	body := fmt.Sprintf("%s step %s: %s (%s)", marker, p.StepOutcome.GetStep(), outcome, duration)
	if p.StepOutcome.GetError() != "" {
		body += fmt.Sprintf(" [%s]", p.StepOutcome.GetError())
	}
	return body
}

func formatWaitEntered(p *pb.Envelope_WaitEntered) string {
	detail := p.WaitEntered.GetMode()
	if p.WaitEntered.GetMode() == "duration" && p.WaitEntered.GetDuration() != "" {
		detail = "duration=" + p.WaitEntered.GetDuration()
	} else if p.WaitEntered.GetSignal() != "" {
		detail = "signal=" + p.WaitEntered.GetSignal()
	}
	return fmt.Sprintf("⏸ wait %s (%s)", p.WaitEntered.GetNode(), detail)
}

func formatRunOutputs(p *pb.Envelope_RunOutputs) string {
	if len(p.RunOutputs.GetOutputs()) == 0 {
		return ""
	}
	lines := make([]string, 0, len(p.RunOutputs.GetOutputs()))
	for _, out := range p.RunOutputs.GetOutputs() {
		if out.GetDeclaredType() != "" {
			lines = append(lines, fmt.Sprintf("  output %s (%s) = %s", out.GetName(), out.GetDeclaredType(), out.GetValue()))
		} else {
			lines = append(lines, fmt.Sprintf("  output %s = %s", out.GetName(), out.GetValue()))
		}
	}
	return strings.Join(lines, "\n")
}

func formatRunConcise(env *pb.Envelope) (string, bool) {
	switch p := env.Payload.(type) {
	case *pb.Envelope_RunStarted:
		return fmt.Sprintf("▶ run started: %s (initial step: %s)", p.RunStarted.GetWorkflowName(), p.RunStarted.GetInitialStep()), true
	case *pb.Envelope_StepTransition:
		return fmt.Sprintf("  → %s", p.StepTransition.GetTo()), true
	case *pb.Envelope_BranchEvaluated:
		return fmt.Sprintf("  ↳ branch %s → %s", p.BranchEvaluated.GetNode(), p.BranchEvaluated.GetTarget()), true
	case *pb.Envelope_ForEachEntered:
		return fmt.Sprintf("↻ %s iterating (%d items)", p.ForEachEntered.GetNode(), p.ForEachEntered.GetCount()), true
	case *pb.Envelope_StepIterationStarted:
		return fmt.Sprintf("↻ %s[%d] = %s", p.StepIterationStarted.GetNode(), p.StepIterationStarted.GetIndex(), p.StepIterationStarted.GetValue()), true
	case *pb.Envelope_StepIterationItem:
		return fmt.Sprintf("↻ %s[%d] → %s", p.StepIterationItem.GetNode(), p.StepIterationItem.GetIndex(), p.StepIterationItem.GetStep()), true
	case *pb.Envelope_StepIterationCompleted:
		return fmt.Sprintf("↻ %s → %s (%s)", p.StepIterationCompleted.GetNode(), p.StepIterationCompleted.GetTarget(), p.StepIterationCompleted.GetOutcome()), true
	case *pb.Envelope_RunOutputs:
		return formatRunOutputs(p), true
	}
	return "", false
}

func formatStepConcise(env *pb.Envelope) (string, bool) {
	switch p := env.Payload.(type) {
	case *pb.Envelope_StepEntered:
		return formatStepEntered(p), true
	case *pb.Envelope_StepOutcome:
		return formatStepOutcome(p), true
	case *pb.Envelope_StepLog:
		return fmt.Sprintf("[%s %s] %s", p.StepLog.GetStep(), logStreamName(p.StepLog.GetStream()), strings.TrimSuffix(p.StepLog.GetChunk(), "\n")), true
	case *pb.Envelope_VariableSet:
		return fmt.Sprintf("  · var %s=%s (%s)", p.VariableSet.GetName(), p.VariableSet.GetValue(), p.VariableSet.GetSource()), true
	case *pb.Envelope_StepOutputCaptured:
		keys := make([]string, 0, len(p.StepOutputCaptured.GetOutputs()))
		for k := range p.StepOutputCaptured.GetOutputs() {
			keys = append(keys, k)
		}
		return fmt.Sprintf("  · step %s outputs: %s", p.StepOutputCaptured.GetStep(), strings.Join(keys, ", ")), true
	case *pb.Envelope_StepResumed:
		return fmt.Sprintf("↻ step %s resumed (attempt %d, %s)", p.StepResumed.GetStep(), p.StepResumed.GetAttempt(), p.StepResumed.GetReason()), true
	case *pb.Envelope_WaitEntered:
		return formatWaitEntered(p), true
	case *pb.Envelope_WaitResumed:
		return fmt.Sprintf("▶ wait %s resumed", p.WaitResumed.GetNode()), true
	case *pb.Envelope_ApprovalRequested:
		return fmt.Sprintf("⏸ approval requested at %s", p.ApprovalRequested.GetNode()), true
	case *pb.Envelope_ApprovalDecision:
		return fmt.Sprintf("  · approval %s at %s by %s", p.ApprovalDecision.GetDecision(), p.ApprovalDecision.GetNode(), p.ApprovalDecision.GetActor()), true
	}
	return "", false
}

func formatEventConcise(env *pb.Envelope) string {
	for _, fn := range []func(*pb.Envelope) (string, bool){formatRunConcise, formatStepConcise} {
		if line, ok := fn(env); ok {
			return line
		}
	}

	// AdapterEvent, CriteriaHeartbeat, CriteriaDisconnected, ScopeIterCursorSet, etc. are
	// intentionally omitted from concise output to keep the terminal view readable.
	return ""
}

func logStreamName(s pb.LogStream) string {
	switch s {
	case pb.LogStream_LOG_STREAM_STDOUT:
		return "stdout"
	case pb.LogStream_LOG_STREAM_STDERR:
		return "stderr"
	case pb.LogStream_LOG_STREAM_AGENT:
		return "agent"
	default:
		return "log"
	}
}
