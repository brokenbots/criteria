// Wrapper test for the CRI-168 M6.2 depth/cycle conformance suite
// (RunAdapterToolsDepthCycleConformance): locks the tool-call depth
// enforcement and cycle-handling runtime behaviors delivered by CRI-162
// against ADR-0004 §6/§7/§8, so a future refactor cannot silently drift from
// the ruled semantics.
//
// Like the CRI-167 wrapper, this lives in the external test package and runs
// the exported entry point as its own always-on conformance suite: the depth
// and cycle gates are host-side and unconditional, so there is no
// capability-gated skip path (matrix.yaml gates suites on adapter
// capabilities, which would skip a host-side suite in CI forever).
package conformance_test

import (
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

func TestAdapterToolsDepthCycleConformance(t *testing.T) {
	conformance.RunAdapterToolsDepthCycleConformance(t)
}
