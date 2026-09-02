package workflow

import (
	"fmt"
	"net"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

// NetworkAllowClass describes the interpretation of a network.allow list from
// an environment's type-specific network policy object.
type NetworkAllowClass int

const (
	// NetworkAllowDeny means the allow list is absent or empty; outbound
	// networking must be denied.
	NetworkAllowDeny NetworkAllowClass = iota
	// NetworkAllowExact means the list contains one or more explicit host:port
	// entries and the backend should enforce only those endpoints.
	NetworkAllowExact
	// NetworkAllowWildcard means the list contains exactly the reserved "*"
	// entry, explicitly opting in to unrestricted outbound networking.
	NetworkAllowWildcard
)

// ClassifyNetworkAllow validates a network.allow string list and returns its
// class. The reserved wildcard "*" is valid only as the sole entry. Any value
// containing glob meta-characters other than exactly "*" is rejected.
func ClassifyNetworkAllow(allow []string) (NetworkAllowClass, error) {
	if len(allow) == 0 {
		return NetworkAllowDeny, nil
	}
	if len(allow) == 1 && allow[0] == "*" {
		return NetworkAllowWildcard, nil
	}
	for _, entry := range allow {
		if entry == "" {
			return 0, fmt.Errorf("network.allow entry must not be empty")
		}
		if entry == "*" {
			return 0, fmt.Errorf("network.allow wildcard %q must be the only entry", entry)
		}
		host, _, err := net.SplitHostPort(entry)
		if err != nil {
			return 0, fmt.Errorf("network.allow entry %q must be host:port or [host]:port: %w", entry, err)
		}
		if hasGlobMeta(host) {
			return 0, fmt.Errorf("network.allow entry %q contains unsupported glob characters; use exact host:port values or the sole wildcard %q", entry, "*")
		}
	}
	return NetworkAllowExact, nil
}

// hasGlobMeta reports whether s contains shell-style glob meta-characters.
func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[]")
}

// NetworkAllowFromObject extracts the allow string list from a type-specific
// network cty object. The second return value reports whether the allow
// attribute is present; callers can distinguish "attribute absent" from
// "attribute present but empty". An error is returned if the allow attribute
// exists but is not a list/tuple/set of known strings.
func NetworkAllowFromObject(netObj cty.Value) (allow []string, hasAllow bool, err error) {
	if netObj.IsNull() || !netObj.IsKnown() || !netObj.Type().IsObjectType() {
		return nil, false, nil
	}
	if !netObj.Type().HasAttribute("allow") {
		return nil, false, nil
	}
	val := netObj.GetAttr("allow")
	if val.IsNull() || !val.IsKnown() {
		return nil, true, nil
	}
	if !val.Type().IsListType() && !val.Type().IsTupleType() && !val.Type().IsSetType() {
		return nil, true, fmt.Errorf("network.allow must be a list of strings, got %s", val.Type().FriendlyName())
	}
	allow, err = ctyStringList(val)
	if err != nil {
		return nil, true, err
	}
	return allow, true, nil
}

// ctyStringList extracts a slice of strings from a cty.Value that is expected
// to be a list/tuple/set of strings. Returns an error if any element is not a
// known, non-null string.
func ctyStringList(v cty.Value) ([]string, error) {
	var out []string
	it := v.ElementIterator()
	for it.Next() {
		_, ev := it.Element()
		if ev.IsNull() || !ev.IsKnown() || ev.Type() != cty.String {
			return nil, fmt.Errorf("network.allow must be a list of strings")
		}
		out = append(out, ev.AsString())
	}
	return out, nil
}
