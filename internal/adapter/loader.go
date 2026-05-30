package adapter

import (
	"os/exec"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin/runner"

	"github.com/brokenbots/criteria/internal/adapter/environment/container"
	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// BuildContainerRunner inspects the workflow graph and lockfile to determine
// whether the named adapter instance should run inside a container. If so, it
// returns a go-plugin RunnerFunc that launches docker/podman. If the adapter
// is not container-bound, it returns nil. A non-nil error means the adapter
// is container-bound but cannot run (e.g. FailClosed).
func BuildContainerRunner(graph *workflow.FSMGraph, lf *lockfile.Lockfile, ref string) (func(hclog.Logger, *exec.Cmd, string) (runner.Runner, error), error) {
	if graph == nil {
		return nil, nil
	}
	adaptr, ok := graph.Adapters[ref]
	if !ok {
		return nil, nil // not an adapter instance
	}

	envKey := adaptr.Environment
	if envKey == "" {
		return nil, nil // no environment bound
	}

	envNode, ok := graph.Environments[envKey]
	if !ok {
		return nil, nil // environment not compiled
	}

	if envNode.Type != "container" {
		return nil, nil // not a container environment
	}

	mft := manifestFromLockfile(lf, adaptr.Type, adaptr.Name)

	h := &container.Handler{}
	prepared, err := h.Prepare(&container.PrepareContext{
		Environment: *envNode,
		Manifest:    mft,
		AdapterRef:  ref,
	})
	if err != nil {
		return nil, err
	}

	return func(_ hclog.Logger, cmd *exec.Cmd, socketDir string) (runner.Runner, error) {
		return container.NewDockerRunner(hclog.NewNullLogger(), cmd, socketDir, &prepared)
	}, nil
}

func manifestFromLockfile(lf *lockfile.Lockfile, typ, name string) *manifest.Manifest {
	mft := &manifest.Manifest{}
	if lf == nil {
		return mft
	}
	entry := findLockedAdapter(lf, typ, name)
	if entry == nil {
		return mft
	}
	mft.SourceURL = entry.SourceURL
	if entry.ContainerImage != nil {
		mft.ContainerImage = &manifest.ContainerImageRef{
			Ref:    entry.ContainerImage.Ref,
			Digest: entry.ContainerImage.Digest,
		}
	}
	return mft
}

func findLockedAdapter(lf *lockfile.Lockfile, typ, name string) *lockfile.LockedAdapter {
	for i := range lf.Adapters {
		if lf.Adapters[i].Type == typ && lf.Adapters[i].Name == name {
			return &lf.Adapters[i]
		}
	}
	return nil
}
