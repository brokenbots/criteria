package workflow

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestClassifyNetworkAllow(t *testing.T) {
	tests := []struct {
		name      string
		allow     []string
		wantClass NetworkAllowClass
		wantErr   bool
	}{
		{
			name:      "absent or empty is deny",
			allow:     nil,
			wantClass: NetworkAllowDeny,
		},
		{
			name:      "empty list is deny",
			allow:     []string{},
			wantClass: NetworkAllowDeny,
		},
		{
			name:      "sole wildcard is unrestricted",
			allow:     []string{"*"},
			wantClass: NetworkAllowWildcard,
		},
		{
			name:    "wildcard combined with host is rejected",
			allow:   []string{"*", "api.linear.app:443"},
			wantErr: true,
		},
		{
			name:    "other glob meta-characters are rejected",
			allow:   []string{"api.*.app:443"},
			wantErr: true,
		},
		{
			name:    "question-mark glob is rejected",
			allow:   []string{"api?.linear.app:443"},
			wantErr: true,
		},
		{
			name:    "bracket glob is rejected",
			allow:   []string{"api[0-9].linear.app:443"},
			wantErr: true,
		},
		{
			name:    "empty entry is rejected",
			allow:   []string{"api.linear.app:443", ""},
			wantErr: true,
		},
		{
			name:      "exact host:port list",
			allow:     []string{"api.linear.app:443", "127.0.0.1:443"},
			wantClass: NetworkAllowExact,
		},
		{
			name:      "exact single host:port",
			allow:     []string{"api.linear.app:443"},
			wantClass: NetworkAllowExact,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			class, err := ClassifyNetworkAllow(tc.allow)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ClassifyNetworkAllow(%v) expected error, got class=%d", tc.allow, class)
				}
				return
			}
			if err != nil {
				t.Fatalf("ClassifyNetworkAllow(%v) unexpected error: %v", tc.allow, err)
			}
			if class != tc.wantClass {
				t.Errorf("ClassifyNetworkAllow(%v) class = %d, want %d", tc.allow, class, tc.wantClass)
			}
		})
	}
}

func TestNetworkAllowFromObject(t *testing.T) {
	tests := []struct {
		name         string
		obj          cty.Value
		wantAllow    []string
		wantHasAllow bool
		wantErr      bool
	}{
		{
			name:         "null object",
			obj:          cty.NullVal(cty.Object(map[string]cty.Type{"allow": cty.List(cty.String)})),
			wantHasAllow: false,
		},
		{
			name:         "object without allow",
			obj:          cty.ObjectVal(map[string]cty.Value{}),
			wantHasAllow: false,
		},
		{
			name:         "allow absent attribute",
			obj:          cty.ObjectVal(map[string]cty.Value{"deny": cty.BoolVal(true)}),
			wantHasAllow: false,
		},
		{
			name:         "empty allow list",
			obj:          cty.ObjectVal(map[string]cty.Value{"allow": cty.ListValEmpty(cty.String)}),
			wantAllow:    []string{},
			wantHasAllow: true,
		},
		{
			name: "wildcard allow list",
			obj: cty.ObjectVal(map[string]cty.Value{
				"allow": cty.ListVal([]cty.Value{cty.StringVal("*")}),
			}),
			wantAllow:    []string{"*"},
			wantHasAllow: true,
		},
		{
			name: "exact allow list",
			obj: cty.ObjectVal(map[string]cty.Value{
				"allow": cty.ListVal([]cty.Value{cty.StringVal("api.linear.app:443")}),
			}),
			wantAllow:    []string{"api.linear.app:443"},
			wantHasAllow: true,
		},
		{
			name: "allow is wrong type",
			obj: cty.ObjectVal(map[string]cty.Value{
				"allow": cty.StringVal("*"),
			}),
			wantHasAllow: true,
			wantErr:      true,
		},
		{
			name: "allow contains non-string",
			obj: cty.ObjectVal(map[string]cty.Value{
				"allow": cty.ListVal([]cty.Value{cty.NumberIntVal(443)}),
			}),
			wantHasAllow: true,
			wantErr:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allow, hasAllow, err := NetworkAllowFromObject(tc.obj)
			if hasAllow != tc.wantHasAllow {
				t.Errorf("hasAllow = %v, want %v", hasAllow, tc.wantHasAllow)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got allow=%v", allow)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !sliceEqual(allow, tc.wantAllow) {
				t.Errorf("allow = %v, want %v", allow, tc.wantAllow)
			}
		})
	}
}

func sliceEqual(a, b []string) bool {
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
