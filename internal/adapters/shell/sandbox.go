package shell

// sandbox.go — environment allowlist, PATH sanitization, working-directory
// confinement, and output-capture bounds for the shell adapter.
//
// The CRITERIA_SHELL_LEGACY=1 opt-out was removed in v0.2.0 as committed in
// the v0.2.0 threat model. Sandbox defaults are now unconditional.
// See docs/security/shell-adapter-threat-model.md §6.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	allowedPathsEnvVar = "CRITERIA_SHELL_ALLOWED_PATHS"

	defaultOutputLimitBytes = 4 * 1024 * 1024  // 4 MiB per stream
	minOutputLimitBytes     = 1024             // 1 KiB
	maxOutputLimitBytes     = 64 * 1024 * 1024 // 64 MiB

	defaultTimeout = 5 * time.Minute
	minTimeout     = time.Second
	maxTimeout     = time.Hour

	// killGrace is how long terminateWithGrace waits between SIGTERM and SIGKILL.
	killGrace = 5 * time.Second
)

// sandboxConfig holds resolved sandbox parameters for one step execution.
type sandboxConfig struct {
	env              []string      // child process environment
	timeout          time.Duration // hard timeout; defaultTimeout unless overridden by step input
	outputLimitBytes int64         // per-stream capture limit
	workingDir       string        // CWD for child; empty = inherit operator CWD
}

// buildSandboxConfig reads the step Input map and returns the resolved sandbox
// configuration. Errors are returned for out-of-range timeout/output_limit_bytes
// values so the adapter can surface them as step failures rather than panics.
func buildSandboxConfig(input map[string]string) (sandboxConfig, error) {
	cfg := sandboxConfig{
		timeout:          defaultTimeout,
		outputLimitBytes: defaultOutputLimitBytes,
	}

	timeout, err := parseTimeoutInput(input["timeout"])
	if err != nil {
		return cfg, err
	}
	cfg.timeout = timeout

	if lim, ok := input["output_limit_bytes"]; ok && lim != "" {
		n, err := parseOutputLimitInput(lim)
		if err != nil {
			return cfg, err
		}
		cfg.outputLimitBytes = n
	}

	if wd, ok := input["working_directory"]; ok && wd != "" {
		cfg.workingDir = wd
	}

	envDecl, err := parseEnvInput(input["env"])
	if err != nil {
		return cfg, fmt.Errorf("shell adapter: invalid env: %w", err)
	}
	cfg.env = buildAllowlistedEnv(envDecl, buildSanitizedPath(input["command_path"]))

	return cfg, nil
}

// parseTimeoutInput parses the optional "timeout" input field.
// Returns (defaultTimeout, nil) when the field is absent or empty.
// Returns (parsed, nil) when the field is present and valid.
func parseTimeoutInput(raw string) (time.Duration, error) {
	if raw == "" {
		return defaultTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("shell adapter: invalid timeout %q: %w", raw, err)
	}
	if d < minTimeout {
		return 0, fmt.Errorf("shell adapter: timeout %v is below minimum %v", d, minTimeout)
	}
	if d > maxTimeout {
		return 0, fmt.Errorf("shell adapter: timeout %v exceeds maximum %v", d, maxTimeout)
	}
	return d, nil
}

// parseOutputLimitInput parses the "output_limit_bytes" input field.
func parseOutputLimitInput(raw string) (int64, error) {
	var n int64
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, fmt.Errorf("shell adapter: invalid output_limit_bytes %q: %w", raw, err)
	}
	if n < minOutputLimitBytes {
		return 0, fmt.Errorf("shell adapter: output_limit_bytes %d is below minimum %d", n, minOutputLimitBytes)
	}
	if n > maxOutputLimitBytes {
		return 0, fmt.Errorf("shell adapter: output_limit_bytes %d exceeds maximum %d", n, maxOutputLimitBytes)
	}
	return n, nil
}

// parseEnvInput decodes the `env` input field. The field must be a
// JSON-encoded map[string]string. An empty or absent value is allowed and
// returns a nil map (no extra vars to pass).
//
// PATH is reserved: callers must use `command_path` to control PATH so the
// sandbox-applied sanitized PATH cannot be silently overridden via `env`.
func parseEnvInput(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("env must be a JSON-encoded map[string]string: %w", err)
	}
	for k := range m {
		if strings.EqualFold(k, "PATH") {
			return nil, fmt.Errorf("env may not set PATH; use input.command_path instead")
		}
	}
	return m, nil
}

// allowedBaseEnvExact is the set of env var names inherited without any
// special logic when sandbox mode is active.
var allowedBaseEnvExact = map[string]bool{
	"HOME":    true,
	"USER":    true,
	"LOGNAME": true,
	"LANG":    true,
	"TZ":      true,
}

// allowedBaseEnvPrefixes lists env var name prefixes that are unconditionally
// passed through (e.g. LC_ALL, LC_CTYPE).
var allowedBaseEnvPrefixes = []string{"LC_"}

// buildAllowlistedEnv constructs the environment slice for the child process.
// Only the vars listed in allowedBaseEnvExact / allowedBaseEnvPrefixes plus
// TERM (if stdin is a TTY) are inherited. PATH is set to sanitizedPath.
// Additional vars declared in envDecl are appended; a value starting with "$"
// means "resolve from parent environment" (e.g. "$GOFLAGS" → os.Getenv("GOFLAGS")).
func buildAllowlistedEnv(envDecl map[string]string, sanitizedPath string) []string {
	env := make([]string, 0, len(allowedBaseEnvExact)+len(envDecl)+2)

	for _, kv := range os.Environ() {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		k := kv[:idx]
		if k == "PATH" {
			continue // PATH is appended below with the sanitized value
		}
		if allowedBaseEnvExact[k] {
			env = append(env, kv)
			continue
		}
		if k == "TERM" && isTerminalStdin() {
			env = append(env, kv)
			continue
		}
		for _, pfx := range allowedBaseEnvPrefixes {
			if strings.HasPrefix(k, pfx) {
				env = append(env, kv)
				break
			}
		}
	}

	env = append(env, "PATH="+sanitizedPath)

	for k, v := range envDecl {
		if strings.HasPrefix(v, "$") {
			env = append(env, k+"="+os.Getenv(v[1:]))
		} else {
			env = append(env, k+"="+v)
		}
	}

	return env
}

// buildSanitizedPath returns the PATH to give the child process.
// When commandPath is non-empty it replaces the inherited PATH entirely.
// Otherwise, the parent PATH is sanitized to remove "." and empty segments.
func buildSanitizedPath(commandPath string) string {
	if commandPath != "" {
		return commandPath
	}
	return sanitizePath(os.Getenv("PATH"))
}

// sanitizePath strips empty segments and any non-absolute segments from a
// PATH-style string. Empty segments and "." silently resolve to the current
// working directory; relative segments like "bin" do the same. All are a
// classic privilege-escalation vector (see T2 in the threat model).
func sanitizePath(path string) string {
	segments := strings.Split(path, string(os.PathListSeparator))
	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		if !filepath.IsAbs(seg) {
			continue
		}
		out = append(out, seg)
	}
	return strings.Join(out, string(os.PathListSeparator))
}

// validateWorkingDirectory checks that wd is confined to the operator's home
// directory or a path explicitly listed in CRITERIA_SHELL_ALLOWED_PATHS.
// Returns nil if the path is permitted.
func validateWorkingDirectory(wd string) error {
	cleaned := filepath.Clean(wd)
	// After Clean, a path that starts with ".." has escaped an implicit base.
	// Absolute paths that contain ".." components are resolved by Clean (e.g.
	// /foo/../bar → /bar), so only genuinely relative-escape paths remain.
	if strings.HasPrefix(cleaned, "..") {
		return fmt.Errorf("shell adapter: working_directory %q contains a path-traversal sequence (..); use an absolute path", wd)
	}
	if isPathAllowed(cleaned) {
		return nil
	}
	return fmt.Errorf(
		"shell adapter: working_directory %q is outside $HOME (%s) and CRITERIA_SHELL_ALLOWED_PATHS; "+
			"add the path to CRITERIA_SHELL_ALLOWED_PATHS to allow it",
		wd, os.Getenv("HOME"),
	)
}

// isPathAllowed reports whether cleaned is under $HOME or any path listed in
// CRITERIA_SHELL_ALLOWED_PATHS (separated using os.PathListSeparator —
// ':' on Unix-like systems and ';' on Windows).
func isPathAllowed(cleaned string) bool {
	if home := filepath.Clean(os.Getenv("HOME")); home != "" && home != "." {
		if cleaned == home || strings.HasPrefix(cleaned, home+string(filepath.Separator)) {
			return true
		}
	}
	allowed := os.Getenv(allowedPathsEnvVar)
	if allowed == "" {
		return false
	}
	for _, p := range strings.Split(allowed, string(os.PathListSeparator)) {
		if p == "" {
			continue
		}
		pc := filepath.Clean(p)
		if cleaned == pc || strings.HasPrefix(cleaned, pc+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// captureState tracks bounded output capture for a single stream.
// Writes beyond limit are counted in dropped but not buffered.
type captureState struct {
	mu      sync.Mutex
	buf     strings.Builder
	dropped int64
	limit   int64
}

func newCaptureState(limit int64) *captureState {
	return &captureState{limit: limit}
}

// write appends data to the capture buffer, truncating at limit.
func (cs *captureState) write(data []byte) {
	if len(data) == 0 {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	remaining := cs.limit - int64(cs.buf.Len())
	if remaining <= 0 {
		cs.dropped += int64(len(data))
		return
	}
	if int64(len(data)) <= remaining {
		cs.buf.Write(data)
	} else {
		cs.buf.Write(data[:remaining])
		cs.dropped += int64(len(data)) - remaining
	}
}

// content returns the captured bytes as a string.
func (cs *captureState) content() string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.buf.String()
}

// droppedBytes returns the number of bytes that were truncated.
func (cs *captureState) droppedBytes() int64 {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.dropped
}
