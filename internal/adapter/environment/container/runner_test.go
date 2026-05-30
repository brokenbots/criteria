package container

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
)

func TestNewDockerRunner_CommandArgs(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{
		"CRITERIA_PLUGIN=secret",
		"PLUGIN_UNIX_SOCKET_DIR=/tmp/sock",
	}}
	prepared := Prepared{
		Runtime:  "docker",
		ImageRef: manifest.ContainerImageRef{Ref: "alpine:latest"},
		Policy: PolicyArgs{
			NetworkMode: "bridge",
			VolumeMounts: []VolumeMount{
				{HostPath: "/data", ContainerPath: "/data", ReadOnly: true},
				{HostPath: "/tmp", ContainerPath: "/tmp", ReadOnly: false},
			},
			CPUs:   "2",
			Memory: "1Gi",
		},
	}

	logger := hclog.NewNullLogger()
	r, err := NewDockerRunner(logger, cmd, "/tmp/sock", &prepared)
	require.NoError(t, err)

	dr, ok := r.(*dockerRunner)
	require.True(t, ok)

	want := []string{
		"run", "--rm", "-i",
		"-v", "/tmp/sock:/tmp/sock",
		"-e", "CRITERIA_PLUGIN=secret",
		"-e", "PLUGIN_UNIX_SOCKET_DIR=/tmp/sock",
		"--network", "bridge",
		"-v", "/data:/data:ro",
		"-v", "/tmp:/tmp",
		"--cpus", "2",
		"--memory", "1Gi",
		"alpine:latest",
	}

	got := dr.cmd.Args
	require.GreaterOrEqual(t, len(got), 3)
	assert.Equal(t, "docker", got[0])

	// Drop the leading "docker" and the trailing cidfile value for comparison.
	argsWithoutBinary := got[1:]
	// Find cidfile index.
	cidIdx := -1
	for i, a := range argsWithoutBinary {
		if a == "--cidfile" {
			cidIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, cidIdx, 0)

	before := argsWithoutBinary[:cidIdx]
	after := argsWithoutBinary[cidIdx+2:]
	combined := make([]string, 0, len(before)+len(after))
	combined = append(combined, before...)
	combined = append(combined, after...)
	assert.Equal(t, want, combined)
}

func TestDockerRunner_AddrTranslators(t *testing.T) {
	dr := &dockerRunner{}

	hNet, hAddr, err := dr.HostToPlugin("unix", "/tmp/sock/plugin.sock")
	require.NoError(t, err)
	assert.Equal(t, "unix", hNet)
	assert.Equal(t, "/tmp/sock/plugin.sock", hAddr)

	pNet, pAddr, err := dr.PluginToHost("unix", "/tmp/sock/plugin.sock")
	require.NoError(t, err)
	assert.Equal(t, "unix", pNet)
	assert.Equal(t, "/tmp/sock/plugin.sock", pAddr)
}

func TestDockerRunner_NameAndID(t *testing.T) {
	dr := &dockerRunner{imageRef: "alpine:latest", cid: "abc123"}
	assert.Equal(t, "alpine:latest", dr.Name())
	assert.Equal(t, "abc123", dr.ID())
}

func TestDockerRunner_Diagnose_NoCID(t *testing.T) {
	dr := &dockerRunner{}
	msg := dr.Diagnose(context.Background())
	assert.Equal(t, "container has not started or CID is not available", msg)
}

func TestDockerRunner_Integration(t *testing.T) {
	if os.Getenv("CRITERIA_CONTAINER_TESTS") != "1" {
		t.Skip("set CRITERIA_CONTAINER_TESTS=1 to run container integration tests")
	}

	logger := hclog.NewNullLogger()
	cmd := &exec.Cmd{Env: []string{"CRITERIA_PLUGIN=secret", "PLUGIN_PROTOCOL_VERSIONS=1,2"}}
	prepared := Prepared{
		Runtime:  "docker",
		ImageRef: manifest.ContainerImageRef{Ref: "busybox:latest"},
		Policy:   PolicyArgs{NetworkMode: "none"},
	}

	r, err := NewDockerRunner(logger, cmd, t.TempDir(), &prepared)
	require.NoError(t, err)

	// Validate that the constructed docker args contain expected flags
	// before we override for the integration test.
	dr := r.(*dockerRunner)
	args := strings.Join(dr.cmd.Args, " ")
	assert.Contains(t, args, "docker run")
	assert.Contains(t, args, "--rm")
	assert.Contains(t, args, "--network none")
	assert.Contains(t, args, "-e CRITERIA_PLUGIN=secret")
	assert.Contains(t, args, "-e PLUGIN_PROTOCOL_VERSIONS=1,2")
	assert.Contains(t, args, "busybox:latest")

	// Override the command to run a short-lived container so we can observe
	// start/wait/kill without needing a real adapter.
	dr.cmd = exec.Command("docker", "run", "--rm", "-i", "busybox:latest", "sh", "-c", "sleep 30")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, dr.Start(ctx))
	require.NotEmpty(t, dr.cid)

	// Verify container is running.
	out, err := exec.Command("docker", "ps", "--filter", "id="+dr.cid, "--format", "{{.ID}}").CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(out), dr.cid[:12])

	// Kill it.
	require.NoError(t, dr.Kill(ctx))

	// Wait for the Wait goroutine.
	dr.wg.Wait()

	// Verify container is gone.
	out, _ = exec.Command("docker", "ps", "--filter", "id="+dr.cid, "--format", "{{.ID}}").CombinedOutput()
	assert.Empty(t, strings.TrimSpace(string(out)))
}
