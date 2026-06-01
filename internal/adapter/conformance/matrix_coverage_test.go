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
	Suites []string `yaml:"suites"`
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
}
