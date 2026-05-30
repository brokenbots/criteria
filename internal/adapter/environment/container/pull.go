package container

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
)

// PullContainerImage ensures the image referenced in adapter.yaml is present
// in the local docker/podman daemon. Uses docker pull / podman pull shelled
// out via os/exec.
func PullContainerImage(ctx context.Context, ref manifest.ContainerImageRef, runtime string) error {
	if runtime == "" {
		runtime = "docker"
	}

	imageRef := ref.Ref
	if ref.Digest != "" {
		imageRef = ref.Ref + "@" + ref.Digest
	}

	cmd := exec.CommandContext(ctx, runtime, "pull", imageRef)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s pull %s: %w\noutput: %s", runtime, imageRef, err, out)
	}
	return nil
}
