package lockfile

// Merge combines parent and child lockfiles into a new lockfile. Adapters
// defined in child override adapters of the same key ("<type>.<name>") in
// parent, so a subworkflow's own lockfile remains the authority for its own
// adapters. Workflow refs are appended parent-first then child, with exact
// (name, source, resolved_ref, kind) duplicates dropped so re-merging a tree
// that propagates the same pin at multiple levels does not duplicate it.
// A nil argument is treated as an empty lockfile.
func Merge(parent, child *Lockfile) *Lockfile {
	out := &Lockfile{
		SchemaVersion: 1,
	}
	seen := make(map[string]struct{})
	seenRefs := make(map[string]struct{})

	if parent != nil {
		out.SchemaVersion = parent.SchemaVersion
		for i := range parent.Adapters {
			a := &parent.Adapters[i]
			key := a.Type + "." + a.Name
			out.Adapters = append(out.Adapters, *a)
			seen[key] = struct{}{}
		}
		out.WorkflowRefs = appendWorkflowRefs(out.WorkflowRefs, parent.WorkflowRefs, seenRefs)
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
		out.WorkflowRefs = appendWorkflowRefs(out.WorkflowRefs, child.WorkflowRefs, seenRefs)
	}

	return out
}

// appendWorkflowRefs appends refs that are not already present under the same
// exact (name, source, resolved_ref, kind) tuple, keeping first occurrences.
func appendWorkflowRefs(out, refs []LockedWorkflowRef, seen map[string]struct{}) []LockedWorkflowRef {
	for i := range refs {
		w := &refs[i]
		key := w.Name + "\x00" + w.Source + "\x00" + w.ResolvedRef + "\x00" + w.Kind
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, *w)
	}
	return out
}
