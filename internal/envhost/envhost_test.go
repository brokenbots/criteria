package envhost

import (
	"context"
	"errors"
	"testing"
)

func TestScopeRef_Key(t *testing.T) {
	// The key shape IS the phone-home scope key: "<scope>/<instance>",
	// "/<instance>" for the root scope, and "" for the legacy no-scope
	// shape. Every dispatch verb passes through this rendering.
	for _, tc := range []struct {
		ref  ScopeRef
		want string
	}{
		{ScopeRef{}, ""},
		{ScopeRef{Scope: "alpha"}, "alpha"},
		{ScopeRef{Instance: "uuid-1"}, "/uuid-1"},
		{ScopeRef{Scope: "alpha", Instance: "uuid-1"}, "alpha/uuid-1"},
		{ScopeRef{Scope: "scope-a", Instance: "1"}, "scope-a/1"},
	} {
		if got := tc.ref.Key(); got != tc.want {
			t.Errorf("Key(%+v) = %q, want %q", tc.ref, got, tc.want)
		}
		if tc.ref.String() != tc.want {
			t.Errorf("String(%+v) = %q, want %q", tc.ref, tc.ref.String(), tc.want)
		}
	}
}

func TestParseScopeRef_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		key           string
		wantOK        bool
		want          ScopeRef
		wantRoundTrip bool
	}{
		{key: "", wantOK: true, want: ScopeRef{}, wantRoundTrip: true},
		{key: "/uuid-1", wantOK: true, want: ScopeRef{Instance: "uuid-1"}, wantRoundTrip: true},
		{key: "alpha/uuid-1", wantOK: true, want: ScopeRef{Scope: "alpha", Instance: "uuid-1"}, wantRoundTrip: true},
		{key: "scope-a/1", wantOK: true, want: ScopeRef{Scope: "scope-a", Instance: "1"}, wantRoundTrip: true},
		// Extra slashes ride in the instance half and round-trip.
		{key: "a/b/c", wantOK: true, want: ScopeRef{Scope: "a", Instance: "b/c"}, wantRoundTrip: true},
		// A slashless key is malformed for the parse...
		{key: "plain", wantOK: false},
		// ...and a trailing-slash parse loses the trailing slash, which is
		// why the session layer's folder guards the round trip wholesale.
		{key: "abc/", wantOK: true, want: ScopeRef{Scope: "abc"}, wantRoundTrip: false},
	} {
		got, ok := ParseScopeRef(tc.key)
		if ok != tc.wantOK {
			t.Errorf("ParseScopeRef(%q) ok = %v, want %v", tc.key, ok, tc.wantOK)
			continue
		}
		if !tc.wantOK {
			continue
		}
		if got != tc.want {
			t.Errorf("ParseScopeRef(%q) = %+v, want %+v", tc.key, got, tc.want)
		}
		if round := got.Key(); (round == tc.key) != tc.wantRoundTrip {
			t.Errorf("ParseScopeRef(%q).Key() = %q, round trip = %v, want %v", tc.key, round, round == tc.key, tc.wantRoundTrip)
		}
	}
}

func TestHandles_Get(t *testing.T) {
	var empty Handles
	if _, ok := empty.Get("noop.alpha"); ok {
		t.Error("Get on empty handles settled a handle")
	}
	h := Handles{"noop.alpha": "h1", "noop.beta": "h2"}
	if got, ok := h.Get("noop.alpha"); !ok || got != "h1" {
		t.Errorf("Get(noop.alpha) = %q, %v; want h1, true", got, ok)
	}
	if _, ok := h.Get("noop.missing"); ok {
		t.Error("Get(noop.missing) settled a handle")
	}
}

func TestFateValues(t *testing.T) {
	// The fate vocabulary journals verbatim — the values are stable
	// identifiers by contract.
	if FateRelease != "release" || FateDetach != "detach" {
		t.Errorf("fate values = %q, %q; want release, detach", FateRelease, FateDetach)
	}
}

func TestControlKinds_Distinct(t *testing.T) {
	// Arms are append-only and never repurposed; the set must stay five
	// distinct, nonzero values until the host-owned control plane adds more.
	kinds := []ControlKind{ControlRegisterScope, ControlUnregisterScope, ControlCloseHandle, ControlListenAddress, ControlEnsureFreshHandle}
	seen := make(map[ControlKind]bool)
	for _, k := range kinds {
		if k == 0 {
			t.Error("control kind zero value in the declared set")
		}
		if seen[k] {
			t.Errorf("control kind %d declared twice", k)
		}
		seen[k] = true
	}
}

// spyJournalReader is the thinnest possible JournalReader, for compile-time
// checks against the seam's interface surface.
type spyJournalReader struct{}

func (spyJournalReader) Entries(context.Context) ([]JournalEntry, error) {
	return nil, errors.New("spyJournalReader: not a journal")
}

var _ JournalReader = spyJournalReader{}
