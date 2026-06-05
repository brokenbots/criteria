package shell

// execute.go — the core command execution path for the shell adapter. It is
// expressed as a pure function (runShell) that takes the resolved step input
// and resolved secret environment, streams stdout/stderr chunks through an
// emit callback, and returns the outcome, typed outputs, and adapter events.
// This keeps the execution logic unit-testable in-process while the SDK
// Service in shell.go adapts it to the protocol-v2 wire.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
)

// adapterEvt is a structured adapter event emitted on the Execute stream
// (e.g. a timeout notice or an output-truncation notice).
type adapterEvt struct {
	kind    string
	payload map[string]any
}

// emitFunc streams a chunk of one output stream ("stdout"/"stderr") to the host.
type emitFunc func(stream string, chunk []byte)

// runShell executes the command declared in input["command"]. It applies the
// sandbox defaults (env allowlist, PATH sanitization, hard timeout, bounded
// output capture, working-directory confinement), streams output chunks via
// emit, and returns the step outcome, outputs, and any adapter events.
//
// A non-nil error means the step could not be run as configured (bad input,
// caller cancellation, or an unexpected wait error). A non-zero exit code or a
// step timeout is a normal "failure" outcome, not a Go error.
func runShell(ctx context.Context, input, secretEnv map[string]string, emit emitFunc) (outcome string, outputs map[string]any, events []adapterEvt, err error) {
	cmdStr, ok := input["command"]
	if !ok || cmdStr == "" {
		return "failure", nil, nil, errors.New("shell adapter: input.command is required")
	}

	cfg, err := buildSandboxConfig(input, secretEnv)
	if err != nil {
		return "failure", nil, nil, err
	}

	if cfg.workingDir != "" {
		if err := validateWorkingDirectory(cfg.workingDir); err != nil {
			return "failure", nil, nil, err
		}
	}

	// Create a step-level timeout context. cfg.timeout is always positive
	// (defaultTimeout or validated 1s–1h from step input).
	timeoutCtx, cancelTimeout := context.WithTimeout(ctx, cfg.timeout)
	defer cancelTimeout()

	cmd := buildCmd(timeoutCtx, cmdStr, cfg)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "failure", nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "failure", nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "failure", nil, nil, fmt.Errorf("start: %w", err)
	}

	stdoutCS := newCaptureState(cfg.outputLimitBytes)
	stderrCS := newCaptureState(cfg.outputLimitBytes)
	drainPumps(timeoutCtx, stdoutPipe, stderrPipe, emit, stdoutCS, stderrCS)

	return resolveWait(cmd.Wait(), ctx, timeoutCtx, cfg, stdoutCS, stderrCS)
}

// drainPumps starts both stream pumps and blocks until they exit. To unblock
// pumps promptly when timeoutCtx fires (caller cancellation or step timeout),
// it spawns a watcher goroutine that closes the pipe read-ends — necessary
// because grandchildren spawned by `sh -c` may hold the write-ends open after
// the parent sh has been killed, which would otherwise leave the pumps
// blocked indefinitely on Read.
func drainPumps(
	timeoutCtx context.Context,
	stdoutPipe, stderrPipe io.ReadCloser,
	emit emitFunc,
	stdoutCS, stderrCS *captureState,
) {
	pumpsDone := make(chan struct{})
	go func() {
		select {
		case <-timeoutCtx.Done():
			_ = stdoutPipe.Close()
			_ = stderrPipe.Close()
		case <-pumpsDone:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go pumpStream(&wg, stdoutPipe, "stdout", emit, stdoutCS)
	go pumpStream(&wg, stderrPipe, "stderr", emit, stderrCS)
	wg.Wait()
	close(pumpsDone)
}

// buildCmd constructs and configures the exec.Cmd for the sandbox.
func buildCmd(timeoutCtx context.Context, cmdStr string, cfg sandboxConfig) *exec.Cmd {
	sh, flag := defaultShell()
	cmd := exec.CommandContext(timeoutCtx, sh, flag, cmdStr)
	setSIGTERMCancel(cmd)
	cmd.WaitDelay = killGrace
	cmd.Env = cfg.env
	if cfg.workingDir != "" {
		cmd.Dir = cfg.workingDir
	}
	return cmd
}

// resolveWait interprets the error returned by cmd.Wait and builds the outcome,
// outputs, and adapter events. It distinguishes caller-context cancellation
// from step-level timeout from normal non-zero exit.
func resolveWait(
	waitErr error,
	callerCtx, timeoutCtx context.Context,
	cfg sandboxConfig,
	stdoutCS, stderrCS *captureState,
) (outcome string, outputs map[string]any, events []adapterEvt, err error) {
	if waitErr == nil {
		outputs, events := buildOutputs(stdoutCS, stderrCS, 0, cfg.outputLimitBytes)
		return "success", outputs, events, nil
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
		return "failure", nil, nil, callerCtx.Err()
	case stepTimedOut:
		outputs, events := buildOutputs(stdoutCS, stderrCS, exitCode, cfg.outputLimitBytes)
		events = append([]adapterEvt{{
			kind:    "adapter",
			payload: map[string]any{"event_type": "timeout", "limit": cfg.timeout.String()},
		}}, events...)
		return "failure", outputs, events, nil //nolint:nilerr // timeout is a step outcome, not a Go error
	case isExitError:
		// Non-zero exit code is a normal step failure, not a Go error.
		outputs, events := buildOutputs(stdoutCS, stderrCS, exitCode, cfg.outputLimitBytes)
		return "failure", outputs, events, nil
	default:
		return "failure", nil, nil, waitErr
	}
}

// buildOutputs assembles the outputs map from captured buffers and exit code,
// plus an "output_truncated" adapter event for any stream whose buffer was
// truncated (with a _truncated_<stream>: "true" sentinel in outputs).
func buildOutputs(stdoutCS, stderrCS *captureState, exitCode int, limit int64) (map[string]any, []adapterEvt) {
	// Shell outputs are declared as strings in the schema (exit_code included),
	// so emit string values to honor that contract.
	outputs := map[string]any{
		"stdout":    stdoutCS.content(),
		"stderr":    stderrCS.content(),
		"exit_code": strconv.Itoa(exitCode),
	}
	var events []adapterEvt
	if d := stdoutCS.droppedBytes(); d > 0 {
		events = append(events, truncationEvent("stdout", d, limit))
		outputs["_truncated_stdout"] = "true"
	}
	if d := stderrCS.droppedBytes(); d > 0 {
		events = append(events, truncationEvent("stderr", d, limit))
		outputs["_truncated_stderr"] = "true"
	}
	return outputs, events
}

func truncationEvent(stream string, dropped, limit int64) adapterEvt {
	return adapterEvt{
		kind: "adapter",
		payload: map[string]any{
			"event_type":    "output_truncated",
			"stream":        stream,
			"dropped_bytes": dropped,
			"limit_bytes":   limit,
		},
	}
}

// pumpStream reads from r in chunks, streams each chunk via emit, and writes
// to cs for bounded capture. Using chunk-based (not line-based) reading ensures
// the pipe is always drained even when output contains no newlines, preventing
// the subprocess from blocking on a full pipe write.
func pumpStream(wg *sync.WaitGroup, r io.Reader, stream string, emit emitFunc, cs *captureState) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			emit(stream, chunk)
			cs.write(chunk)
		}
		if err != nil {
			if err != io.EOF {
				emit(stream, []byte(stream+" read error: "+err.Error()+"\n"))
			}
			return
		}
	}
}

// defaultShell returns the shell binary and flag for the current OS.
func defaultShell() (shell, flag string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/C"
	}
	return "sh", "-c"
}
