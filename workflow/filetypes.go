package workflow

// HCLExtensions lists the file extensions the tool recognises as HCL.
// .chcl is the criteria-native extension; .hcl is accepted for compatibility.
// To change the canonical extension, update this slice.
var HCLExtensions = []string{".chcl", ".hcl"}
