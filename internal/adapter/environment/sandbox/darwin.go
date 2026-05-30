//go:build darwin

package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

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
	BlockSysctl       bool
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
	if p.DefaultDeny {
		b.WriteString("(deny default)\n")
	}
	b.WriteString("(allow process-fork)\n")

	if len(p.AllowExec) > 0 {
		b.WriteString("(allow process-exec\n")
		for _, path := range p.AllowExec {
			b.WriteString(fmt.Sprintf("  (literal %q)\n", path))
		}
		b.WriteString(")\n")
	} else {
		b.WriteString("(deny process-exec)\n")
	}

	if len(p.AllowFileReads) > 0 {
		b.WriteString("(allow file-read*\n")
		for _, path := range p.AllowFileReads {
			b.WriteString(fmt.Sprintf("  (literal %q)\n", path))
		}
		b.WriteString(")\n")
	}

	if len(p.AllowFileWrites) > 0 {
		b.WriteString("(allow file-write*\n")
		for _, path := range p.AllowFileWrites {
			b.WriteString(fmt.Sprintf("  (literal %q)\n", path))
		}
		b.WriteString(")\n")
	}

	if len(p.AllowNetworkHosts) > 0 {
		b.WriteString("(allow network-outbound\n")
		for _, host := range p.AllowNetworkHosts {
			b.WriteString(fmt.Sprintf("  (remote ip %q)\n", host))
		}
		b.WriteString(")\n")
	}

	if p.BlockSysctl {
		b.WriteString("(deny system-kext-load)\n")
	}
	if p.BlockMachLookup {
		b.WriteString("(deny mach-lookup)\n")
	}

	return b.String()
}

// Handler prepares Darwin sandbox configuration from a ResolvedPolicy.
type Handler struct{}

// PrepareContext carries the compile-time policy and runtime capabilities
// needed to build a DarwinPrepared configuration.
type PrepareContext struct {
	Policy *workflow.ResolvedPolicy
	Env    *workflow.EnvironmentNode
	Caps   Capabilities
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

// RlimitConfig describes a single rlimit. Kept for interface parity
// with Linux; rlimits cannot be applied directly on Darwin via
// SysProcAttr, so they are TODO for the sandbox-exec-less fallback.
type RlimitConfig struct {
	Resource int
	Rlimit   syscall.Rlimit
}

// ShimConfig is unused on Darwin (no in-process shim).
type ShimConfig struct{}

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

	profile := FromPolicy(*ctx.Policy, "")
	profile.DefaultDeny = true
	if len(profile.resolveWarnings) > 0 {
		if mode == "strict" {
			var hosts []string
			for _, w := range profile.resolveWarnings {
				hosts = append(hosts, w.host)
			}
			return LinuxPrepared{}, fmt.Errorf("strict mode: hostname resolution failed for %v", hosts)
		}
		for _, w := range profile.resolveWarnings {
			slog.Warn("sandbox hostname resolution skipped", "host", w.host, "error", w.err)
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

	// Ensure the adapter binary itself is allowed.
	prep.Profile.AllowExec = append(prep.Profile.AllowExec, adapterPath)
	// Also allow reading the binary and its directory for mmap/dlopen.
	prep.Profile.AllowFileReads = append(prep.Profile.AllowFileReads, adapterPath)
	binDir := filepath.Dir(adapterPath)
	if binDir != "" && binDir != "/" {
		prep.Profile.AllowFileReads = append(prep.Profile.AllowFileReads, binDir)
	}

	tmpPath, err := writeProfile(prep.Profile)
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
func writeProfile(profile Profile) (string, error) {
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

	// TODO: rlimits cannot be set directly on Darwin via SysProcAttr;
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
func MaybeUseBubblewrap(_ *LinuxPrepared, _ *workflow.EnvironmentNode) *exec.Cmd { return nil }

// ApplyEnv is a no-op on Darwin (no shim config to apply).
func ApplyEnv() error { return nil }

// RunIfEnv always returns false on Darwin (no shim).
func RunIfEnv() (bool, error) { return false, nil }
