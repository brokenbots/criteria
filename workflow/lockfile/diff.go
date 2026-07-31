package lockfile

import (
	"reflect"
	"sort"
)

// Diff compares two lockfiles and returns a stable, sorted list of changes.
func Diff(old, next *Lockfile) []Change {
	if old == nil {
		old = &Lockfile{}
	}
	if next == nil {
		next = &Lockfile{}
	}

	oldMap := make(map[string]*LockedAdapter, len(old.Adapters))
	for i := range old.Adapters {
		a := &old.Adapters[i]
		oldMap[adapterKey(a)] = a
	}

	nextMap := make(map[string]*LockedAdapter, len(next.Adapters))
	for i := range next.Adapters {
		a := &next.Adapters[i]
		nextMap[adapterKey(a)] = a
	}

	var changes []Change
	changes = appendWorkflowRefChanges(changes, old.WorkflowRefs, next.WorkflowRefs)

	// Added.
	for k, a := range nextMap {
		if _, ok := oldMap[k]; !ok {
			changes = append(changes, Change{Adapter: k, Kind: Added, After: *a})
		}
	}

	// Removed.
	for k, a := range oldMap {
		if _, ok := nextMap[k]; !ok {
			changes = append(changes, Change{Adapter: k, Kind: Removed, Before: *a})
		}
	}

	// Changed.
	for k, oldA := range oldMap {
		nextA, ok := nextMap[k]
		if !ok {
			continue
		}
		changes = appendAdapterChanges(changes, k, oldA, nextA)
	}

	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Adapter != changes[j].Adapter {
			return changes[i].Adapter < changes[j].Adapter
		}
		return changes[i].Kind < changes[j].Kind
	})

	return changes
}

func appendAdapterChanges(changes []Change, k string, oldA, nextA *LockedAdapter) []Change { //nolint:funlen // WS07: compares 7 distinct change kinds for a single adapter pair
	if oldA.ResolvedDigest != nextA.ResolvedDigest {
		changes = append(changes, Change{
			Adapter: k,
			Kind:    DigestChanged,
			Before:  oldA.ResolvedDigest,
			After:   nextA.ResolvedDigest,
		})
	}

	if !stringSliceEqual(oldA.Platforms, nextA.Platforms) {
		changes = append(changes, Change{
			Adapter: k,
			Kind:    PlatformsChanged,
			Before:  oldA.Platforms,
			After:   nextA.Platforms,
		})
	}

	if !signatureEqual(oldA.Signature, nextA.Signature) {
		changes = append(changes, Change{
			Adapter: k,
			Kind:    SignerChanged,
			Before:  oldA.Signature,
			After:   nextA.Signature,
		})
	}

	if !containerImageEqual(oldA.ContainerImage, nextA.ContainerImage) {
		changes = append(changes, Change{
			Adapter: k,
			Kind:    ContainerImageChanged,
			Before:  oldA.ContainerImage,
			After:   nextA.ContainerImage,
		})
	}

	if !remoteEqual(oldA.Remote, nextA.Remote) {
		changes = append(changes, Change{
			Adapter: k,
			Kind:    RemoteChanged,
			Before:  oldA.Remote,
			After:   nextA.Remote,
		})
	}

	overrideChanged := !stringSliceEqual(oldA.CompatibleEnvironmentsOverride, nextA.CompatibleEnvironmentsOverride) ||
		oldA.OverriddenBy != nextA.OverriddenBy
	if overrideChanged {
		changes = append(changes, Change{
			Adapter: k,
			Kind:    OverrideChanged,
			Before: map[string]any{
				"compatible_environments_override": oldA.CompatibleEnvironmentsOverride,
				"overridden_by":                    oldA.OverriddenBy,
			},
			After: map[string]any{
				"compatible_environments_override": nextA.CompatibleEnvironmentsOverride,
				"overridden_by":                    nextA.OverriddenBy,
			},
		})
	}

	return changes
}

func appendWorkflowRefChanges(changes []Change, oldRefs, nextRefs []LockedWorkflowRef) []Change {
	oldMap := workflowRefMap(oldRefs)
	nextMap := workflowRefMap(nextRefs)

	// Added.
	for k, w := range nextMap {
		if _, ok := oldMap[k]; !ok {
			changes = append(changes, Change{Adapter: "workflow_ref." + k, Kind: WorkflowRefChanged, After: *w})
		}
	}

	// Removed.
	for k, w := range oldMap {
		if _, ok := nextMap[k]; !ok {
			changes = append(changes, Change{Adapter: "workflow_ref." + k, Kind: WorkflowRefChanged, Before: *w})
		}
	}

	// Changed.
	for k, oldW := range oldMap {
		nextW, ok := nextMap[k]
		if !ok {
			continue
		}
		if oldW.ResolvedRef != nextW.ResolvedRef || oldW.Source != nextW.Source || oldW.Kind != nextW.Kind {
			changes = append(changes, Change{
				Adapter: "workflow_ref." + k,
				Kind:    WorkflowRefChanged,
				Before:  *oldW,
				After:   *nextW,
			})
		}
	}
	return changes
}

func adapterKey(a *LockedAdapter) string {
	return a.Type + "." + a.Name
}

func workflowRefMap(refs []LockedWorkflowRef) map[string]*LockedWorkflowRef {
	m := make(map[string]*LockedWorkflowRef, len(refs))
	for i := range refs {
		w := &refs[i]
		m[w.Name] = w
	}
	return m
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func signatureEqual(a, b *LockedSignature) bool {
	return reflect.DeepEqual(a, b)
}

func containerImageEqual(a, b *LockedContainerImage) bool {
	return reflect.DeepEqual(a, b)
}

func remoteEqual(a, b *LockedRemote) bool {
	return reflect.DeepEqual(a, b)
}
