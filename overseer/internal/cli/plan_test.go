package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanGolden(t *testing.T) {
	repoRoot, fixtures := workflowFixtures(t)
	for _, path := range fixtures {
		path := path
		relPath, _ := filepath.Rel(repoRoot, path)
		name := strings.TrimSuffix(filepath.Base(path), ".hcl") + "__" + sanitizeFixturePath(relPath)
		t.Run(name, func(t *testing.T) {
			out, err := renderPlanOutput(context.Background(), path, nil)
			if err != nil {
				t.Fatalf("plan output: %v", err)
			}
			assertGoldenFile(t, filepath.Join("testdata", "plan", name+".golden"), []byte(out))
		})
	}
}
