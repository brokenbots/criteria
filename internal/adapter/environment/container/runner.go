package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin/runner"
)

// dockerRunner implements runner.Runner for docker/podman containers.
// It translates go-plugin lifecycle calls into docker CLI invocations.
type dockerRunner struct {
	runtime  string // "docker" or "podman"
	imageRef string
	cid      string
	cidFile  string

	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu   sync.Mutex
	done chan struct{}
	wg   sync.WaitGroup
}

// NewDockerRunner creates a runner.Runner that launches the adapter inside a
// container. The plugin handshake env vars from cmd.Env are forwarded into the
// container. socketDir is mounted at the same path so Unix socket paths need no
// translation.
func NewDockerRunner(_ hclog.Logger, cmd *exec.Cmd, socketDir string, prepared Prepared) (runner.Runner, error) {
	// Extract env vars from cmd.Env.
	envVars := make(map[string]string)
	for _, e := range cmd.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			envVars[k] = v
		}
	}

	args := []string{"run", "--rm", "-i"}

	// Mount the Unix socket directory at the same path (identity mapping).
	args = append(args, "-v", socketDir+":"+socketDir)

	// Forward all plugin handshake env vars.
	for k, v := range envVars {
		args = append(args, "-e", k+"="+v)
	}

	// Policy-derived flags.
	if prepared.Policy.NetworkMode != "" {
		args = append(args, "--network", prepared.Policy.NetworkMode)
	}
	for _, vm := range prepared.Policy.VolumeMounts {
		mount := vm.HostPath + ":" + vm.ContainerPath
		if vm.ReadOnly {
			mount += ":ro"
		}
		args = append(args, "-v", mount)
	}
	if prepared.Policy.CPUs != "" {
		args = append(args, "--cpus", prepared.Policy.CPUs)
	}
	if prepared.Policy.Memory != "" {
		args = append(args, "--memory", prepared.Policy.Memory)
	}

	// Capture container ID for lifecycle management.
	cidFile := filepath.Join(os.TempDir(), "criteria-cid-"+uniqueID()+".txt")
	args = append(args, "--cidfile", cidFile)

	// Image reference.
	args = append(args, prepared.ImageRef.Ref)

	runtimeCmd := exec.Command(prepared.Runtime, args...)
	runtimeCmd.Stdin = os.Stdin

	stdout, err := runtimeCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("container stdout pipe: %w", err)
	}
	stderr, err := runtimeCmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("container stderr pipe: %w", err)
	}

	return &dockerRunner{
		runtime:  prepared.Runtime,
		imageRef: prepared.ImageRef.Ref,
		cidFile:  cidFile,
		cmd:      runtimeCmd,
		stdout:   stdout,
		stderr:   stderr,
		done:     make(chan struct{}),
	}, nil
}

// Start launches the container and waits for the CID file to appear.
func (r *dockerRunner) Start(_ context.Context) error {
	if err := r.cmd.Start(); err != nil {
		return err
	}

	cid, err := r.waitForCID(5 * time.Second)
	if err != nil {
		_ = r.cmd.Process.Kill()
		_ = os.Remove(r.cidFile)
		return fmt.Errorf("container started but CID not available: %w", err)
	}
	r.cid = cid

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		_ = r.cmd.Wait()
		close(r.done)
		_ = os.Remove(r.cidFile)
	}()

	return nil
}

func (r *dockerRunner) waitForCID(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(r.cidFile)
		if err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data)), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for cidfile %s", r.cidFile)
}

// Wait blocks until the container exits.
func (r *dockerRunner) Wait(_ context.Context) error {
	<-r.done
	return r.cmd.Wait()
}

// Kill stops the container via docker kill (preferred) and falls back to
// process kill.
func (r *dockerRunner) Kill(_ context.Context) error {
	var killErr error
	if r.cid != "" {
		cmd := exec.Command(r.runtime, "kill", r.cid)
		killErr = cmd.Run()
	}
	if r.cmd.Process != nil {
		if err := r.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			if killErr != nil {
				return killErr
			}
			return err
		}
	}
	return killErr
}

// ID returns the container ID.
func (r *dockerRunner) ID() string {
	return r.cid
}

// Name returns the image reference for diagnostics.
func (r *dockerRunner) Name() string {
	return r.imageRef
}

// Stdout returns the container stdout stream.
func (r *dockerRunner) Stdout() io.ReadCloser {
	return r.stdout
}

// Stderr returns the container stderr stream.
func (r *dockerRunner) Stderr() io.ReadCloser {
	return r.stderr
}

// PluginToHost is an identity translator because socketDir is mounted at
// the same path in host and container.
func (r *dockerRunner) PluginToHost(pluginNet, pluginAddr string) (string, string, error) {
	return pluginNet, pluginAddr, nil
}

// HostToPlugin is an identity translator because socketDir is mounted at
// the same path in host and container.
func (r *dockerRunner) HostToPlugin(hostNet, hostAddr string) (string, string, error) {
	return hostNet, hostAddr, nil
}

// Diagnose returns the last 50 lines of container logs.
func (r *dockerRunner) Diagnose(_ context.Context) string {
	if r.cid == "" {
		return "container has not started or CID is not available"
	}
	out, err := exec.Command(r.runtime, "logs", "--tail", "50", r.cid).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("failed to retrieve logs: %v", err)
	}
	return string(out)
}

// uniqueID returns a simple process-unique string for temp file names.
func uniqueID() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}
