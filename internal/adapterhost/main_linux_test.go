//go:build linux

package adapterhost

import (
	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

func init() {
	sandboxShimEntry = sandbox.RunIfEnv
}
