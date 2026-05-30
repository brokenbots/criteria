package container

import (
	"errors"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/workflow"
)

func TestFailClosed_Error(t *testing.T) {
	fc := FailClosed{
		Reason:    "adapter does not publish a container image",
		Adapter:   "noop.default",
		SourceURL: "https://github.com/example/noop",
		Runtime:   "docker",
	}
	msg := fc.Error()
	if !strings.Contains(msg, "noop.default does not publish a container image") {
		t.Errorf("error message missing adapter ref: %s", msg)
	}
	if !strings.Contains(msg, `environment.runtime = "docker"`) {
		t.Errorf("error message missing runtime: %s", msg)
	}
	if !strings.Contains(msg, "Publisher: https://github.com/example/noop") {
		t.Errorf("error message missing source URL: %s", msg)
	}
	if !strings.Contains(msg, "Ask the publisher to enable image publishing") {
		t.Errorf("error message missing guidance: %s", msg)
	}
}

func TestHandler_Prepare_RuntimeNone(t *testing.T) {
	h := &Handler{}
	_, err := h.Prepare(&PrepareContext{
		Environment: workflow.EnvironmentNode{
			Name:         "dev",
			TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("none")},
		},
		Manifest:   &manifest.Manifest{SourceURL: "https://example.com"},
		AdapterRef: "noop.default",
	})
	if err == nil {
		t.Fatal("expected error for runtime=none")
	}
	if !strings.Contains(err.Error(), `runtime = "none"`) {
		t.Errorf("expected runtime=none error, got: %v", err)
	}
}

func TestHandler_Prepare_FailClosed(t *testing.T) {
	h := &Handler{}
	_, err := h.Prepare(&PrepareContext{
		Environment: workflow.EnvironmentNode{
			Name:         "dev",
			TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("docker")},
		},
		Manifest:   &manifest.Manifest{SourceURL: "https://example.com"},
		AdapterRef: "noop.default",
	})
	if err == nil {
		t.Fatal("expected fail-closed error")
	}
	var fc FailClosed
	if !errors.As(err, &fc) {
		t.Fatalf("expected FailClosed, got: %T %v", err, err)
	}
}

func TestHandler_Prepare_DefaultRuntime(t *testing.T) {
	h := &Handler{}
	p, err := h.Prepare(&PrepareContext{
		Environment: workflow.EnvironmentNode{
			Name: "dev",
		},
		Manifest: &manifest.Manifest{
			SourceURL:      "https://example.com",
			ContainerImage: &manifest.ContainerImageRef{Ref: "alpine:latest"},
		},
		AdapterRef: "noop.default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Runtime != "docker" {
		t.Errorf("expected default runtime docker, got %q", p.Runtime)
	}
}

func TestHandler_Prepare_PolicyArgs(t *testing.T) {
	h := &Handler{}
	p, err := h.Prepare(&PrepareContext{
		Environment: workflow.EnvironmentNode{
			Name: "dev",
			TypeSpecific: map[string]cty.Value{
				"runtime": cty.StringVal("podman"),
				"network": cty.ObjectVal(map[string]cty.Value{
					"allow": cty.ListVal([]cty.Value{cty.StringVal("api.example.com:443")}),
				}),
				"filesystem": cty.ObjectVal(map[string]cty.Value{
					"read":  cty.ListVal([]cty.Value{cty.StringVal("/data")}),
					"write": cty.ListVal([]cty.Value{cty.StringVal("/tmp")}),
				}),
				"resources": cty.ObjectVal(map[string]cty.Value{
					"cpu":    cty.StringVal("2"),
					"memory": cty.StringVal("1Gi"),
				}),
			},
		},
		Manifest: &manifest.Manifest{
			SourceURL:      "https://example.com",
			ContainerImage: &manifest.ContainerImageRef{Ref: "alpine:latest"},
		},
		AdapterRef: "noop.default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Runtime != "podman" {
		t.Errorf("runtime = %q, want podman", p.Runtime)
	}
	if p.Policy.NetworkMode != "bridge" {
		t.Errorf("networkMode = %q, want bridge", p.Policy.NetworkMode)
	}
	if len(p.Policy.VolumeMounts) != 2 {
		t.Fatalf("expected 2 volume mounts, got %d", len(p.Policy.VolumeMounts))
	}
	if p.Policy.VolumeMounts[0].HostPath != "/data" || !p.Policy.VolumeMounts[0].ReadOnly {
		t.Errorf("first mount = %+v, want /data ro", p.Policy.VolumeMounts[0])
	}
	if p.Policy.VolumeMounts[1].HostPath != "/tmp" || p.Policy.VolumeMounts[1].ReadOnly {
		t.Errorf("second mount = %+v, want /tmp rw", p.Policy.VolumeMounts[1])
	}
	if p.Policy.CPUs != "2" {
		t.Errorf("cpus = %q, want 2", p.Policy.CPUs)
	}
	if p.Policy.Memory != "1Gi" {
		t.Errorf("memory = %q, want 1Gi", p.Policy.Memory)
	}
}

func TestHandler_Prepare_NetworkDeny(t *testing.T) {
	h := &Handler{}
	p, err := h.Prepare(&PrepareContext{
		Environment: workflow.EnvironmentNode{
			Name: "dev",
			TypeSpecific: map[string]cty.Value{
				"network": cty.ObjectVal(map[string]cty.Value{
					"allow": cty.ListValEmpty(cty.String),
				}),
			},
		},
		Manifest: &manifest.Manifest{
			SourceURL:      "https://example.com",
			ContainerImage: &manifest.ContainerImageRef{Ref: "alpine:latest"},
		},
		AdapterRef: "noop.default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Policy.NetworkMode != "none" {
		t.Errorf("networkMode = %q, want none", p.Policy.NetworkMode)
	}
}

func TestHandler_Prepare_InvalidPath(t *testing.T) {
	h := &Handler{}
	_, err := h.Prepare(&PrepareContext{
		Environment: workflow.EnvironmentNode{
			Name: "dev",
			TypeSpecific: map[string]cty.Value{
				"filesystem": cty.ObjectVal(map[string]cty.Value{
					"read": cty.ListVal([]cty.Value{cty.StringVal("../etc")}),
				}),
			},
		},
		Manifest: &manifest.Manifest{
			SourceURL:      "https://example.com",
			ContainerImage: &manifest.ContainerImageRef{Ref: "alpine:latest"},
		},
		AdapterRef: "noop.default",
	})
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}
