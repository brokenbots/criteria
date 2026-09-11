package tools_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// govulncheckFilterPath is the shell script under test, relative to the tools
// module root (the working directory for this package's tests).
const govulncheckFilterPath = "govulncheck-filter.sh"

func runFilter(t *testing.T, fakeBinPath, ignoreFilePath string, args ...string) (string, int) {
	t.Helper()

	script, err := filepath.Abs(govulncheckFilterPath)
	if err != nil {
		t.Fatalf("resolving filter script path: %v", err)
	}

	cmdArgs := []string{
		"-govulncheck", fakeBinPath,
		"-ignore", ignoreFilePath,
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command(script, cmdArgs...)
	cmd.Dir = t.TempDir()

	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running filter: %v\noutput:\n%s", err, out)
	}
	return string(out), code
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func writeFakeGovulncheck(t *testing.T, exitCode int, output string) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
` + "printf '%s' " + shellescape(output) + "\nexit " + itoa(exitCode) + "\n"
	path := writeFile(t, dir, "fake-govulncheck", script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod fake govulncheck: %v", err)
	}
	return path
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var sb strings.Builder
	negative := i < 0
	if negative {
		i = -i
	}
	for i > 0 {
		sb.WriteByte(byte('0' + i%10))
		i /= 10
	}
	s := sb.String()
	// reverse
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	if negative {
		return "-" + string(runes)
	}
	return string(runes)
}

func shellescape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func TestGovulncheckFilter_CleanScan(t *testing.T) {
	fake := writeFakeGovulncheck(t, 0, "No vulnerabilities found.\n")
	ignore := writeFile(t, t.TempDir(), "ignore.txt", "")

	out, code := runFilter(t, fake, ignore, "./...")
	if code != 0 {
		t.Fatalf("expected exit code 0 for clean scan, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "No vulnerabilities found") {
		t.Fatalf("expected original output in response, got:\n%s", out)
	}
}

func TestGovulncheckFilter_AllIgnored(t *testing.T) {
	fake := writeFakeGovulncheck(t, 3, "Vulnerability #1: GO-2026-5932\nSome details.\n")
	ignore := writeFile(t, t.TempDir(), "ignore.txt", "# suppressed\nGO-2026-5932\n")

	out, code := runFilter(t, fake, ignore, "./...")
	if code != 0 {
		t.Fatalf("expected exit code 0 when all findings are ignored, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "all affected findings are in the documented ignore list") {
		t.Fatalf("expected ignore confirmation, got:\n%s", out)
	}
}

func TestGovulncheckFilter_UnignoredFinding(t *testing.T) {
	fake := writeFakeGovulncheck(t, 3, "Vulnerability #1: GO-2026-9999\nSome details.\n")
	ignore := writeFile(t, t.TempDir(), "ignore.txt", "# only the old one\nGO-2026-5932\n")

	out, code := runFilter(t, fake, ignore, "./...")
	if code != 3 {
		t.Fatalf("expected exit code 3 for unignored finding, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "Vulnerability #1: GO-2026-9999") {
		t.Fatalf("expected original vulnerability output, got:\n%s", out)
	}
}

func TestGovulncheckFilter_GovulncheckError(t *testing.T) {
	fake := writeFakeGovulncheck(t, 1, "compilation error\n")
	ignore := writeFile(t, t.TempDir(), "ignore.txt", "")

	out, code := runFilter(t, fake, ignore, "./...")
	if code != 1 {
		t.Fatalf("expected exit code 1 for govulncheck error, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "compilation error") {
		t.Fatalf("expected original error output, got:\n%s", out)
	}
}
