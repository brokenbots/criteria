package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFixtures_V2 validates every .hcl fixture under testdata/v2/* by parsing
// and compiling it. Each subdirectory is treated as a separate module.
func TestFixtures_V2(t *testing.T) {
	fixtureDir := filepath.Join("testdata", "v2")
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("read testdata/v2: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			dir := filepath.Join(fixtureDir, entry.Name())
			spec, diags := ParseDir(dir)
			if diags.HasErrors() {
				t.Fatalf("parse %s: %s", dir, diags.Error())
			}
			_, diags = Compile(spec, nil)
			if diags.HasErrors() {
				t.Fatalf("compile %s: %s", dir, diags.Error())
			}
		})
	}
}
