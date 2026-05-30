package adapter

import (
	"errors"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/environment/container"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestBuildContainerRunner_NilGraph(t *testing.T) {
	rf, err := BuildContainerRunner(nil, nil, "noop.default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rf != nil {
		t.Fatal("expected nil runner func for nil graph")
	}
}

func TestBuildContainerRunner_NotAdapter(t *testing.T) {
	g := &workflow.FSMGraph{Adapters: map[string]*workflow.AdapterNode{}}
	rf, err := BuildContainerRunner(g, nil, "noop.default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rf != nil {
		t.Fatal("expected nil runner func for missing adapter")
	}
}

func TestBuildContainerRunner_NoEnvironment(t *testing.T) {
	g := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default", Environment: ""},
		},
	}
	rf, err := BuildContainerRunner(g, nil, "noop.default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rf != nil {
		t.Fatal("expected nil runner func for adapter with no environment")
	}
}

func TestBuildContainerRunner_NonContainerEnv(t *testing.T) {
	g := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default", Environment: "dev"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"dev": {Name: "dev", Type: "shell"},
		},
	}
	rf, err := BuildContainerRunner(g, nil, "noop.default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rf != nil {
		t.Fatal("expected nil runner func for non-container environment")
	}
}

func TestBuildContainerRunner_FailClosed(t *testing.T) {
	g := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default", Environment: "dev"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"dev": {Name: "dev", Type: "container"},
		},
	}
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "noop", Name: "default", SourceURL: "https://example.com/noop"},
		},
	}
	_, err := BuildContainerRunner(g, lf, "noop.default")
	if err == nil {
		t.Fatal("expected fail-closed error")
	}
	var fc container.FailClosed
	if !errors.As(err, &fc) {
		t.Fatalf("expected FailClosed, got %T %v", err, err)
	}
	if fc.SourceURL != "https://example.com/noop" {
		t.Errorf("sourceURL = %q, want https://example.com/noop", fc.SourceURL)
	}
}

func TestBuildContainerRunner_Success(t *testing.T) {
	g := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default", Environment: "dev"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"dev": {Name: "dev", Type: "container"},
		},
	}
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "noop", Name: "default",
				SourceURL:      "https://example.com/noop",
				ContainerImage: &lockfile.LockedContainerImage{Ref: "alpine:latest"},
			},
		},
	}
	rf, err := BuildContainerRunner(g, lf, "noop.default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rf == nil {
		t.Fatal("expected non-nil runner func")
	}
}
