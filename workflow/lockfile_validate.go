package workflow

import (
	"sort"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// ValidateLockfileAgainstGraph ensures every adapter referenced by the compiled
// graph has a matching lockfile entry, and every lockfile entry refers to an
// adapter still referenced by the graph.
//
// Returns:
//   - missing: adapters referenced by graph but not in lockfile (compile hint:
//     "run `criteria adapter lock`")
//   - stale:   adapters in lockfile but not referenced (lock command will prune
//     these next run)
func ValidateLockfileAgainstGraph(lf *lockfile.Lockfile, graph *FSMGraph) (missing, stale []string) {
	if lf == nil || graph == nil {
		return nil, nil
	}

	lockSet := make(map[string]struct{}, len(lf.Adapters))
	for i := range lf.Adapters {
		a := &lf.Adapters[i]
		lockSet[a.Type+"."+a.Name] = struct{}{}
	}

	wfSet := make(map[string]struct{}, len(graph.Adapters))
	for k := range graph.Adapters {
		wfSet[k] = struct{}{}
	}

	for k := range wfSet {
		if _, ok := lockSet[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)

	for k := range lockSet {
		if _, ok := wfSet[k]; !ok {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)

	return missing, stale
}
