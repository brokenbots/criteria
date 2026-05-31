package langserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHCLDiagsToCompileDiags_WithSubject(t *testing.T) {
	diags := hcl.Diagnostics{
		{
			Severity: hcl.DiagError,
			Summary:  "bad block",
			Detail:   "extra detail",
			Subject: &hcl.Range{
				Filename: "/tmp/test.hcl",
				Start:    hcl.Pos{Line: 3, Column: 5},
				End:      hcl.Pos{Line: 3, Column: 10},
			},
		},
	}

	out := hclDiagsToCompileDiags(diags)
	require.Len(t, out, 1)
	assert.Equal(t, hcl.DiagError, out[0].severity)
	assert.Equal(t, "/tmp/test.hcl", out[0].file)
	assert.Equal(t, 3, out[0].line)
	assert.Equal(t, 5, out[0].col)
	assert.Equal(t, 3, out[0].endLine)
	assert.Equal(t, 10, out[0].endCol)
	assert.Equal(t, "bad block\nextra detail", out[0].message)
}

func TestHCLDiagsToCompileDiags_WithoutSubject(t *testing.T) {
	diags := hcl.Diagnostics{
		{
			Severity: hcl.DiagWarning,
			Summary:  "something off",
			Detail:   "",
		},
	}

	out := hclDiagsToCompileDiags(diags)
	require.Len(t, out, 1)
	assert.Equal(t, hcl.DiagWarning, out[0].severity)
	assert.Equal(t, "", out[0].file)
	assert.Equal(t, 1, out[0].line)
	assert.Equal(t, 1, out[0].col)
	assert.Equal(t, 1, out[0].endLine)
	assert.Equal(t, 1, out[0].endCol)
	assert.Equal(t, "something off", out[0].message)
}

func TestFindFirstHCLFile_FindsHCL(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "other.txt"), []byte(""), 0o644)

	found, err := findFirstHCLFile(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "main.hcl"), found)
}

func TestFindFirstHCLFile_FindsCHCL(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.chcl"), []byte(""), 0o644)

	found, err := findFirstHCLFile(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "main.chcl"), found)
}

func TestFindFirstHCLFile_NoneFound(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "other.txt"), []byte(""), 0o644)

	_, err := findFirstHCLFile(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no .chcl or .hcl files")
}

func TestMakeDirDiag(t *testing.T) {
	d := makeDirDiag("/tmp/wf", hcl.DiagError, "compile failed")
	assert.Equal(t, hcl.DiagError, d.severity)
	assert.Equal(t, "/tmp/wf", d.file)
	assert.Equal(t, 1, d.line)
	assert.Equal(t, 1, d.col)
	assert.Equal(t, 1, d.endLine)
	assert.Equal(t, 1, d.endCol)
	assert.Equal(t, "compile failed", d.message)
}
