package workflow

// HCLExtensions lists the file extensions the tool recognises as HCL.
// .chcl is the criteria-native extension; .hcl is accepted for compatibility.
// To change the canonical extension, update this slice.
var HCLExtensions = []string{".chcl", ".hcl"}

// LockfileName is the adapter lockfile that lives alongside a workflow's source
// files. It carries an .hcl suffix but is NOT part of the workflow module: it is
// excluded from directory parsing so a locked workflow still loads.
const LockfileName = ".criteria.lock.hcl"
