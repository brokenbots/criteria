package engine

import (
	"slices"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

func graphForEnvWalk(defaultEnv string, adapters map[string]*workflow.AdapterNode, steps map[string]*workflow.StepNode, subworkflows map[string]*workflow.SubworkflowNode) *workflow.FSMGraph {
	return &workflow.FSMGraph{
		DefaultEnvironment: defaultEnv,
		Adapters:           adapters,
		Steps:              steps,
		Subworkflows:       subworkflows,
	}
}

func adapterForEnvWalk(adType, env string) *workflow.AdapterNode {
	return &workflow.AdapterNode{Type: adType, Name: "main", Environment: env}
}

func TestDeclaredAdaptersForEnv(t *testing.T) {
	t.Run("top-level adapter declared on the environment", func(t *testing.T) {
		g := graphForEnvWalk("", map[string]*workflow.AdapterNode{
			"noop.main":    adapterForEnvWalk("noop", "remote.prod"),
			"local.helper": adapterForEnvWalk("sh", "sandbox.dev"),
		}, nil, nil)
		got := declaredAdaptersForEnv("remote.prod", g)
		if !slices.Equal(got, []string{"noop"}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("step environment override pulls the adapter in", func(t *testing.T) {
		g := graphForEnvWalk("", map[string]*workflow.AdapterNode{
			"noop.main": adapterForEnvWalk("noop", "remote.prod"),
			"sh.aux":    adapterForEnvWalk("sh", "sandbox.dev"),
		}, map[string]*workflow.StepNode{
			"kick": {Name: "kick", TargetKind: workflow.StepTargetAdapter, AdapterRef: "sh.aux", Environment: "remote.prod"},
		}, nil)
		got := declaredAdaptersForEnv("remote.prod", g)
		if !slices.Equal(got, []string{"noop", "sh"}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("default environment covers adapters without explicit env", func(t *testing.T) {
		g := graphForEnvWalk("remote.prod", map[string]*workflow.AdapterNode{
			"noop.main": adapterForEnvWalk("noop", ""),
		}, nil, nil)
		got := declaredAdaptersForEnv("remote.prod", g)
		if !slices.Equal(got, []string{"noop"}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("subworkflow body adapters are walked recursively", func(t *testing.T) {
		callee := graphForEnvWalk("", map[string]*workflow.AdapterNode{
			"copilot.a": adapterForEnvWalk("copilot", "remote.prod"),
			"sh.local":  adapterForEnvWalk("sh", "sandbox.dev"),
		}, nil, nil)
		g := graphForEnvWalk("", map[string]*workflow.AdapterNode{
			"noop.main": adapterForEnvWalk("noop", "remote.prod"),
		}, nil, map[string]*workflow.SubworkflowNode{
			"child": {Name: "child", Body: callee},
		})
		got := declaredAdaptersForEnv("remote.prod", g)
		if !slices.Equal(got, []string{"copilot", "noop"}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("no matches yields empty set (legacy escape hatch)", func(t *testing.T) {
		g := graphForEnvWalk("", map[string]*workflow.AdapterNode{
			"sh.local": adapterForEnvWalk("sh", "sandbox.dev"),
		}, nil, nil)
		if got := declaredAdaptersForEnv("remote.prod", g); got != nil {
			t.Fatalf("got %v", got)
		}
		if got := declaredAdaptersForEnv("remote.prod", nil); got != nil {
			t.Fatalf("nil graph: got %v", got)
		}
	})
}
