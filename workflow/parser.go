package workflow

import (
	"fmt"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
)

// File is the top-level HCL file structure: one workflow block.
type File struct {
	Workflows []Spec `hcl:"workflow,block"`
}

// ParseFile reads and decodes an HCL file into a Spec. The file must contain
// exactly one `workflow` block.
func ParseFile(path string) (*Spec, hcl.Diagnostics) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "cannot read workflow file",
			Detail:   err.Error(),
		}}
	}
	return Parse(path, src)
}

// Parse decodes HCL source into a Spec.
func Parse(filename string, src []byte) (*Spec, hcl.Diagnostics) {
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCL(src, filename)
	if diags.HasErrors() {
		return nil, diags
	}
	var file File
	if d := gohcl.DecodeBody(f.Body, nil, &file); d.HasErrors() {
		return nil, d
	}
	if len(file.Workflows) != 1 {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "expected exactly one workflow block",
			Detail:   fmt.Sprintf("got %d", len(file.Workflows)),
		}}
	}
	return &file.Workflows[0], nil
}
