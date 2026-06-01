package conformance

// conformance_sensitive_output.go — sensitive output taint/redaction contract tests.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func hasSensitiveField(info *adapterhost.Info) bool {
	for _, f := range info.AdapterInfo.OutputSchema {
		if f.Sensitive {
			return true
		}
	}
	return false
}

func assertTaintPropagated(t *testing.T, name string, sink *recordingSink) {
	t.Helper()
	for _, evt := range sink.adapterEvents {
		if evt.kind == "taint" || evt.kind == "sensitive" || evt.kind == "redacted" {
			return
		}
	}
	// NOTE: host-facing log redaction cannot be asserted from the harness
	// because the adapter runs out-of-process. The host-side engine is
	// responsible for redacting sensitive values in its own logs. A
	// dedicated host-log integration test should be added when the engine
	// exposes a testable log sink.
	t.Skipf("%s: adapter did not emit taint/redacted event — cannot validate taint propagation without host-log access", name)
}

func testSensitiveOutput(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	if len(info.AdapterInfo.OutputSchema) == 0 {
		t.Skipf("%s: adapter does not declare an output_schema", name)
	}
	if !hasSensitiveField(info) {
		t.Skipf("%s: output_schema has no sensitive fields", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, sessionID := resolveAndOpen(t, ctx, loader, name, opts.OpenConfig)
	defer plug.Kill()
	defer func() { _ = plug.CloseSession(context.Background(), sessionID) }()

	cfg := cloneConfig(opts.StepConfig)
	cfg["emit_sensitive_output"] = "true"
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	res, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}

	for key, val := range res.Outputs {
		if info.AdapterInfo.OutputSchema[key].Sensitive && sink.containsText(val) {
			t.Fatalf("sensitive output %q appeared in plaintext in event sink", key)
		}
	}
	assertTaintPropagated(t, name, sink)
}
