package remote

import (
	"fmt"
	"os"
	"testing"

	"github.com/brokenbots/criteria/internal/testutil"
)

func TestMain(m *testing.M) {
	if !testutil.CanDialLoopbackTCP() {
		fmt.Println("skipping remote environment suite: loopback TCP connect not permitted in sandbox")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
