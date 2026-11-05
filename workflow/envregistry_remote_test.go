package workflow

// envregistry_remote_test.go — unit tests for the single-sourced remote
// environment attribute grammar (RemoteEnvAcceptedAttrs).

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
)

func mustParseRemoteBody(t *testing.T, src string) hcl.Body {
	t.Helper()
	file, diags := hclparse.NewParser().ParseHCL([]byte(src), "remote.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	return file.Body
}

// TestRemoteEnvGrammar_AcceptsEveryListedAttribute drives the handler the
// default registry installs for compile over the single grammar list: every
// listed attribute must be accepted, so the list and the validator cannot
// drift apart.
func TestRemoteEnvGrammar_AcceptsEveryListedAttribute(t *testing.T) {
	handler := defaultEnvRegistry{}.Lookup("remote")
	if handler == nil {
		t.Fatal("defaultEnvRegistry must install a remote handler")
	}
	if handler.Type() != "remote" {
		t.Fatalf("handler type = %q, want remote", handler.Type())
	}
	for _, name := range RemoteEnvAcceptedAttrs() {
		t.Run(name, func(t *testing.T) {
			body := mustParseRemoteBody(t, name+` = "x"`+"\n")
			if diags := handler.ValidateFields(body); diags.HasErrors() {
				t.Errorf("handler rejected grammar attribute %q: %s", name, diags.Error())
			}
		})
	}
}

// TestValidateRemoteEnvFields_AcceptsFullGrammarBody exercises one body that
// uses every listed attribute plus every tolerated remote block.
func TestValidateRemoteEnvFields_AcceptsFullGrammarBody(t *testing.T) {
	var attrs []string
	for _, name := range RemoteEnvAcceptedAttrs() {
		switch name {
		case "variables", "process", "config":
			attrs = append(attrs, name+" = { key = \"value\" }")
		case "mtls", "insecure", "per_scope_sessions":
			attrs = append(attrs, name+" = true")
		default:
			attrs = append(attrs, name+" = \"x\"")
		}
	}
	src := strings.Join(attrs, "\n") + "\n" + `
mtls {
  server_cert = "/certs/server.pem"
  server_key  = "/certs/key.pem"
  client_ca   = "/certs/ca.pem"
}
network {
  allow = ["*"]
}
filesystem {}
resources {}
`
	if diags := ValidateRemoteEnvFields(mustParseRemoteBody(t, src)); diags.HasErrors() {
		t.Fatalf("full remote grammar body produced diagnostics: %s", diags.Error())
	}
}

func TestValidateRemoteEnvFields_RejectsUnknownAttribute(t *testing.T) {
	diags := ValidateRemoteEnvFields(mustParseRemoteBody(t, `bogus_attr = "x"`+"\n"))
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics for unknown attribute")
	}
	wantSummary := `remote environment: unknown attribute "bogus_attr"`
	if diags[0].Summary != wantSummary {
		t.Errorf("summary = %q, want %q", diags[0].Summary, wantSummary)
	}
	wantDetail := "remote environments accept variables, policy_mode, os, working_directory, listen_address, mtls, accept_token, accept_digest_from, insecure, process, per_scope_sessions, config, tls_handshake_deadline, and identity_handshake_deadline."
	if diags[0].Detail != wantDetail {
		t.Errorf("detail =\n%q\nwant\n%q", diags[0].Detail, wantDetail)
	}
}

func TestValidateRemoteEnvFields_RejectsUnexpectedBlock(t *testing.T) {
	diags := ValidateRemoteEnvFields(mustParseRemoteBody(t, "steps {}\n"))
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics for unexpected block")
	}
	if !strings.Contains(diags.Error(), `Unexpected "steps" block`) {
		t.Errorf("expected unexpected-block error, got: %s", diags.Error())
	}
}
