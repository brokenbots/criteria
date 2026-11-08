package workflow

// compile_continuation.go — the session-fate (continuation) compile rules of
// ADR-0014 D3: adapter instances declare their session fate at subworkflow
// boundaries (continuation = "reuse" | "fresh"), and adapter instances whose
// re-declaration makes the subworkflow re-entry shape explicit (declared by
// BOTH a parent graph and its callee) must agree on one declared fate. An
// undeclared re-entry pair is a hard validation error — fail-loud replaces
// any default at the boundary; instances with no re-entry keep the
// churn-kill detach-at-body-exit semantics unless they themselves declare fresh.

import (
	"fmt"
	"sort"
)

const (
	continuationReuse = "reuse"
	continuationFresh = "fresh"

	// ContinuationFresh is the enforced fate for an adapter instance whose
	// graphs declare continuation = "fresh": the session is released
	// (close + unregister + tombstone) at the subworkflow body exit, so the
	// next re-entry opens a new conversation.
	ContinuationFresh = continuationFresh
)

// validateContinuation enforces the re-entry graph rule: an adapter instance
// declared by BOTH a parent graph and a subworkflow body it invokes must
// declare its fate (an undeclared re-declaration hides which side owns the
// boundary engagement — the CRI-145 re-entry shape), declare the SAME fate
// (a parent and its callee cannot contradict each other on one instance),
// and the enforced values are "reuse" (today's detach/adopt path) or "fresh"
// (release at the boundary; the callee opens a new conversation at entry).
// Failures are hard validation errors, matching the fail-loud doctrine.
func validateContinuation(g *FSMGraph) []error {
	var errs []error
	for _, swName := range subworkflowOrder(g) {
		sw := g.Subworkflows[swName]
		if sw == nil || sw.Body == nil {
			continue
		}
		for _, instance := range sw.Body.AdapterOrder {
			parent, ok := g.Adapters[instance]
			if !ok || parent == nil {
				continue
			}
			callee := sw.Body.Adapters[instance]
			if parent.Continuation == "" || callee.Continuation == "" {
				errs = append(errs, fmt.Errorf(
					"subworkflow %q re-declares parent adapter %q: both graphs must declare continuation (one of %q or %q); the parent declares %q, the callee declares %q",
					swName, instance, continuationReuse, continuationFresh,
					displayContinuation(parent.Continuation), displayContinuation(callee.Continuation)))
				continue
			}
			if parent.Continuation != callee.Continuation {
				errs = append(errs, fmt.Errorf(
					"subworkflow %q re-declares parent adapter %q with conflicting continuation: the parent declares %q, the callee declares %q; one instance, one fate",
					swName, instance, parent.Continuation, callee.Continuation))
				continue
			}
		}
	}
	return errs
}

// subworkflowOrder lists subworkflow names in sorted (alphabetical) order for
// deterministic diagnostics.
func subworkflowOrder(g *FSMGraph) []string {
	names := make([]string, 0, len(g.Subworkflows))
	for name := range g.Subworkflows {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// displayContinuation renders a declared value for diagnostics; undeclared
// reads as "(undeclared)" so the error names the missing declaration.
func displayContinuation(declared string) string {
	if declared == "" {
		return "(undeclared)"
	}
	return declared
}
