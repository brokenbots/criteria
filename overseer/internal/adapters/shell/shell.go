// Package shell implements the `shell` adapter. It runs an arbitrary command
// and maps the exit code to a step outcome (0 -> "success", non-zero ->
// "failure"). Stdout and stderr are streamed to the sink.
package shell

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"sync"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/workflow"
)

const Name = "shell"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return Name }

func (a *Adapter) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	cmdStr, ok := step.Config["command"]
	if !ok || cmdStr == "" {
		return adapter.Result{Outcome: "failure"}, errors.New("shell adapter: config.command is required")
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

	var wg sync.WaitGroup
	wg.Add(2)
	go pump(&wg, stdout, "stdout", sink)
	go pump(&wg, stderr, "stderr", sink)
	wg.Wait()

	err = cmd.Wait()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return adapter.Result{Outcome: "failure"}, nil
		}
		return adapter.Result{Outcome: "failure"}, err
	}
	return adapter.Result{Outcome: "success"}, nil
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
}

func defaultShell() (string, string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/C"
	}
	return "sh", "-c"
}
