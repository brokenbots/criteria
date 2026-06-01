package conformance

// conformance_sensitive_output.go — sensitive output taint/redaction contract tests.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testSensitiveOutput(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	if len(info.AdapterInfo.OutputSchema) == 0 {
		t.Skipf("%s: adapter does not declare an output_schema", name)
	}

	hasSensitive := false
	for _, f := range info.AdapterInfo.OutputSchema {
		if f.Sensitive {
			hasSensitive = true
			break
		}
	}
	if !hasSensitive {
		t.Skipf("%s: output_schema has no sensitive fields", name)
	}

	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("sensitive")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	cfg := cloneConfig(opts.StepConfig)
	cfg["emit_sensitive_output"] = "true"
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	res, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}

	// Assert the sensitive value is redacted in the host-facing output.
	// The harness records the adapter's raw output in the sink; if the
	// adapter itself redacts before sending, the sink will not contain
	// the plaintext. We verify that the result outputs do not appear in
	// log chunks in plaintext.
	for key, val := range res.Outputs {
		if info.AdapterInfo.OutputSchema[key].Sensitive && sink.containsText(val) {
			t.Fatalf("sensitive output %q appeared in plaintext in event sink", key)
		}
	}
}
