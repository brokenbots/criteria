package remote

// handler_grammar_test.go — the runtime-side remote handler must validate
// against the workflow-defined remote grammar so the accepted-attribute list
// keeps exactly one definition.

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/brokenbots/criteria/workflow"
)

func mustParseHandlerBody(t *testing.T, src string) hcl.Body {
	t.Helper()
	file, diags := hclparse.NewParser().ParseHCL([]byte(src), "remote.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	return file.Body
}

// TestRemoteHandlerValidateFields_UsesSingleWorkflowGrammar proves every
// attribute in the workflow-defined grammar passes the runtime-side handler.
// The list drives the loop, so the handler cannot hold a stale copy: adding
// an attribute to workflow.RemoteEnvAcceptedAttrs keeps both paths green, and
// removing one makes the stale side fail.
func TestRemoteHandlerValidateFields_UsesSingleWorkflowGrammar(t *testing.T) {
	handler := &RemoteHandler{}
	for _, name := range workflow.RemoteEnvAcceptedAttrs() {
		t.Run(name, func(t *testing.T) {
			body := mustParseHandlerBody(t, name+` = "x"`+"\n")
			if diags := handler.ValidateFields(body); diags.HasErrors() {
				t.Errorf("handler rejected grammar attribute %q: %s", name, diags.Error())
			}
		})
	}
}

func TestRemoteHandlerValidateFields_RejectsUnknownAttribute(t *testing.T) {
	diags := (&RemoteHandler{}).ValidateFields(mustParseHandlerBody(t, `bogus_attr = "x"`+"\n"))
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics for unknown attribute")
	}
	wantSummary := `remote environment: unknown attribute "bogus_attr"`
	if diags[0].Summary != wantSummary {
		t.Errorf("summary = %q, want %q", diags[0].Summary, wantSummary)
	}
	wantDetail := "remote environments accept variables, policy_mode, os, working_directory, listen_address, mtls, accept_token, accept_digest_from, insecure, process, per_scope_sessions, config, tls_handshake_deadline, and identity_handshake_deadline."
	if diags[0].Detail != wantDetail {
		t.Errorf("detail = %q, want %q", diags[0].Detail, wantDetail)
	}
}
