package container

import (
	"context"
	"fmt"
	"os/exec"
)

// PullContainerImage ensures the image referenced in adapter.yaml is present
// in the local docker/podman daemon. Uses docker pull / podman pull shelled
// out via os/exec.
func PullContainerImage(ctx context.Context, ref, runtime string) error {
	if runtime == "" {
		runtime = "docker"
	}
	cmd := exec.CommandContext(ctx, runtime, "pull", ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s pull %s: %w\noutput: %s", runtime, ref, err, out)
	}
	return nil
}
