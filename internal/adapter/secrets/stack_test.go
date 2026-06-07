package secrets

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow"
)

func TestStack_Resolve(t *testing.T) {
	t.Setenv("STACK_TEST_A", "alpha")
	t.Setenv("STACK_TEST_B", "beta")

	env := EnvProvider{}
	stack := &Stack{providers: []Provider{env}}

	val, err := stack.Resolve(context.Background(), OriginRef{Kind: "env", Ref: "STACK_TEST_A"})
	require.NoError(t, err)
	require.Equal(t, "alpha", val)
}

func TestStack_Resolve_Ordering(t *testing.T) {
	// First provider in the stack wins.
	p1 := &staticProvider{name: "p1", kind: "static", val: "first"}
	p2 := &staticProvider{name: "p2", kind: "static", val: "second"}

	stack := &Stack{providers: []Provider{p1, p2}}
	val, err := stack.Resolve(context.Background(), OriginRef{Kind: "static", Ref: "anything"})
	require.NoError(t, err)
	require.Equal(t, "first", val)
}

func TestStack_Resolve_Fallback(t *testing.T) {
	p1 := &staticProvider{name: "p1", kind: "static", val: ""} // empty means not found
	p2 := &staticProvider{name: "p2", kind: "static", val: "fallback"}

	stack := &Stack{providers: []Provider{p1, p2}}
	val, err := stack.Resolve(context.Background(), OriginRef{Kind: "static", Ref: "anything"})
	require.NoError(t, err)
	require.Equal(t, "fallback", val)
}

func TestStack_Resolve_NotFound(t *testing.T) {
	stack := &Stack{providers: []Provider{}}
	_, err := stack.Resolve(context.Background(), OriginRef{Kind: "env", Ref: "MISSING"})
	require.Error(t, err)
}

func TestStackFromEnvironment(t *testing.T) {
	t.Setenv("STACK_TEST_C", "gamma")

	envNode := &workflow.EnvironmentNode{
		Secrets: &workflow.SecretsPolicy{
			Provider: "env",
			Fallback: []string{},
		},
	}

	stack, err := StackFromEnvironment(envNode)
	require.NoError(t, err)

	val, err := stack.Resolve(context.Background(), OriginRef{Kind: "env", Ref: "STACK_TEST_C"})
	require.NoError(t, err)
	require.Equal(t, "gamma", val)
}

func TestDefaultStack(t *testing.T) {
	stack := DefaultStack()
	require.NotNil(t, stack)

	// Should resolve env refs.
	t.Setenv("DEFAULT_TEST", "ok")
	val, err := stack.Resolve(context.Background(), OriginRef{Kind: "env", Ref: "DEFAULT_TEST"})
	require.NoError(t, err)
	require.Equal(t, "ok", val)
}

// staticProvider is a test provider that resolves any ref of its kind to a fixed value.
type staticProvider struct {
	name string
	kind string
	val  string
}

func (p *staticProvider) Name() string { return p.name }
func (p *staticProvider) CanResolve(ref OriginRef) bool {
	return ref.Kind == p.kind && ref.Ref != "" && p.val != ""
}
func (p *staticProvider) Resolve(_ context.Context, ref OriginRef) (string, error) {
	if ref.Ref == "" {
		return "", fmt.Errorf("not found")
	}
	if p.val == "" {
		return "", fmt.Errorf("not found")
	}
	return p.val, nil
}
