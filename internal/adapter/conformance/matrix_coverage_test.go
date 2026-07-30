package conformance_test

// matrix_coverage_test.go — meta-test that every suite declared in
// matrix.yaml has a corresponding conformance_*.go implementation file.

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

type matrix struct {
	Suites         []string `yaml:"suites"`
	RequiredSuites []string `yaml:"required_suites"`
}

func TestMatrixCoverage(t *testing.T) {
	matrixPath := filepath.Join("..", "..", "..", "internal", "adapter", "conformance", "matrix.yaml")
	data, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("read matrix.yaml: %v", err)
	}

	var m matrix
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse matrix.yaml: %v", err)
	}

	conformanceDir := filepath.Join("..", "..", "..", "internal", "adapter", "conformance")
	entries, err := os.ReadDir(conformanceDir)
	if err != nil {
		t.Fatalf("read conformance dir: %v", err)
	}

	files := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files[e.Name()] = struct{}{}
	}

	for _, suite := range m.Suites {
		wantFile := "conformance_" + suite + ".go"
		if _, ok := files[wantFile]; !ok {
			t.Errorf("matrix.yaml suite %q missing implementation file %q", suite, wantFile)
		}
	}

	suites := make(map[string]struct{}, len(m.Suites))
	for _, suite := range m.Suites {
		suites[suite] = struct{}{}
	}
	for _, suite := range m.RequiredSuites {
		if _, ok := suites[suite]; !ok {
			t.Errorf("matrix.yaml required suite %q must also be listed in suites", suite)
		}
		wantFile := "conformance_" + suite + ".go"
		if _, ok := files[wantFile]; !ok {
			t.Errorf("matrix.yaml required suite %q missing implementation file %q", suite, wantFile)
		}
	}
}
