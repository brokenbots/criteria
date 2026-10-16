package adapterhost

// sessions_bootgate_test.go — the KB-266 boot gate on the run-init surface.
//
// The verify handshake (verifyAdapterInfo, common funnel of VerifyGraph and
// scope-entry Verify) validates the manifest's declared surface. Two
// contract arms are pinned here:
//
//   - An adapter whose handshake declares NO output_schema used to ship
//     silently. Per the KB-264/269 doctrine its absence is the adapter's own
//     declared-permissive act, so boot proceeds (no typed failure) and the
//     instance is recorded + warned — for every adapter layer in the
//     compiled graph, including the callee/subworkflow-declared layer.
//   - An unresolvable manifest (the adapter binary cannot be resolved for
//     the handshake) is a typed boot failure at init, naming the instance
//     and the remediation — the "no shapeless knowledge at boot" arm.
//
// The typed-output-schema control arm pins that a declared (non-empty)
// output_schema is NOT recorded permissive.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// bootGateLoader hands out a configurable handle per Resolve and records the
// types it resolved, so tests can pin both the handshake path and the typed
// boot failure on an unresolvable manifest.
type bootGateLoader struct {
	mu       sync.Mutex
	handle   Handle
	resolveF func(name string) (Handle, error)
	resolved []string
}

func (l *bootGateLoader) Resolve(_ context.Context, name string) (Handle, error) {
	l.mu.Lock()
	l.resolved = append(l.resolved, name)
	l.mu.Unlock()
	if l.resolveF != nil {
		return l.resolveF(name)
	}
	return l.handle, nil
}

func (l *bootGateLoader) Shutdown(context.Context) error { return nil }

func (l *bootGateLoader) resolutionsOf(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, resolved := range l.resolved {
		if resolved == name {
			n++
		}
	}
	return n
}

// typedOutputHandle serves a handshake that declares an output_schema field,
// the KB-264 default output type.
type typedOutputHandle struct {
	cri269Handle
}

func (typedOutputHandle) Info(context.Context) (Info, error) {
	return Info{
		Capabilities: []string{"execute"},
		AdapterInfo: workflow.AdapterInfo{
			OutputSchema: map[string]workflow.ConfigField{
				"value": {CtyType: cty.String},
			},
		},
	}, nil
}

// TestSessions_BootGate_AbsentOutputDeclarationRecordedAndWarned pins the
// declared-permissive arm across ALL adapter layers: the subworkflow-declared
// remote layer (`noop.developer`) and the local layer (`shell.local`) both
// declare no output_schema over a successful handshake, so boot must proceed
// (no typed failure — knowledge is present, the absence is a declaration)
// and every handshaken instance must be recorded as declared-permissive.
func TestSessions_BootGate_AbsentOutputDeclarationRecordedAndWarned(t *testing.T) {
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, false)
	const developer = "noop.developer"
	const local = "shell.local"

	loader := &bootGateLoader{handle: cri269Handle{}}
	m := NewSessionManager(loader)
	m.SetGraph(g)
	m.SetRemoteShim(newVerifyHandleShim(false))

	if err := m.VerifyGraph(ctx, g, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}

	if n := loader.resolutionsOf("shell"); n != 1 {
		t.Fatalf("VerifyGraph must handshake the local layer via the loader, got %d resolution(s)", n)
	}
	for _, instance := range []string{developer, local} {
		if !m.DeclaredPermissiveOutput(instance) {
			t.Fatalf("instance %q declared no output_schema but was not recorded as declared-permissive; the boot gate must record every layer of the graph, not just the root", instance)
		}
	}
}

// TestSessions_BootGate_TypedOutputSchemaNotRecorded pins the control arm: a
// handshake that declares a non-empty output_schema is a typed default
// output surface and must not be recorded declared-permissive.
func TestSessions_BootGate_TypedOutputSchemaNotRecorded(t *testing.T) {
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, false)
	const local = "shell.local"

	loader := &bootGateLoader{handle: typedOutputHandle{}}
	m := NewSessionManager(loader)
	m.SetGraph(g)
	m.SetRemoteShim(newVerifyHandleShim(false))

	if err := m.VerifyGraph(ctx, g, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}

	if m.DeclaredPermissiveOutput(local) {
		t.Fatal("instance with a declared output_schema must not be recorded as declared-permissive")
	}
	// The subworkflow-declared remote layer still serves an undeclared Info
	// and must stay recorded — the gate is keyed per instance, not per type.
	if !m.DeclaredPermissiveOutput("noop.developer") {
		t.Fatal("subworkflow layer with no output_schema should be recorded")
	}
}

// TestSessions_BootGate_ScopeEntryVerifyRecordsPermissive pins the same gate
// on the scope-entry Verify path (the path deferred per-scope adapters take),
// both for the absent and the typed handshake.
func TestSessions_BootGate_ScopeEntryVerifyRecordsPermissive(t *testing.T) {
	ctx := context.Background()

	absent := &bootGateLoader{handle: cri269Handle{}}
	m := NewSessionManager(absent)
	if err := m.Verify(ctx, "shell.local", "shell", "", nil, nil, nil, "", "root", ""); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !m.DeclaredPermissiveOutput("shell.local") {
		t.Fatal("scope-entry verify with no output_schema must record the declared-permissive act")
	}

	typed := &bootGateLoader{handle: typedOutputHandle{}}
	m2 := NewSessionManager(typed)
	if err := m2.Verify(ctx, "shell.local", "shell", "", nil, nil, nil, "", "root", ""); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if m2.DeclaredPermissiveOutput("shell.local") {
		t.Fatal("typed output_schema must not be recorded declared-permissive on the scope-entry verify path")
	}
}

// TestSessions_BootGate_UnresolvableManifestFailsAtInit pins the typed
// boot-failure arm on both verify paths: an adapter whose manifest cannot be
// resolved at all fails the boot at init with an error naming the instance
// and the remediation — never a mid-run session-wait death.
func TestSessions_BootGate_UnresolvableManifestFailsAtInit(t *testing.T) {
	ctx := context.Background()

	g, _ := compileCRI269SessionsGraph(t, false)
	const local = "shell.local"

	loader := &bootGateLoader{resolveF: func(string) (Handle, error) {
		return nil, errors.New("adapter binary not found in any install path")
	}}
	m := NewSessionManager(loader)
	m.SetGraph(g)
	m.SetRemoteShim(newVerifyHandleShim(false))

	err := m.VerifyGraph(ctx, g, nil)
	if err == nil {
		t.Fatal("VerifyGraph with an unresolvable manifest must fail the boot at init")
	}
	if !strings.Contains(err.Error(), local) {
		t.Fatalf("boot failure must name the adapter instance %q; got: %v", local, err)
	}
	if !strings.Contains(err.Error(), "criteria adapter lock") {
		t.Fatalf("boot failure must name the remediation command; got: %v", err)
	}

	// Scope-entry Verify path: same typed failure.
	m2 := NewSessionManager(&bootGateLoader{resolveF: func(string) (Handle, error) {
		return nil, errors.New("adapter binary not found in any install path")
	}})
	if err := m2.Verify(ctx, "shell.local", "shell", "", nil, nil, nil, "", "root", ""); err == nil {
		t.Fatal("Verify with an unresolvable manifest must fail")
	}
	if m2.DeclaredPermissiveOutput("shell.local") {
		t.Fatal("a failed handshake must not record the instance as declared-permissive")
	}
}
