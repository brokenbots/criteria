// Package shell implements the `shell` adapter. It runs an arbitrary command
// and maps the exit code to a step outcome (0 → "success", non-zero →
// "failure"). Stdout and stderr are streamed to the sink and captured into
// bounded buffers (default 4 MiB each). A hard timeout (default 5 minutes)
// prevents runaway steps. See docs/security/shell-adapter-threat-model.md for
// the full security design.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"sync"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// Name is the canonical adapter identifier.
const Name = "shell"

// Adapter is the built-in shell adapter.
type Adapter struct{}

// New returns a new shell Adapter.
func New() *Adapter { return &Adapter{} }

// Name returns the adapter's registered name.
func (a *Adapter) Name() string { return Name }

// Info returns the adapter's declared schema. Fields `env`, `command_path`,
// `timeout`, `output_limit_bytes`, and `working_directory` are declared as
// optional strings to enable schema-aware compile-time validation.
func (a *Adapter) Info() workflow.AdapterInfo {
	return workflow.AdapterInfo{
		InputSchema: map[string]workflow.ConfigField{
			"command": {
				Required: true,
				Type:     workflow.ConfigFieldString,
				Doc:      "Shell command string passed to `sh -c` (Unix) or `cmd /C` (Windows).",
			},
			"env": {
				Type: workflow.ConfigFieldString,
				Doc:  "JSON-encoded map[string]string of additional environment variables. Values starting with '$' inherit from the parent env (e.g. '$GOFLAGS'). Use jsonencode({KEY: \"$KEY\"}) in HCL.",
			},
			"command_path": {
				Type: workflow.ConfigFieldString,
				Doc:  "OS-path-separator-delimited list of directories that replace PATH for the child process.",
			},
			"timeout": {
				Type: workflow.ConfigFieldString,
				Doc:  "Hard timeout for the step (e.g. '10m'). Minimum 1s, maximum 1h. Default: 5m.",
			},
			"output_limit_bytes": {
				Type: workflow.ConfigFieldString,
				Doc:  "Per-stream stdout/stderr capture limit in bytes. Range: 1024–67108864. Default: 4194304 (4 MiB).",
			},
			"working_directory": {
				Type: workflow.ConfigFieldString,
				Doc:  "CWD for the spawned process. Must be under $HOME or CRITERIA_SHELL_ALLOWED_PATHS.",
			},
		},
		OutputSchema: map[string]workflow.ConfigField{
			"stdout":    {Type: workflow.ConfigFieldString, Doc: "Captured stdout (bounded; see output_limit_bytes)."},
			"stderr":    {Type: workflow.ConfigFieldString, Doc: "Captured stderr (bounded; see output_limit_bytes)."},
			"exit_code": {Type: workflow.ConfigFieldString, Doc: "Process exit code as a string."},
		},
	}
}

// Execute runs the shell command declared in step.Input["command"]. It applies
// sandbox defaults (env allowlist, PATH sanitization, hard timeout, bounded
// output capture, working-directory confinement) unless CRITERIA_SHELL_LEGACY=1
// is set.
func (a *Adapter) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	cmdStr, ok := step.Input["command"]
	if !ok || cmdStr == "" {
		return adapter.Result{Outcome: "failure"}, errors.New("shell adapter: input.command is required")
	}

	cfg, err := buildSandboxConfig(step.Input)
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}

	if cfg.workingDir != "" {
		if err := validateWorkingDirectory(cfg.workingDir); err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
	}

	// Create a step-level timeout context when a timeout is configured.
	// In legacy mode without an explicit timeout attribute, cfg.timeout == 0
	// and we use the caller's context directly (restoring pre-W05 behavior).
	timeoutCtx := ctx
	cancelTimeout := func() {}
	if cfg.timeout > 0 {
		timeoutCtx, cancelTimeout = context.WithTimeout(ctx, cfg.timeout)
	}
	defer cancelTimeout()

	cmd := buildCmd(timeoutCtx, cmdStr, cfg)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("start: %w", err)
	}

	stdoutCS := newCaptureState(cfg.outputLimitBytes)
	stderrCS := newCaptureState(cfg.outputLimitBytes)

	// pumpsDone is closed once both pump goroutines have exited. The pipe
	// closer goroutine below watches timeoutCtx.Done() (which fires on caller
	// cancellation or step timeout) and closes the pipe read-ends so the
	// pumps unblock from their blocking Read calls promptly — necessary
	// because grandchildren spawned by `sh -c` may hold the write-ends open
	// after the parent sh has been killed, leaving wg.Wait() hung otherwise.
	pumpsDone := make(chan struct{})
	go func() {
		select {
		case <-timeoutCtx.Done():
			stdoutPipe.Close() //nolint:errcheck
			stderrPipe.Close() //nolint:errcheck
		case <-pumpsDone:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go pumpStream(&wg, stdoutPipe, "stdout", sink, stdoutCS)
	go pumpStream(&wg, stderrPipe, "stderr", sink, stderrCS)
	wg.Wait()
	close(pumpsDone)

	return resolveWait(cmd.Wait(), ctx, timeoutCtx, cfg, stdoutCS, stderrCS, sink)
}

// buildCmd constructs and configures the exec.Cmd for the sandbox.
func buildCmd(timeoutCtx context.Context, cmdStr string, cfg sandboxConfig) *exec.Cmd {
	sh, flag := defaultShell()
	cmd := exec.CommandContext(timeoutCtx, sh, flag, cmdStr)
	setSIGTERMCancel(cmd)
	cmd.WaitDelay = killGrace
	if cfg.env != nil {
		cmd.Env = cfg.env
	}
	if cfg.workingDir != "" {
		cmd.Dir = cfg.workingDir
	}
	return cmd
}

// resolveWait interprets the error returned by cmd.Wait and builds the adapter
// Result. It distinguishes caller-context cancellation from step-level timeout
// from normal non-zero exit.
func resolveWait(
	waitErr error,
	callerCtx, timeoutCtx context.Context,
	cfg sandboxConfig,
	stdoutCS, stderrCS *captureState,
	sink adapter.EventSink,
) (adapter.Result, error) {
	if waitErr == nil {
		return adapter.Result{
			Outcome: "success",
			Outputs: buildOutputs(stdoutCS, stderrCS, 0, cfg.outputLimitBytes, sink),
		}, nil
	}

	exitCode := 0
	var exitErr *exec.ExitError
	isExitError := errors.As(waitErr, &exitErr)
	if isExitError {
		exitCode = exitErr.ExitCode()
	}

	// callerCancelled and stepTimedOut are stored as bools to avoid triggering
	// the nilerr linter on "if err != nil { return ..., nil }" patterns.
	callerCancelled := callerCtx.Err() != nil
	stepTimedOut := timeoutCtx.Err() != nil

	switch {
	case callerCancelled:
		return adapter.Result{Outcome: "failure"}, callerCtx.Err()
	case stepTimedOut:
		sink.Adapter("adapter", map[string]any{
			"event_type": "timeout",
			"limit":      cfg.timeout.String(),
		})
		return adapter.Result{ //nolint:nilerr // timeout is a step outcome, not a Go error
			Outcome: "failure",
			Outputs: buildOutputs(stdoutCS, stderrCS, exitCode, cfg.outputLimitBytes, sink),
		}, nil
	case isExitError:
		// Non-zero exit code is a normal step failure, not a Go error.
		return adapter.Result{
			Outcome: "failure",
			Outputs: buildOutputs(stdoutCS, stderrCS, exitCode, cfg.outputLimitBytes, sink),
		}, nil
	default:
		return adapter.Result{Outcome: "failure"}, waitErr
	}
}

// buildOutputs assembles the Outputs map from captured buffers and exit code.
// Emits an "output_truncated" adapter event for any stream whose buffer was
// truncated, and adds a _truncated_<stream>: "true" sentinel to outputs.
func buildOutputs(stdoutCS, stderrCS *captureState, exitCode int, limit int64, sink adapter.EventSink) map[string]string {
	outputs := map[string]string{
		"stdout":    stdoutCS.content(),
		"stderr":    stderrCS.content(),
		"exit_code": strconv.Itoa(exitCode),
	}
	if d := stdoutCS.droppedBytes(); d > 0 {
		sink.Adapter("adapter", map[string]any{
			"event_type":    "output_truncated",
			"stream":        "stdout",
			"dropped_bytes": d,
			"limit_bytes":   limit,
		})
		outputs["_truncated_stdout"] = "true"
	}
	if d := stderrCS.droppedBytes(); d > 0 {
		sink.Adapter("adapter", map[string]any{
			"event_type":    "output_truncated",
			"stream":        "stderr",
			"dropped_bytes": d,
			"limit_bytes":   limit,
		})
		outputs["_truncated_stderr"] = "true"
	}
	return outputs
}

// pumpStream reads from r in chunks, streams each chunk to sink, and writes
// to cs for bounded capture. Using chunk-based (not line-based) reading ensures
// the pipe is always drained even when output contains no newlines, preventing
// the subprocess from blocking on a full pipe write.
func pumpStream(wg *sync.WaitGroup, r io.Reader, stream string, sink adapter.EventSink, cs *captureState) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			sink.Log(stream, chunk)
			cs.write(chunk)
		}
		if err != nil {
			if err != io.EOF {
				sink.Log(stream, []byte(stream+" read error: "+err.Error()+"\n"))
			}
			return
		}
	}
}

// defaultShell returns the shell binary and flag for the current OS.
func defaultShell() (string, string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/C"
	}
	return "sh", "-c"
}
