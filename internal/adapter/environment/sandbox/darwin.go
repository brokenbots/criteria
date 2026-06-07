//go:build darwin

package sandbox

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brokenbots/criteria/workflow"
)

// ProfileVersion is the embedded version literal for rendered profiles.
const ProfileVersion = 1

// Profile describes the SBPL rules for a single sandbox-exec session.
type Profile struct {
	AllowFileReads    []string
	AllowFileWrites   []string
	AllowNetworkHosts []string
	AllowExec         []string
	BlockKextLoad     bool
	BlockMachLookup   bool
	DefaultDeny       bool

	// resolveWarnings collects hostname resolution failures so Prepare
	// can decide whether to fail closed in strict mode.
	resolveWarnings []resolveWarn
}

type resolveWarn struct {
	host string
	err  error
}

// Render produces an SBPL-formatted profile string.
func (p *Profile) Render() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("; criteria sandbox profile version %d\n", ProfileVersion))
	b.WriteString("(version 1)\n")
	// Import Apple's base system profile so the dynamic linker can map the
	// dyld shared cache and system frameworks; without this no dynamically
	// linked binary (i.e. every adapter) can start under (deny default).
	// Explicit (deny default) below still governs application-specific
	// file/network access — system.sb only grants the OS read closure.
	b.WriteString("(import \"system.sb\")\n")
	if p.DefaultDeny {
		b.WriteString("(deny default)\n")
	}
	b.WriteString("(allow process-fork)\n")

	if len(p.AllowExec) > 0 {
		writeFileRule(&b, "process-exec", p.AllowExec)
	} else {
		b.WriteString("(deny process-exec)\n")
	}
	if len(p.AllowFileReads) > 0 {
		writeFileRule(&b, "file-read*", p.AllowFileReads)
	}
	if len(p.AllowFileWrites) > 0 {
		writeFileRule(&b, "file-write*", p.AllowFileWrites)
	}

	if len(p.AllowNetworkHosts) > 0 {
		b.WriteString("(allow network-outbound\n")
		for _, host := range p.AllowNetworkHosts {
			b.WriteString(fmt.Sprintf("  (remote ip %q)\n", sandboxRemoteAddr(host)))
		}
		b.WriteString(")\n")
	}

	if p.BlockKextLoad {
		b.WriteString("(deny system-kext-load)\n")
	}
	if p.BlockMachLookup {
		b.WriteString("(deny mach-lookup)\n")
	}

	return b.String()
}

// writeFileRule emits an (allow <op> (literal <path>)...) block with every
// path symlink-resolved so the literals match the kernel's resolved view.
func writeFileRule(b *strings.Builder, op string, paths []string) {
	b.WriteString("(allow " + op + "\n")
	for _, path := range paths {
		fmt.Fprintf(b, "  (literal %q)\n", resolveSandboxPath(path))
	}
	b.WriteString(")\n")
}

// resolveSandboxPath returns p with symlinks evaluated, so profile literals
// match the real paths the kernel checks. macOS firmlinks /tmp -> /private/tmp,
// /var -> /private/var, /etc -> /private/etc; an allow-list entry under any of
// these silently fails to match unless resolved. EvalSymlinks needs the path to
// exist, so for a missing leaf we resolve the longest existing ancestor and
// re-append the remainder.
func resolveSandboxPath(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	// Leaf may not exist yet (e.g. a write target). Resolve the deepest
	// existing ancestor and re-attach the unresolved tail.
	dir, rest := filepath.Dir(p), filepath.Base(p)
	for dir != "/" && dir != "." {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = filepath.Dir(dir)
	}
	return p
}

// sandboxRemoteAddr maps an allowed "host[:port]" entry to a form accepted by
// the macOS sandbox-exec network filter, which only permits "localhost" or "*"
// as the host and requires a port.
//
// Mapping:
//   - loopback hosts (127.0.0.1, ::1, "localhost") -> "localhost:<port>"
//   - any other host                                -> "*:<port>"
//   - missing port                                  -> "<host>:*"
//
// macOS sandbox-exec cannot filter outbound traffic by arbitrary remote IP;
// the `remote ip` filter only understands localhost/*. Non-loopback hosts are
// therefore restricted by port only ("*:<port>"). Callers that need true
// per-IP egress control must run under the Linux sandbox.
func sandboxRemoteAddr(hostPort string) string {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		// No port present: treat the whole string as the host.
		host = hostPort
		port = "*"
	}
	if port == "" {
		port = "*"
	}

	mapped := "*"
	if host == "localhost" {
		mapped = "localhost"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		mapped = "localhost"
	}
	return mapped + ":" + port
}

// Handler prepares Darwin sandbox configuration from a ResolvedPolicy.
type Handler struct{}

// PrepareContext carries the compile-time policy and runtime capabilities
// needed to build a DarwinPrepared configuration.
type PrepareContext struct {
	Policy        *workflow.ResolvedPolicy
	Env           *workflow.EnvironmentNode
	Caps          Capabilities
	AdapterBinary string // resolved adapter plugin path; populated at prepare time
}

// LinuxPrepared is the Darwin-specific prepared sandbox configuration.
// The exported name is preserved for cross-platform compatibility with
// call sites in internal/adapterhost/sessions.go.
type LinuxPrepared struct {
	Mode        string // "strict" or "permissive"
	Profile     Profile
	fallback    bool   // true when sandbox-exec is unavailable
	profilePath string // temp profile file path
}

// DarwinPrepared is an alias for LinuxPrepared on Darwin builds,
// provided for call-site clarity.
type DarwinPrepared = LinuxPrepared

// Prepare converts a ResolvedPolicy into LinuxPrepared.
func (h Handler) Prepare(ctx PrepareContext) (LinuxPrepared, error) {
	if ctx.Policy == nil {
		return LinuxPrepared{}, fmt.Errorf("sandbox policy is nil")
	}
	if ctx.Policy.OS != "" && ctx.Policy.OS != "darwin" {
		return LinuxPrepared{}, fmt.Errorf("sandbox policy OS %q is not supported on this host", ctx.Policy.OS)
	}

	mode := ctx.Policy.PolicyMode
	if mode == "" {
		mode = "permissive"
	}

	if !ctx.Caps.SandboxExec {
		if mode == "strict" {
			return LinuxPrepared{}, fmt.Errorf("sandbox-exec not available but policy_mode=strict")
		}
		slog.Info("sandbox-exec unavailable; falling back to process-hardening primitives", "mode", mode)
		return LinuxPrepared{Mode: mode, fallback: true}, nil
	}

	profile := FromPolicy(*ctx.Policy, ctx.AdapterBinary)
	profile.DefaultDeny = true
	if len(profile.resolveWarnings) > 0 {
		if mode == "strict" {
			var hosts []string
			for _, w := range profile.resolveWarnings {
				hosts = append(hosts, w.host)
			}
			return LinuxPrepared{}, fmt.Errorf("strict mode: policy validation failed for %v", hosts)
		}
		for _, w := range profile.resolveWarnings {
			slog.Warn("sandbox policy validation skipped", "host", w.host, "error", w.err)
		}
	}

	return LinuxPrepared{
		Mode:    mode,
		Profile: profile,
	}, nil
}

// Cleanup removes transient resources created during preparation.
func (prep *LinuxPrepared) Cleanup() error {
	if prep.profilePath != "" {
		_ = os.Remove(prep.profilePath)
		prep.profilePath = ""
	}
	return nil
}

// ApplyToCmd modifies cmd so that it will run inside the sandbox.
func (prep *LinuxPrepared) ApplyToCmd(cmd *exec.Cmd, criteriaBin string) error {
	if prep.fallback {
		return applyFallbackHardening(cmd)
	}

	adapterPath := cmd.Path
	adapterArgs := cmd.Args[1:]

	// The adapter binary and its directory were already allow-listed at
	// prepare time (via PrepareContext.AdapterBinary → FromPolicy).
	// ApplyToCmd must not mutate the receiver's Profile.

	tmpPath, err := writeProfile(&prep.Profile)
	if err != nil {
		return err
	}
	prep.profilePath = tmpPath

	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = append([]string{"sandbox-exec", "-f", tmpPath, adapterPath}, adapterArgs...)

	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = scrubEnv(cmd.Env)
	cmd.Env = sanitizePathEnv(cmd.Env)

	if cmd.Dir == "" {
		cmd.Dir = filepath.Dir(adapterPath)
	}

	return nil
}

// writeProfile serializes profile to a private temp file.
func writeProfile(profile *Profile) (string, error) {
	tmpFile, err := os.CreateTemp(os.TempDir(), "criteria-sb-*.sb")
	if err != nil {
		return "", fmt.Errorf("create sandbox profile: %w", err)
	}
	if _, err := tmpFile.WriteString(profile.Render()); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return "", fmt.Errorf("write sandbox profile: %w", err)
	}
	tmpFile.Close()
	return tmpFile.Name(), nil
}

// applyFallbackHardening applies process-hardening primitives when
// sandbox-exec is unavailable (permissive mode). Strict mode already
// failed closed in Prepare.
func applyFallbackHardening(cmd *exec.Cmd) error {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = scrubEnv(cmd.Env)
	cmd.Env = sanitizePathEnv(cmd.Env)

	if cmd.Dir == "" {
		cmd.Dir = filepath.Dir(cmd.Path)
	}

	// NOTE: rlimits cannot be set directly on Darwin via SysProcAttr;
	// a pre-exec shim would be required. Documented as a known gap
	// for the sandbox-exec-less fallback path.
	return nil
}

// sanitizePathEnv removes relative and empty entries from PATH.
func sanitizePathEnv(env []string) []string {
	for i, e := range env {
		name, val, ok := strings.Cut(e, "=")
		if !ok || name != "PATH" {
			continue
		}
		parts := strings.Split(val, string(os.PathListSeparator))
		var clean []string
		for _, p := range parts {
			if p == "" || !strings.HasPrefix(p, "/") {
				continue
			}
			clean = append(clean, p)
		}
		env[i] = "PATH=" + strings.Join(clean, string(os.PathListSeparator))
		break
	}
	return env
}

// MaybeUseBubblewrap always returns nil on Darwin.
func MaybeUseBubblewrap(_ *LinuxPrepared, _ *workflow.EnvironmentNode, _ string) *exec.Cmd {
	return nil
}

// ApplyEnv is a no-op on Darwin (no shim config to apply).
func ApplyEnv() error { return nil }

// RunIfEnv always returns false on Darwin (no shim).
func RunIfEnv() (bool, error) { return false, nil }
