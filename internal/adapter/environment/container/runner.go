package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin/runner"
)

// dockerRunner implements runner.Runner for docker/podman containers.
// It translates go-plugin lifecycle calls into docker CLI invocations.
type dockerRunner struct {
	runtime  string
	imageRef string
	cid      string
	cidFile  string
	timeout  string

	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	waitErr error

	done chan struct{}
	wg   sync.WaitGroup
}

// NewDockerRunner creates a runner.Runner that launches the adapter inside a
// container. The plugin handshake env vars from cmd.Env are forwarded into the
// container. socketDir is mounted at the same path so Unix socket paths need no
// translation.
func NewDockerRunner(_ hclog.Logger, cmd *exec.Cmd, socketDir string, prepared *Prepared) (runner.Runner, error) {
	envVars := extractEnvVars(cmd)
	cidFile := filepath.Join(os.TempDir(), "criteria-cid-"+uniqueID()+".txt")
	args := buildDockerArgs(envVars, socketDir, cidFile, prepared)

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
		timeout:  prepared.Policy.Timeout,
		cmd:      runtimeCmd,
		stdout:   stdout,
		stderr:   stderr,
		done:     make(chan struct{}),
	}, nil
}

func extractEnvVars(cmd *exec.Cmd) map[string]string {
	envVars := make(map[string]string)
	for _, e := range cmd.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			// Whitelist only go-plugin handshake variables and the Criteria
			// plugin cookie. This prevents accidental secret leakage into the
			// container via -e flags (D72/D73).
			if isHandshakeVar(k) {
				envVars[k] = v
			}
		}
	}
	return envVars
}

// isHandshakeVar returns true for known go-plugin handshake environment
// variables that are safe to forward into the container.
func isHandshakeVar(k string) bool {
	return k == "CRITERIA_PLUGIN" ||
		strings.HasPrefix(k, "PLUGIN_")
}

func buildDockerArgs(envVars map[string]string, socketDir, cidFile string, prepared *Prepared) []string {
	args := []string{"run", "--rm", "-i"}
	args = append(args, "-v", socketDir+":"+socketDir)
	keys := make([]string, 0, len(envVars))
	for k := range envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+envVars[k])
	}
	if prepared.Policy.NetworkName != "" {
		args = append(args, "--network", prepared.Policy.NetworkName)
	} else if prepared.Policy.NetworkMode != "" {
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
	args = append(args, "--cidfile", cidFile, prepared.ImageRef.Ref)
	return args
}

// Start launches the container and waits for the CID file to appear.
func (r *dockerRunner) Start(ctx context.Context) error {
	if err := r.cmd.Start(); err != nil {
		return err
	}

	waitCtx := ctx
	if r.timeout != "" {
		d, err := time.ParseDuration(r.timeout)
		if err == nil {
			var cancel context.CancelFunc
			waitCtx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}

	cid, err := r.waitForCID(waitCtx)
	if err != nil {
		_ = r.cmd.Process.Kill()
		_ = os.Remove(r.cidFile)
		return fmt.Errorf("container started but CID not available: %w", err)
	}
	r.cid = cid

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.waitErr = r.cmd.Wait()
		close(r.done)
		_ = os.Remove(r.cidFile)
	}()

	return nil
}

func (r *dockerRunner) waitForCID(ctx context.Context) (string, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for cidfile %s: %w", r.cidFile, ctx.Err())
		case <-ticker.C:
			data, err := os.ReadFile(r.cidFile)
			if err == nil && len(data) > 0 {
				return strings.TrimSpace(string(data)), nil
			}
		}
	}
}

// Wait blocks until the container exits or the context is cancelled.
func (r *dockerRunner) Wait(ctx context.Context) error {
	select {
	case <-r.done:
		return r.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Kill stops the container via docker kill (preferred) and falls back to
// process kill.
func (r *dockerRunner) Kill(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var killErr error
	if r.cid != "" {
		cmd := exec.CommandContext(ctx, r.runtime, "kill", r.cid)
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
func (r *dockerRunner) PluginToHost(pluginNet, pluginAddr string) (hostNet, hostAddr string, err error) {
	return pluginNet, pluginAddr, nil
}

// HostToPlugin is an identity translator because socketDir is mounted at
// the same path in host and container.
func (r *dockerRunner) HostToPlugin(hostNet, hostAddr string) (pluginNet, pluginAddr string, err error) {
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
