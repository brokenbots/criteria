package conformance_test

import (
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

func TestAdapterToolsCallEdgesConformance(t *testing.T) {
	conformance.RunAdapterToolsCallEdgesConformance(t)
}
