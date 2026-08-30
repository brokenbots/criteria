//go:build linux

package sandbox

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/zclconf/go-cty/cty"
)

// ctyString extracts a string from a cty.Value. Returns the empty string
// if the value is null or unknown.
func ctyString(v cty.Value) string {
	if v.IsNull() || !v.IsKnown() {
		return ""
	}
	if v.Type() == cty.String {
		return v.AsString()
	}
	return ""
}

// parseMemoryLimit parses a human-readable memory limit string such as
// "512M", "1G", "128K" and returns the value in bytes. Returns 0 for
// empty/invalid strings.
func parseMemoryLimit(s string) uint64 {
	if s == "" {
		return 0
	}
	s = strings.TrimSpace(s)
	multiplier := uint64(1)
	switch {
	case strings.HasSuffix(s, "G") || strings.HasSuffix(s, "g"):
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "M") || strings.HasSuffix(s, "m"):
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "K") || strings.HasSuffix(s, "k"):
		multiplier = 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n * multiplier
}

// parseCPULimit parses a CPU limit string such as "1" or "0.5" and
// returns the value as a float64. Returns 0 for empty/invalid strings.
func parseCPULimit(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// parseTimeout parses a duration string. Returns 0 for empty/invalid strings.
func parseTimeout(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return d
}

// parseMaxThreads parses a plain base-10 integer thread limit string.
// An empty string returns 0 and no error (caller should apply the
// default). Any non-numeric or negative value returns an error so the
// caller can surface misconfiguration at prepare time.
func parseMaxThreads(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// resolveMaxThreads parses maxThreads and applies strict/permissive policy.
// It returns 0 when the caller should apply the default. In strict mode an
// invalid value returns an error; in permissive mode it logs a warning and
// falls back to 0.
func resolveMaxThreads(maxThreadsStr, mode string) (uint64, error) {
	maxThreads, err := parseMaxThreads(maxThreadsStr)
	if err != nil {
		if mode == "strict" {
			return 0, fmt.Errorf("max_threads: %w", err)
		}
		slog.Warn("sandbox max_threads invalid, using default", "value", maxThreadsStr, "error", err)
		return 0, nil
	}
	return maxThreads, nil
}

// stringFromObject extracts a string from a nested object field.
func stringFromObject(obj cty.Value, field string) string {
	if obj.IsNull() || !obj.IsKnown() || !obj.Type().IsObjectType() {
		return ""
	}
	if !obj.Type().HasAttribute(field) {
		return ""
	}
	return ctyString(obj.GetAttr(field))
}

// splitHostPort splits a network endpoint string "host:port" into host and
// port. Returns empty strings if the input is malformed.
func splitHostPort(s string) (host, port string) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return "", ""
	}
	host = s[:idx]
	port = s[idx+1:]
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return host, port
}
