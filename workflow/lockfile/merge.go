package lockfile

// Merge combines parent and child lockfiles into a new lockfile. Adapters
// defined in child override adapters of the same key ("<type>.<name>") in
// parent, so a subworkflow's own lockfile remains the authority for its own
// adapters. Workflow refs are appended parent-first then child.
// A nil argument is treated as an empty lockfile.
func Merge(parent, child *Lockfile) *Lockfile {
	out := &Lockfile{
		SchemaVersion: 1,
	}
	seen := make(map[string]struct{})

	if parent != nil {
		out.SchemaVersion = parent.SchemaVersion
		for i := range parent.Adapters {
			a := &parent.Adapters[i]
			key := a.Type + "." + a.Name
			out.Adapters = append(out.Adapters, *a)
			seen[key] = struct{}{}
		}
		out.WorkflowRefs = append(out.WorkflowRefs, parent.WorkflowRefs...)
	}

	if child != nil {
		for i := range child.Adapters {
			a := &child.Adapters[i]
			key := a.Type + "." + a.Name
			if _, ok := seen[key]; ok {
				// Override parent's entry for this adapter.
				for j := range out.Adapters {
					existing := &out.Adapters[j]
					if existing.Type+"."+existing.Name == key {
						out.Adapters[j] = *a
						break
					}
				}
				continue
			}
			out.Adapters = append(out.Adapters, *a)
			seen[key] = struct{}{}
		}
		out.WorkflowRefs = append(out.WorkflowRefs, child.WorkflowRefs...)
	}

	return out
}
