package peer

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseAdaptersEnv_List(t *testing.T) {
	specs, err := ParseAdaptersEnv("shell, copilot ,shell-x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := []string{}
	for _, s := range specs {
		got = append(got, s.Name)
	}
	want := []string{"shell", "copilot", "shell-x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
}

func TestParseAdaptersEnv_PathForm(t *testing.T) {
	specs, err := ParseAdaptersEnv("shell=/opt/shell, copilot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if specs[0].Name != "shell" || specs[0].Binary != "/opt/shell" {
		t.Errorf("shell spec = %+v", specs[0])
	}
	if specs[1].Name != "copilot" || specs[1].Binary != "" {
		t.Errorf("copilot spec = %+v", specs[1])
	}
}

func TestParseAdaptersEnv_Errors(t *testing.T) {
	for _, raw := range []string{"=path", "a= ", "a,a"} {
		if _, err := ParseAdaptersEnv(raw); err == nil {
			t.Errorf("ParseAdaptersEnv(%q) = nil error, want one", raw)
		}
	}
}

func TestParseAdaptersEnv_Duplicate(t *testing.T) {
	_, err := ParseAdaptersEnv("sh,sh")
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("duplicate not rejected: %v", err)
	}
}

func TestParseAdaptersConfig_DirScan(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"criteria-adapter-shell",
		"criteria-adapter-copilot",
		"criteria-adapter-remote-runner", // skipped
		"criteria-adapter-copilot.bak",   // name would be copilot.bak? derived
		"unrelated",
	} {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	specs, err := ParseAdaptersConfig(getenvFrom(map[string]string{EnvAdaptersDir: dir}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// copilot (dir entry) + copilot.bak (derived name) + shell, sorted by
	// name; remote-runner and unrelated skipped; subdir skipped.
	if len(specs) != 3 || specs[0].Name != "copilot" || specs[1].Name != "copilot.bak" || specs[2].Name != "shell" {
		t.Fatalf("specs = %+v", specs)
	}
	if specs[0].Binary != filepath.Join(dir, "criteria-adapter-copilot") {
		t.Errorf("copilot binary = %q", specs[0].Binary)
	}
}

func TestParseAdaptersConfig_ListWinsAndOverrides(t *testing.T) {
	env := map[string]string{
		EnvAdapters:                        "my-tool, shell",
		EnvAdaptersDir:                     "/nonexistent",
		"CRITERIA_ADAPTER_MY_TOOL_BINARY":  "/opt/my-tool",
		"CRITERIA_ADAPTER_MY_TOOL_VERSION": "9.9.9",
		"CRITERIA_ADAPTER_SHELL_DIGEST":    "sha256:deadbeef",
	}
	specs, err := ParseAdaptersConfig(getenvFrom(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %+v", specs)
	}
	if specs[0].Name != "my-tool" || specs[0].Binary != "/opt/my-tool" || specs[0].Version != "9.9.9" {
		t.Errorf("my-tool spec = %+v, want binary+version overrides", specs[0])
	}
	if specs[1].Name != "shell" || specs[1].Digest != "sha256:deadbeef" {
		t.Errorf("shell spec = %+v, want digest override", specs[1])
	}
}

func TestAdapterEnvName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"shell", "SHELL"},
		{"my-tool", "MY_TOOL"},
		{"copilot.v2", "COPILOT_V2"},
		{"a b!c", "A_B_C"},
	} {
		if got := adapterEnvName(tc.in); got != tc.want {
			t.Errorf("adapterEnvName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveAdapterSpec_BinaryNotFound(t *testing.T) {
	spec := AdapterSpec{Name: "no-such-adapter-xyz"}
	err := ResolveAdapterSpec(&spec)
	if err == nil || !strings.Contains(err.Error(), "no-such-adapter-xyz") {
		t.Errorf("err = %v, want named-binary-not-found", err)
	}
}

func TestResolveAdapterSpec_PathForm(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "criteria-adapter-shell")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := AdapterSpec{Name: "shell", Binary: bin}
	if err := ResolveAdapterSpec(&spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Name != "shell" || spec.Version != DefaultVersion {
		t.Errorf("spec = %+v", spec)
	}
}

func TestResolveAdapterSpec_DigestError(t *testing.T) {
	spec := AdapterSpec{Name: "shell", Binary: "/tmp/whatever-criteria", Digest: "not-a-digest"}
	err := ResolveAdapterSpec(&spec)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("err = %v, want digest parse error", err)
	}
}

func TestScanRemoteScopes_Layout(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(" tok-"+rel+" \n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Root scope: UUID instance dir with token files directly.
	write("e28f9f9a-4b0e-4a7f-9a2b-6a9bc2e3f001/shell.token")
	write("e28f9f9a-4b0e-4a7f-9a2b-6a9bc2e3f001/copilot.token")
	// Workflow scope: label dir with UUID instance subdirs.
	write("worker/33ecdb4f-7f5c-4d55-8f0e-4b0d1b2f0022/shell.token")
	write("worker/93cc2c0e-1b4d-4e77-a2f6-2dd7df4d0033/copilot.token")
	// Record dir skipped at both levels.
	if err := os.MkdirAll(filepath.Join(dir, "current"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "worker", "current"), 0o755); err != nil {
		t.Fatal(err)
	}
	specs, err := ScanRemoteScopes(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	type want struct{ scope, adapter string }
	wants := []want{
		{"/e28f9f9a-4b0e-4a7f-9a2b-6a9bc2e3f001", "copilot"},
		{"/e28f9f9a-4b0e-4a7f-9a2b-6a9bc2e3f001", "shell"},
		{"worker/33ecdb4f-7f5c-4d55-8f0e-4b0d1b2f0022", "shell"},
		{"worker/93cc2c0e-1b4d-4e77-a2f6-2dd7df4d0033", "copilot"},
	}
	if len(specs) != len(wants) {
		t.Fatalf("specs = %+v, want %d entries", specs, len(wants))
	}
	for i, w := range wants {
		if specs[i].Scope != w.scope || specs[i].Adapter != w.adapter {
			t.Errorf("specs[%d] = %+v, want scope=%q adapter=%q", i, specs[i], w.scope, w.adapter)
		}
	}
	if strings.TrimSpace(specs[0].Token) != "tok-e28f9f9a-4b0e-4a7f-9a2b-6a9bc2e3f001/copilot.token" {
		t.Errorf("token = %q, want trimmed file content", specs[0].Token)
	}
}

func TestScanRemoteScopes_Missing(t *testing.T) {
	specs, err := ScanRemoteScopes(filepath.Join(t.TempDir(), "absent"))
	if err != nil || specs != nil {
		t.Errorf("ScanRemoteScopes(absent) = %v, %v", specs, err)
	}
}

func TestChildEnv_ScrubsMultiManifest(t *testing.T) {
	env := []string{
		"PATH=/bin",
		"CRITERIA_ADAPTERS=shell,copilot",
		"CRITERIA_ADAPTERS_DIR=/scan",
		"CRITERIA_ADAPTER_SHELL_BINARY=/opt/shell",
		"CRITERIA_ADAPTER_COPILOT_DIGEST=sha256:abc",
		// legacy passthrough must survive
		"CRITERIA_ADAPTER_NAME=shell",
		"CRITERIA_ADAPTER_BINARY=/bin/x",
		"CRITERIA_ADAPTER_VERSION=1.0.0",
		"CRITERIA_ADAPTER_MANIFEST=/etc/m.yaml",
		"CRITERIA_REMOTE_HOST=10.0.0.1:7000",
	}
	got := ChildEnv(env)
	for _, kv := range got {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "CRITERIA_ADAPTERS", "CRITERIA_ADAPTERS_DIR", "CRITERIA_ADAPTER_SHELL_BINARY",
			"CRITERIA_ADAPTER_COPILOT_DIGEST", "CRITERIA_REMOTE_HOST":
			t.Errorf("%s not scrubbed from child env", name)
		}
	}
	for _, want := range []string{"CRITERIA_ADAPTER_NAME=shell", "CRITERIA_ADAPTER_BINARY=/bin/x",
		"CRITERIA_ADAPTER_VERSION=1.0.0", "CRITERIA_ADAPTER_MANIFEST=/etc/m.yaml", "PATH=/bin"} {
		found := false
		for _, kv := range got {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Errorf("child env lost %q; got %v", want, got)
		}
	}
}
