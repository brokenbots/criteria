// Package shell implements the `shell` adapter. It runs an arbitrary command
// and maps the exit code to a step outcome (0 -> "success", non-zero ->
// "failure"). Stdout and stderr are streamed to the sink. Stdout is also
// captured into Outputs["stdout"] (capped at 64 KB) and the exit code into
// Outputs["exit_code"] (W04).
package shell

import (
	"bufio"
	"bytes"
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

const (
	Name           = "shell"
	stdoutCapBytes = 64 * 1024 // 64 KB stdout capture cap
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return Name }

func (a *Adapter) Info() workflow.AdapterInfo {
	return workflow.AdapterInfo{
		InputSchema: map[string]workflow.ConfigField{
			"command": {Required: true, Type: workflow.ConfigFieldString, Doc: "Shell command to execute."},
		},
		OutputSchema: map[string]workflow.ConfigField{
			"stdout":    {Type: workflow.ConfigFieldString, Doc: "Captured stdout (capped at 64 KB)."},
			"exit_code": {Type: workflow.ConfigFieldString, Doc: "Process exit code as a string."},
		},
	}
}

func (a *Adapter) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	cmdStr, ok := step.Input["command"]
	if !ok || cmdStr == "" {
		return adapter.Result{Outcome: "failure"}, errors.New("shell adapter: input.command is required")
	}

	shell, flag := defaultShell()
	cmd := exec.CommandContext(ctx, shell, flag, cmdStr)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("start: %w", err)
	}

	var (
		wg        sync.WaitGroup
		stdoutBuf bytes.Buffer
		stdoutMu  sync.Mutex
	)
	wg.Add(2)
	go pumpCapture(&wg, stdout, "stdout", sink, &stdoutBuf, &stdoutMu)
	go pump(&wg, stderr, "stderr", sink)
	wg.Wait()

	exitCode := 0
	err = cmd.Wait()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return adapter.Result{Outcome: "failure"}, ctxErr
			}
			return adapter.Result{
				Outcome: "failure",
				Outputs: captureOutputs(&stdoutBuf, exitCode),
			}, nil
		}
		return adapter.Result{Outcome: "failure"}, err
	}
	return adapter.Result{
		Outcome: "success",
		Outputs: captureOutputs(&stdoutBuf, exitCode),
	}, nil
}

// captureOutputs builds the Outputs map from the captured stdout buffer and
// exit code. The buffer is already capped at stdoutCapBytes by pumpCapture.
func captureOutputs(buf *bytes.Buffer, exitCode int) map[string]string {
	return map[string]string{
		"stdout":    buf.String(),
		"exit_code": strconv.Itoa(exitCode),
	}
}

func pump(wg *sync.WaitGroup, r io.Reader, stream string, sink adapter.EventSink) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		line = append(line, '\n')
		sink.Log(stream, line)
	}
	if err := scanner.Err(); err != nil {
		sink.Log(stream, []byte(stream+" read error: "+err.Error()+"\n"))
	}
}

// pumpCapture streams r to sink under the given stream name and also writes
// to buf (for Outputs capture), protected by mu. Writes to buf are skipped
// once buf reaches stdoutCapBytes so buffer growth is bounded while the
// command is running (not just at serialisation time).
func pumpCapture(wg *sync.WaitGroup, r io.Reader, stream string, sink adapter.EventSink, buf *bytes.Buffer, mu *sync.Mutex) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		b := scanner.Bytes()
		line := append([]byte(nil), b...)
		line = append(line, '\n')
		sink.Log(stream, line)
		mu.Lock()
		if remaining := stdoutCapBytes - buf.Len(); remaining > 0 {
			if len(line) <= remaining {
				buf.Write(line)
			} else {
				buf.Write(line[:remaining])
			}
		}
		mu.Unlock()
	}
	if err := scanner.Err(); err != nil {
		sink.Log(stream, []byte("stdout read error: "+err.Error()+"\n"))
	}
}

func defaultShell() (string, string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/C"
	}
	return "sh", "-c"
}
