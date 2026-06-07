package cli

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPlanGolden(t *testing.T) {
	repoRoot, fixtures := workflowFixtures(t)
	// Some fixtures reference files outside their own directory via file().
	// Allow the whole repo root so those references resolve at compile.
	t.Setenv("CRITERIA_WORKFLOW_ALLOWED_PATHS", repoRoot)
	for _, path := range fixtures {
		path := path
		relPath, _ := filepath.Rel(repoRoot, path)
		name := stripHCLExt(filepath.Base(path)) + "__" + sanitizeFixturePath(relPath)
		t.Run(name, func(t *testing.T) {
			out, err := renderPlanOutput(context.Background(), path, nil)
			if err != nil {
				t.Fatalf("plan output: %v", err)
			}
			assertGoldenFile(t, filepath.Join("testdata", "plan", name+".golden"), []byte(out))
		})
	}
}
