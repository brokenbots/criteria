//go:build linux

package sandbox

import (
	"testing"
)

func TestProbe(t *testing.T) {
	caps := Probe()
	// Probe should always return a non-nil Capabilities struct.
	if caps == (Capabilities{}) {
		// This is fine on systems with no support at all.
		t.Log("all capabilities false (expected in restricted environments)")
	}
}

func TestCapabilitiesMissing(t *testing.T) {
	c := Capabilities{
		UserNamespaces: true,
		Landlock:       false,
		Seccomp:        true,
		Cgroupv2:       false,
		Bubblewrap:     true,
	}
	missing := c.Missing()
	if len(missing) != 2 {
		t.Fatalf("expected 2 missing, got %d: %v", len(missing), missing)
	}
	found := map[string]bool{}
	for _, m := range missing {
		found[m] = true
	}
	if !found["landlock"] {
		t.Fatal("expected landlock in missing list")
	}
	if !found["cgroupv2"] {
		t.Fatal("expected cgroupv2 in missing list")
	}
}

func TestCapabilitiesMissingNone(t *testing.T) {
	c := Capabilities{
		UserNamespaces: true,
		Landlock:       true,
		Seccomp:        true,
		Cgroupv2:       true,
		Bubblewrap:     true,
	}
	missing := c.Missing()
	if len(missing) != 0 {
		t.Fatalf("expected 0 missing, got %d: %v", len(missing), missing)
	}
}
