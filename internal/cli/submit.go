// Package cli holds the cobra subcommands for the criteria binary.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
	"github.com/brokenbots/criteria/workflow"
)

// Stable non-zero exit codes returned by `criteria submit` for distinguishable
// failure modes. These are surfaced through OSErrorCode so cmd/criteria/main.go
// can exit with the right code instead of the generic 1.
const (
	exitInvalidWorkflow     = 2
	exitServerUnreachable   = 3
	exitDuplicateSubmission = 4
	exitAuthFailure         = 5
)

// submitError is a tagged error carrying a stable OS exit code.
type submitError struct {
	msg  string
	code int
}

func (e *submitError) Error() string { return e.msg }
func (e *submitError) ExitCode() int { return e.code }

// submitOptions holds the flags for the criteria submit command.
type submitOptions struct {
	workflowPath   string
	labels         map[string]string
	idempotencyKey string
	watch          bool
	output         string
	serverClientFlags
}

// NewSubmitCmd returns the criteria submit command.
func NewSubmitCmd() *cobra.Command {
	var opts submitOptions

	cmd := &cobra.Command{
		Use:   "submit <workflow.chcl|workflow.hcl|dir>",
		Short: "Submit a workflow to a Criteria server for execution",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			opts.workflowPath = args[0]
			return runSubmit(cmd.Context(), &opts, cmd.OutOrStdout())
		},
	}

	opts.serverClientFlags.bind(cmd)
	cmd.Flags().StringToStringVar(&opts.labels, "label", nil, "Assignment label passed to the server (repeatable: key=value)")
	cmd.Flags().StringVar(&opts.idempotencyKey, "idempotency-key", "", "Opaque caller-provided idempotency key")
	cmd.Flags().BoolVar(&opts.watch, "watch", false, "Watch the submitted run until it reaches a terminal state")
	cmd.Flags().StringVar(&opts.output, "output", envOrDefault("CRITERIA_OUTPUT", "auto"), "Output format for --watch: auto|concise|json")
	return cmd
}

// runSubmit validates the workflow, sends it to the server, prints the run id,
// and optionally tails the run until it reaches a terminal state.
func runSubmit(ctx context.Context, opts *submitOptions, out io.Writer) error {
	workflowPath := strings.TrimSpace(opts.workflowPath)
	if workflowPath == "" {
		return errors.New("workflow path is required")
	}

	spec, err := loadWorkflowForSubmit(ctx, workflowPath)
	if err != nil {
		return err
	}

	client, err := opts.serverClientFlags.client()
	if err != nil {
		return err
	}

	runID, err := submitWorkflow(ctx, client, spec, opts)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, runID)

	if !opts.watch {
		return nil
	}
	return watchSubmittedRun(ctx, client, runID, opts.output, out)
}

// submitWorkflow sends the parsed workflow to the server and returns the run id.
func submitWorkflow(ctx context.Context, client criteriav1connect.ServerServiceClient, spec *workflow.Spec, opts *submitOptions) (string, error) {
	lockfileSource, err := readLockfileSource(workflowDirFromPath(opts.workflowPath))
	if err != nil {
		return "", fmt.Errorf("read lockfile: %w", err)
	}

	resp, err := client.SubmitWorkflowAssignment(ctx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   spec.Header.Name,
		WorkflowSource: string(spec.SourceBytes),
		LockfileSource: lockfileSource,
		Labels:         opts.labels,
		IdempotencyKey: opts.idempotencyKey,
	}))
	if err != nil {
		return "", classifySubmitError(err)
	}

	state := resp.Msg.GetState()
	if state == pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED {
		return "", &submitError{
			msg:  fmt.Sprintf("submission rejected: %s", resp.Msg.GetRejectionReason()),
			code: exitDuplicateSubmission,
		}
	}
	runID := resp.Msg.GetRunId()
	if runID == "" {
		return "", &submitError{
			msg:  "server returned an empty run id",
			code: exitInvalidWorkflow,
		}
	}
	return runID, nil
}

// watchSubmittedRun tails a submitted run until it reaches a terminal state.
func watchSubmittedRun(ctx context.Context, client criteriav1connect.ServerServiceClient, runID, output string, out io.Writer) error {
	mode, err := resolveOutputMode(output, os.Stdout)
	if err != nil {
		return err
	}
	if err := runWatch(ctx, newRunEventClient(client), runID, mode, out); err != nil {
		return classifySubmitError(err)
	}
	return nil
}

// loadWorkflowForSubmit parses and lightly compiles the workflow to reject
// invalid operator input before sending it to the server. Adapter schemas and
// local pin coverage are intentionally not required: the server-side agent
// performs full validation with its own adapter binaries.
func loadWorkflowForSubmit(ctx context.Context, workflowPath string) (*workflow.Spec, error) {
	spec, diags := workflow.ParseFileOrDir(workflowPath)
	if diags.HasErrors() {
		return nil, &submitError{
			msg:  fmt.Sprintf("invalid workflow: %v", newDiagsError(diags)),
			code: exitInvalidWorkflow,
		}
	}
	if spec.Header == nil || strings.TrimSpace(spec.Header.Name) == "" {
		return nil, &submitError{
			msg:  "invalid workflow: missing workflow name",
			code: exitInvalidWorkflow,
		}
	}

	workflowDir := workflowDirFromPath(workflowPath)
	_, diags = workflow.CompileWithContext(ctx, spec, nil, workflow.CompileOpts{
		WorkflowDir:         workflowDir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		return nil, &submitError{
			msg:  fmt.Sprintf("invalid workflow: %v", newDiagsError(diags)),
			code: exitInvalidWorkflow,
		}
	}

	return spec, nil
}

// readLockfileSource returns the raw content of the workflow tree's
// .criteria.lock.hcl, or an empty string when the file is absent.
func readLockfileSource(workflowDir string) (string, error) {
	p := filepath.Join(workflowDir, ".criteria.lock.hcl")
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

// classifySubmitError maps server-side Connect errors to stable submit exit
// codes. Errors that already carry a submit exit code are returned unchanged.
func classifySubmitError(err error) error {
	var se *submitError
	if errors.As(err, &se) {
		return err
	}

	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return err
	}

	switch connectErr.Code() {
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeUnimplemented:
		return &submitError{msg: err.Error(), code: exitInvalidWorkflow}
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return &submitError{msg: err.Error(), code: exitServerUnreachable}
	case connect.CodeAlreadyExists:
		return &submitError{msg: err.Error(), code: exitDuplicateSubmission}
	case connect.CodePermissionDenied, connect.CodeUnauthenticated:
		return &submitError{msg: err.Error(), code: exitAuthFailure}
	default:
		return err
	}
}

// OSErrorCode returns a non-zero OS exit code for errors that carry one, or -1
// when the generic exit code 1 should be used.
func OSErrorCode(err error) int {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}
