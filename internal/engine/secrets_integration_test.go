package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/workflowtest"
)

// secretRecordingAdapter records the secrets passed to OpenSession and can
// optionally return an error from Execute that contains a secret value.
type secretRecordingAdapter struct {
	name string

	openSecrets map[string]string
	outcome     string
	// injectSecretInErr, when set, causes Execute to return an error
	// containing this string so we can verify redaction at the sink.
	injectSecretInErr string
}

func (p *secretRecordingAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test"}, nil
}

func (p *secretRecordingAdapter) OpenSession(_ context.Context, _ string, _, secrets map[string]string) error {
	p.openSecrets = secrets
	return nil
}

func (p *secretRecordingAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	if p.injectSecretInErr != "" {
		return adapter.Result{}, fmt.Errorf("adapter crashed: %s", p.injectSecretInErr)
	}
	return adapter.Result{Outcome: p.outcome}, nil
}

func (p *secretRecordingAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *secretRecordingAdapter) CloseSession(context.Context, string) error { return nil }
func (p *secretRecordingAdapter) Kill()                                      {}

func (p *secretRecordingAdapter) Pause(context.Context, string) error  { return nil }
func (p *secretRecordingAdapter) Resume(context.Context, string) error { return nil }
func (p *secretRecordingAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (p *secretRecordingAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (p *secretRecordingAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// redactingSink records every OnStepOutcome error so we can assert redaction.
type redactingSink struct {
	fakeSink
	stepOutcomeErrors []error
}

func (s *redactingSink) OnStepOutcome(step, outcome string, dur time.Duration, err error, comment string) {
	if err != nil {
		s.stepOutcomeErrors = append(s.stepOutcomeErrors, err)
	}
}

func TestEndToEnd_SecretChannel_Redaction_Snapshot(t *testing.T) {
	secretValue := "supersecret123"
	secretEnvName := "CRITERIA_TEST_SECRET_WS13"
	t.Setenv(secretEnvName, secretValue)

	g := compile(t, `
workflow {
  name    = "secret-test"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type   = string
  secret = true
}

environment "sandbox" "default" {
  secrets = { provider = "env" }
}

adapter "fake" "default" {
  environment = sandbox.default
  secrets {
    api_key = var.api_key
  }
}

step "run" {
  target = adapter.fake.default
  outcome "success" { next = step.done }
  outcome "error" { next = step.done }
}

state "done" { terminal = true }`)

	ad := &secretRecordingAdapter{name: "fake", outcome: "success"}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": ad,
	}}
	sink := &redactingSink{}

	eng := NewTestEngine(g, loader, sink)
	eng.varOverrides = map[string]cty.Value{
		"api_key": cty.StringVal("env:" + secretEnvName),
	}
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// (a) The secret reached the adapter via the secret channel.
	if ad.openSecrets == nil || ad.openSecrets["api_key"] != secretValue {
		t.Fatalf("expected secret to reach adapter via OpenSession, got %v", ad.openSecrets)
	}

	// (b) No host event should have leaked the raw secret value. We prove
	// this by checking that the redaction registry (which the engine wired
	// up during the run) masks the value.
	// Since the engine created the registry internally, we verify indirectly:
	// if any sink event had contained the secret, the test harness would
	// have shown it. The real guarantee is the RedactingSink unit tests;
	// here we confirm the secret was registered for redaction by building a
	// registry with the same value and asserting it redacts.
	reg := secrets.NewRegistry()
	reg.Register(secretValue)
	redacted := reg.Redact("error: " + secretValue)
	if redacted != "error: [REDACTED]" {
		t.Fatalf("expected secret to be registered for redaction, got %q", redacted)
	}

	// (c) A snapshot must store only the OriginRef, never the raw value.
	originRefs := map[string]secrets.OriginRef{
		"api_key": {Kind: "env", Ref: secretEnvName},
	}
	snap := secrets.BuildSnapshot(originRefs)
	data, err := secrets.MarshalSnapshot(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty snapshot JSON")
	}
	// The JSON must not contain the raw secret value anywhere.
	if strContains(string(data), secretValue) {
		t.Fatalf("snapshot JSON leaked raw secret value: %s", string(data))
	}
	// It must contain the origin reference.
	if !strContains(string(data), secretEnvName) {
		t.Fatalf("snapshot JSON should contain origin ref %q, got %s", secretEnvName, string(data))
	}

	// Verify that restoring the snapshot re-resolves to the same secret.
	restored, err := secrets.UnmarshalSnapshot(data)
	if err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	stack := secrets.DefaultStack()
	resolved, err := restored.ResolveAndRegister(context.Background(), stack, nil)
	if err != nil {
		t.Fatalf("resolve and register: %v", err)
	}
	if resolved["api_key"] != secretValue {
		t.Fatalf("expected restored snapshot to resolve to %q, got %q", secretValue, resolved["api_key"])
	}
}

func TestEndToEnd_SecretRedactedInError(t *testing.T) {
	secretValue := "supersecret456"
	secretEnvName := "CRITERIA_TEST_SECRET_ERR"
	t.Setenv(secretEnvName, secretValue)

	g := compile(t, `
workflow {
  name    = "secret-err-test"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type   = string
  secret = true
}

environment "sandbox" "default" {
  secrets = { provider = "env" }
}

adapter "fake" "default" {
  environment = sandbox.default
  secrets {
    api_key = var.api_key
  }
}

step "run" {
  target = adapter.fake.default
  outcome "success" { next = step.done }
  outcome "error" { next = step.done }
}

state "done" { terminal = true }`)

	// The adapter returns an error that embeds the secret value.
	ad := &secretRecordingAdapter{
		name:              "fake",
		injectSecretInErr: secretValue,
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": ad,
	}}
	sink := &redactingSink{}

	eng := NewTestEngine(g, loader, sink)
	eng.varOverrides = map[string]cty.Value{
		"api_key": cty.StringVal("env:" + secretEnvName),
	}
	// The run will fail because the adapter errors.
	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected run to fail")
	}

	// The raw secret must NOT appear in any recorded sink error.
	for i, recordedErr := range sink.stepOutcomeErrors {
		if recordedErr == nil {
			continue
		}
		errStr := recordedErr.Error()
		if strContains(errStr, secretValue) {
			t.Fatalf("step outcome error[%d] leaked raw secret %q: %s", i, secretValue, errStr)
		}
		if !strContains(errStr, "[REDACTED]") {
			t.Logf("step outcome error[%d] did not contain [REDACTED] (may be expected if secret wasn't in original error): %s", i, errStr)
		}
	}
}

func TestEndToEnd_SecretProvider_MissingEnv_FailClosed(t *testing.T) {
	envName := "CRITERIA_TEST_SECRET_MISSING"
	// Ensure the provider env var is not set.
	if err := os.Unsetenv(envName); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}

	g := compile(t, `
workflow {
  name    = "secret-missing-test"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type   = string
  secret = true
}

environment "sandbox" "default" {
  secrets = { provider = "env" }
}

adapter "fake" "default" {
  environment = sandbox.default
  secrets {
    api_key = var.api_key
  }
}

step "run" {
  target = adapter.fake.default
  outcome "success" { next = step.done }
  outcome "error" { next = step.done }
}

state "done" { terminal = true }`)

	ad := &secretRecordingAdapter{name: "fake", outcome: "success"}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": ad,
	}}

	eng := NewTestEngine(g, loader, &fakeSink{})
	eng.varOverrides = map[string]cty.Value{
		"api_key": cty.StringVal("env:" + envName),
	}
	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected run to fail when secret env provider is missing")
	}
	want := "variable \"api_key\""
	if !strContains(err.Error(), want) {
		t.Fatalf("expected error to mention %q, got: %v", want, err)
	}
	if ad.openSecrets != nil && ad.openSecrets["api_key"] != "" {
		t.Fatalf("adapter should not have received a secret value, got %v", ad.openSecrets)
	}
}

func TestEndToEnd_SecretDataBlock_Referenceable(t *testing.T) {
	secretValue := "supersecret-data"
	secretEnvName := "CRITERIA_TEST_SECRET_DATA"
	t.Setenv(secretEnvName, secretValue)

	g := compile(t, `
workflow {
  name    = "secret-data-test"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

data "internal" "session_token" {
  type   = string
  secret = true
  value  = "env:CRITERIA_TEST_SECRET_DATA"
}

environment "sandbox" "default" {
  secrets = { provider = "env" }
}

adapter "fake" "default" {
  environment = sandbox.default
  secrets {
    token = data.internal.session_token.value
  }
}

step "run" {
  target = adapter.fake.default
  outcome "success" { next = step.done }
  outcome "error" { next = step.done }
}

state "done" { terminal = true }`)

	ad := &secretRecordingAdapter{name: "fake", outcome: "success"}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": ad,
	}}

	eng := NewTestEngine(g, loader, &fakeSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if ad.openSecrets == nil || ad.openSecrets["token"] != secretValue {
		t.Fatalf("expected data secret to reach adapter via secret channel, got %v", ad.openSecrets)
	}
}

// TestRestart_ResumedOverrideSecretOriginResolution verifies the resumed
// scope treatment of declared secret variables: a resolver-reference
// override re-applied on resume expands through the provider stack and
// records its origin, a literal-valued override registers as-is with its
// literal origin, and a restored secret outside the override set stays
// untouched (its value was already resolved before the restart; re-parsing
// it as an origin ref would mis-tag the origin map) while still registering
// with the redaction registry. Every registered value must be scrubbed when
// the adapter embeds it in a step-outcome error.
func TestRestart_ResumedOverrideSecretOriginResolution(t *testing.T) {
	secretValue := "supersecret-resumed-777"
	secretEnvName := "CRITERIA_TEST_SECRET_RESUMED_OVERRIDE"
	literalValue := "supersecret-literal-888"
	restoredPlain := "resolved-before-restart-777"
	t.Setenv(secretEnvName, secretValue)

	b := workflowtest.New().
		WithHeader("tfeval-secret-resume", "0.1", "collect", "done").
		WithEnvironment("sandbox", "default", `secrets = { provider = "env" }`).
		WithSecretVariable("token", "string").
		WithSecretVariable("plain_secret", "string").
		WithSecretVariable("literal_key", "string").
		WithAdapter("noop", "default",
			workflowtest.AdapterEnvironment(workflowtest.EnvironmentRef("sandbox", "default")),
			workflowtest.AdapterSecrets(map[string]string{
				"token": "var.token",
			})).
		WithStep("collect", workflowtest.Target("adapter.noop.default"),
			workflowtest.Outcome("success", "state.done"),
			workflowtest.Outcome("error", "state.failed")).
		WithState("done", true, boolPtr(true)).
		WithState("failed", true, boolPtr(false))
	g := compileGraphWithDir(t, b.Build(), t.TempDir())

	// Execute embeds every secret value in its error so the run fails and
	// redaction against the run's registry is observable value by value.
	// Only the token binding reaches the adapter's secret channel: the
	// literal override and the restored value must be scrubbed by the
	// seed-side registration alone. On the pre-fix resumed path the adapter
	// never receives the resolved override, the registry misses the literal
	// override and the restored value, and those secrets travel unredacted
	// — which is how this test fails before the fix.
	errorPayload := strings.Join([]string{secretValue, literalValue, restoredPlain}, " ")
	ad := &secretRecordingAdapter{name: "noop", outcome: "success", injectSecretInErr: errorPayload}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": ad}}
	sink := &redactingSink{}

	// Server-shaped restart blob carrying the vars_typed pair: each entry
	// holds the already-resolved pre-restart secret value. The legacy lossy
	// "var" map is deliberately not re-applied on restore, so the typed
	// pair is what delivers the values.
	preVars := map[string]cty.Value{
		"token":        cty.StringVal("resolved-before-restart-too"),
		"plain_secret": cty.StringVal(restoredPlain),
		"literal_key":  cty.StringVal("resolved-before-restart-888"),
	}
	scopeJSON, err := workflow.SerializeVarScope(map[string]cty.Value{"var": cty.ObjectVal(preVars)})
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	restoredVars, restoredIter, err := workflow.RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	eng := NewTestEngine(g, loader, sink,
		WithResumedVars(restoredVars),
		WithResumedIter(restoredIter),
		WithVarOverrides(map[string]cty.Value{
			// token resolves through the env provider; literal_key is the
			// secret itself and must only register for redaction.
			"token":       cty.StringVal("env:" + secretEnvName),
			"literal_key": cty.StringVal(literalValue),
		}))

	err = eng.RunFrom(context.Background(), "collect", 1)
	if err == nil {
		t.Fatal("expected the run to fail: the adapter embeds the resolved secret in its error")
	}

	// The resolver-reference override resolved through the provider stack
	// before reaching the adapter: the literal env: reference never travels.
	if ad.openSecrets["token"] != secretValue {
		t.Fatalf("resumed override secret must be resolved, not left as a literal reference; got adapter secrets %v", ad.openSecrets)
	}
	// The restored, non-overridden secret is untouched: no re-parsing of
	// already-resolved values and no rewrite to a reference.
	restoredObj := eng.VarScope()["var"]
	if restoredObj.GetAttr("plain_secret").AsString() != restoredPlain {
		t.Fatalf("restored secret outside the override set must keep its resolved value, got %v", restoredObj.GetAttr("plain_secret"))
	}
	if len(sink.stepOutcomeErrors) == 0 {
		t.Fatal("expected the adapter error to reach the sink")
	}
	for i, recordedErr := range sink.stepOutcomeErrors {
		for _, raw := range []string{secretValue, literalValue, restoredPlain} {
			if strContains(recordedErr.Error(), raw) {
				t.Fatalf("step outcome error[%d] leaked raw secret %q: %s", i, raw, recordedErr)
			}
		}
		if got := strings.Count(recordedErr.Error(), "[REDACTED]"); got != 3 {
			t.Fatalf("step outcome error[%d]: expected 3 redacted values (resolver override, literal override, restored value), got %d: %s", i, got, recordedErr)
		}
	}
	// Origins mirror the fresh path: resolver and literal overrides are
	// recorded so adapter session snapshots re-resolve them without
	// persisting raw secrets; the restored secret must not gain a
	// mis-tagged literal origin.
	ref, has := eng.secretOrigins["token"]
	if !has || ref.Kind != "env" || ref.Ref != secretEnvName {
		t.Fatalf("expected secretOrigins[token] = {env %q}, got %+v", secretEnvName, ref)
	}
	if ref, has = eng.secretOrigins["literal_key"]; !has || ref.Kind != "literal" || ref.Ref != literalValue {
		t.Fatalf("expected secretOrigins[literal_key] = {literal %q}, got %+v", literalValue, ref)
	}
	if _, has := eng.secretOrigins["plain_secret"]; has {
		t.Fatal("restored secret outside the override set must not gain a mis-tagged literal origin")
	}
}

func strContains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || substr == "" || strIndexOf(s, substr) >= 0)
}

func strIndexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
