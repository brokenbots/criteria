package secrets

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
)

// KeychainProvider resolves secrets from the OS keychain.
// On macOS it shells out to the security(1) command.
// On Linux it shells out to secret-tool (libsecret).
// If neither is available it falls back to env/file providers.
type KeychainProvider struct {
	// Fallbacks are consulted when the native keychain is unavailable.
	Fallbacks []Provider
}

func (p *KeychainProvider) Name() string { return "keychain" }

func (p *KeychainProvider) CanResolve(ref OriginRef) bool {
	return ref.Kind == "keychain"
}

func (p *KeychainProvider) Resolve(ctx context.Context, ref OriginRef) (string, error) {
	if ref.Ref == "" {
		return "", fmt.Errorf("keychain provider: empty reference")
	}

	slog.Debug("keychain provider resolving secret", "ref", ref.Ref, "os", runtime.GOOS)

	// Try native keychain first.
	val, err := p.resolveNative(ctx, ref)
	if err == nil {
		slog.Debug("keychain provider resolved via native keychain", "ref", ref.Ref)
		return val, nil
	}

	slog.Debug("keychain provider native resolution failed", "ref", ref.Ref, "error", err)

	// Fall back to other providers if configured.
	for _, fb := range p.Fallbacks {
		if fb.CanResolve(ref) {
			slog.Debug("keychain provider falling back", "fallback", fb.Name(), "ref", ref.Ref)
			return fb.Resolve(ctx, ref)
		}
	}
	return "", fmt.Errorf("keychain provider: %w", err)
}

func (p *KeychainProvider) resolveNative(ctx context.Context, ref OriginRef) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return p.resolveDarwin(ctx, ref)
	case "linux":
		return p.resolveLinux(ctx, ref)
	default:
		return "", fmt.Errorf("keychain provider: unsupported OS %q", runtime.GOOS)
	}
}

// resolveDarwin uses the macOS security command.
// ref.Ref is expected to be "service:account" or just "service" (account defaults to "default").
func (p *KeychainProvider) resolveDarwin(ctx context.Context, ref OriginRef) (string, error) {
	service, account, _ := strings.Cut(ref.Ref, ":")
	if account == "" {
		account = "default"
	}
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", account, "-w")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("keychain provider: security command failed: %w", err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// resolveLinux uses secret-tool from libsecret.
// ref.Ref is expected to be "attribute:value" (e.g. "criteria:api_key").
func (p *KeychainProvider) resolveLinux(ctx context.Context, ref OriginRef) (string, error) {
	attr, value, _ := strings.Cut(ref.Ref, ":")
	if attr == "" || value == "" {
		return "", fmt.Errorf("keychain provider: linux ref must be attribute:value, got %q", ref.Ref)
	}
	cmd := exec.CommandContext(ctx, "secret-tool", "lookup", attr, value)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("keychain provider: secret-tool failed: %w", err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}
