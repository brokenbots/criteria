package cli

import (
	"path/filepath"

	"github.com/brokenbots/criteria/workflow"
)

// HCLExtensions lists the file extensions the tool recognises as HCL.
// .chcl is the criteria-native extension; .hcl is accepted for compatibility.
// To change the canonical extension, update this slice.
var HCLExtensions = workflow.HCLExtensions

// hasHCLExtension reports whether name has one of the recognised HCL extensions.
func hasHCLExtension(name string) bool {
	for _, ext := range HCLExtensions {
		if filepath.Ext(name) == ext {
			return true
		}
	}
	return false
}
