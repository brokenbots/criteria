package container

// runner_lifecycle_test.go — exercises dockerRunner and PullContainerImage
// against a fake container-runtime binary on PATH. The runner intentionally
// shells out to the docker/podman CLI; a stub runtime keeps the tests hermetic
// while still covering the real exec paths (Start/CID wait/Wait/Kill, log
// retrieval, pull) that the arg-construction unit tests cannot reach.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
)

// installFakeRuntime writes the fake runtime script to a temp dir and
// prepends it to PATH. Behaviour branches on CRITERIA_FAKE_RUNTIME_MODE and
// every invocation is appended to CRITERIA_FAKE_RUNTIME_ARGS so tests can
// assert the exact CLI arguments the runner built.
func installFakeRuntime(t *testing.T, mode, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake PATH binaries require a POSIX shell")
	}

	const script = `#!/bin/sh
argsfile="$CRITERIA_FAKE_RUNTIME_ARGS"
[ -n "$argsfile" ] && printf '%s\n' "$*" >> "$argsfile"

subcmd="$1"
if [ "$subcmd" = "pull" ]; then
  [ "$CRITERIA_FAKE_RUNTIME_MODE" = "pull-fail" ] && { echo "manifest unknown" >&2; exit 1; }
  exit 0
fi
if [ "$subcmd" = "logs" ]; then
  echo "container log line 1"
  echo "container log line 2"
  exit 0
fi
if [ "$subcmd" = "kill" ]; then
  [ "$CRITERIA_FAKE_RUNTIME_MODE" = "kill-fail" ] && exit 1
  exit 0
fi

cidfile=""
prev=""
for a in "$@"; do
  [ "$prev" = "--cidfile" ] && cidfile="$a"
  prev="$a"
done
if [ "$CRITERIA_FAKE_RUNTIME_MODE" = "no-cid" ]; then
  exec sleep 5
fi
echo "fake-cid-1234567890abcdef" > "$cidfile"
echo "hello-from-container"
echo "err-from-container" >&2
exec sleep 300
`
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRITERIA_FAKE_RUNTIME_MODE", mode)
}

// fakeRuntimeArgsFile returns the unique args journal for one test.
func fakeRuntimeArgsFile(t *testing.T) string {
	t.Helper()
	argsPath := filepath.Join(t.TempDir(), "runtime-args.txt")
	t.Setenv("CRITERIA_FAKE_RUNTIME_ARGS", argsPath)
	return argsPath
}

// newFakeRunner builds a dockerRunner backed by the fake runtime installed by
// installFakeRuntime.
func newFakeRunner(t *testing.T, mode string) (dr *dockerRunner, argsFile string) {
	t.Helper()
	installFakeRuntime(t, mode, "fake-runtime")
	argsFile = fakeRuntimeArgsFile(t)

	cmd := &exec.Cmd{Env: []string{"PLUGIN_MIN_PROTOCOL_VERSION=1"}}
	prepared := Prepared{
		Runtime:  "fake-runtime",
		ImageRef: manifest.ContainerImageRef{Ref: "alpine:latest"},
	}

	r, err := NewDockerRunner(hclog.NewNullLogger(), cmd, t.TempDir(), &prepared)
	require.NoError(t, err)
	dr, ok := r.(*dockerRunner)
	require.True(t, ok)
	return dr, argsFile
}

// reapRunner kills the underlying process if it is still running and waits
// for the Wait goroutine to finish so no process is left behind.
func reapRunner(t *testing.T, dr *dockerRunner) {
	t.Helper()
	if dr.cmd.Process != nil {
		_ = dr.cmd.Process.Kill()
	}
	dr.wg.Wait()
}

func TestDockerRunner_Lifecycle(t *testing.T) {
	dr, argsFile := newFakeRunner(t, "run")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, dr.Start(ctx))
	require.Equal(t, "fake-cid-1234567890abcdef", dr.cid)
	require.Equal(t, "fake-cid-1234567890abcdef", dr.ID())
	require.NotNil(t, dr.Stdout(), "stdout stream must be wired to the runtime process")
	require.NotNil(t, dr.Stderr(), "stderr stream must be wired to the runtime process")

	// The run invocation carries the image and the cidfile flag.
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	firstLine := strings.SplitN(string(args), "\n", 2)[0]
	firstLineFields := strings.Fields(firstLine)
	require.Equal(t, []string{"run", "--rm", "-i"}, firstLineFields[0:3])
	require.Contains(t, firstLine, "alpine:latest")
	require.Contains(t, firstLine, "--cidfile")

	// Stop the container through the runner: the kill subcommand must be
	// invoked with the CID and the process must be reaped.
	require.NoError(t, dr.Kill(ctx))
	dr.wg.Wait()

	args, err = os.ReadFile(argsFile)
	require.NoError(t, err)
	require.Contains(t, string(args), "kill fake-cid-1234567890abcdef")

	// Wait reports the killed process's exit error.
	require.Error(t, dr.Wait(context.Background()))

	// Diagnose pulls container logs via the runtime.
	logs := dr.Diagnose(context.Background())
	require.Contains(t, logs, "container log line 1")
	require.Contains(t, logs, "container log line 2")

	// The CID file is cleaned up after the run completes.
	_, err = os.Stat(dr.cidFile)
	require.True(t, errors.Is(err, os.ErrNotExist))
}

func TestDockerRunner_Start_CIDTimeout(t *testing.T) {
	// no-cid mode never writes the CID file, so Start must time out, kill the
	// runtime process, and clean up the CID file. The Wait goroutine was never
	// started on this path.
	dr, _ := newFakeRunner(t, "no-cid")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := dr.Start(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out waiting for cidfile")

	_, statErr := os.Stat(dr.cidFile)
	require.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestDockerRunner_Start_InvalidPolicyTimeout(t *testing.T) {
	// A policy timeout that does not parse as a duration must be ignored
	// gracefully: Start keeps waiting on the caller's context.
	dr, _ := newFakeRunner(t, "no-cid")
	dr.timeout = "not-a-duration"

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	require.Error(t, dr.Start(ctx))
}

func TestDockerRunner_Start_MissingRuntimeBinary(t *testing.T) {
	// The runtime binary does not exist: cmd.Start fails immediately.
	dr, _ := newFakeRunner(t, "run")
	dr.cmd.Path = filepath.Join(t.TempDir(), "does-not-exist")

	require.Error(t, dr.Start(context.Background()))
}

func TestDockerRunner_Kill_BeforeStart(t *testing.T) {
	// No process has been started and there is no CID: Kill is a no-op.
	dr, _ := newFakeRunner(t, "run")

	require.NoError(t, dr.Kill(context.Background()))
}

func TestDockerRunner_Kill_CancelledContext(t *testing.T) {
	dr, _ := newFakeRunner(t, "run")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, dr.Kill(ctx), context.Canceled)
}

func TestDockerRunner_Kill_KillSubcommandFails(t *testing.T) {
	// The runtime kill subcommand exits non-zero while the process is still
	// alive: Kill must surface that error rather than swallowing it.
	dr, _ := newFakeRunner(t, "kill-fail")

	ctx := context.Background()
	require.NoError(t, dr.Start(ctx))
	defer reapRunner(t, dr)

	err := dr.Kill(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exit status 1")
}

func TestDockerRunner_Kill_AfterProcessExit(t *testing.T) {
	// Killing an already-exited process must not fail: the
	// os.ErrProcessDone case is tolerated.
	dr, _ := newFakeRunner(t, "run")

	ctx := context.Background()
	require.NoError(t, dr.Start(ctx))

	dr.cmd.Process.Kill()
	dr.wg.Wait()

	require.NoError(t, dr.Kill(ctx))
}

func TestDockerRunner_Diagnose_WithoutCID(t *testing.T) {
	dr, _ := newFakeRunner(t, "run")
	require.Equal(t, "container has not started or CID is not available", dr.Diagnose(context.Background()))
}

func TestPullContainerImage(t *testing.T) {
	ctx := context.Background()

	t.Run("explicit_runtime", func(t *testing.T) {
		argsFile := fakeRuntimeArgsFile(t)
		installFakeRuntime(t, "ok", "fake-pull")

		require.NoError(t, PullContainerImage(ctx, manifest.ContainerImageRef{Ref: "alpine:3.20"}, "fake-pull"))

		args, err := os.ReadFile(argsFile)
		require.NoError(t, err)
		require.Equal(t, "pull alpine:3.20\n", string(args))
	})

	t.Run("digest_pinned_ref", func(t *testing.T) {
		argsFile := fakeRuntimeArgsFile(t)
		installFakeRuntime(t, "ok", "fake-pull")

		ref := manifest.ContainerImageRef{Ref: "alpine", Digest: "sha256:abc123"}
		require.NoError(t, PullContainerImage(ctx, ref, "fake-pull"))

		args, err := os.ReadFile(argsFile)
		require.NoError(t, err)
		require.Equal(t, "pull alpine@sha256:abc123\n", string(args))
	})

	t.Run("default_runtime_is_docker", func(t *testing.T) {
		argsFile := fakeRuntimeArgsFile(t)
		installFakeRuntime(t, "ok", "docker")

		require.NoError(t, PullContainerImage(ctx, manifest.ContainerImageRef{Ref: "alpine:latest"}, ""))

		args, err := os.ReadFile(argsFile)
		require.NoError(t, err)
		require.Equal(t, "pull alpine:latest\n", string(args))
	})

	t.Run("pull_failure_includes_output", func(t *testing.T) {
		fakeRuntimeArgsFile(t)
		installFakeRuntime(t, "pull-fail", "fake-pull")

		err := PullContainerImage(ctx, manifest.ContainerImageRef{Ref: "alpine:3.20"}, "fake-pull")
		require.Error(t, err)
		require.Contains(t, err.Error(), "fake-pull pull alpine:3.20")
		require.Contains(t, err.Error(), "manifest unknown")
	})
}
