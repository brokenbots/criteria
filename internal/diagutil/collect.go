// Package diagutil holds shared diagnostic helpers used by the CLI and langserver.
package diagutil

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/opencontainers/go-digest"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// discoveryLoader is a Loader that can resolve an adapter using a per-call
// binary discovery function. *adapterhost.DefaultLoader implements this.
type discoveryLoader interface {
	adapterhost.Loader
	ResolveWithDiscovery(ctx context.Context, name string, discover adapterhost.DiscoveryFunc, customizer func(name string, cmd *exec.Cmd)) (adapterhost.Handle, error)
}

// CollectSchemas resolves Info() for every adapter referenced in spec and
// returns a schemas map suitable for workflow.Compile, plus warning diagnostics
// for any adapter whose schema could not be resolved. Adapters that cannot be
// resolved (binary not found, network error, Info() failure) are skipped so
// that compile still runs in permissive mode for those adapters — a missing
// binary should not block validation — but each such skip yields a DiagWarning
// so callers can surface it (and promote it to an error via
// --warnings-as-errors). If log is non-nil, failures are also logged at debug.
//
// workflowDir is used to read .criteria.lock.hcl so that adapters pinned by
// source + version can be resolved via their locked digest without requiring a
// separate by-name installation.
func CollectSchemas(ctx context.Context, loader adapterhost.Loader, workflowDir string, spec *workflow.Spec, log *slog.Logger) (map[string]workflow.AdapterInfo, hcl.Diagnostics) {
	if loader == nil || spec == nil {
		return nil, nil
	}

	// Collect unique adapter types from declared adapters and step references.
	seen := map[string]bool{}
	for _, ad := range spec.Adapters {
		if ad.Type != "" {
			seen[ad.Type] = true
		}
	}
	for i := range spec.Steps {
		st := &spec.Steps[i]
		// Steps reference adapters via traversal expressions in the Remain body.
		// Extract the adapter type from the resolved reference.
		if adapterRef, present, _ := workflow.ResolveStepAdapterRef(st.Remain); present && adapterRef != "" {
			parts := strings.Split(adapterRef, ".")
			if len(parts) == 2 && parts[0] != "" {
				seen[parts[0]] = true
			}
		}
	}

	if len(seen) == 0 {
		return nil, nil
	}

	lockStatus := readLockfileStatus(workflowDir)

	schemas := make(map[string]workflow.AdapterInfo)
	var diags hcl.Diagnostics
	for typeName := range seen {
		info, diag, ok := collectAdapterSchema(ctx, loader, typeName, lockStatus, log)
		if diag != nil {
			diags = append(diags, diag)
		}
		if ok {
			schemas[typeName] = info
		}
	}
	return schemas, diags
}

// collectAdapterSchema resolves and returns the declared AdapterInfo for a
// single adapter type. The returned ok flag is true when the adapter binary was
// located and its Info() was read successfully; only then should the caller
// add the returned info to the schemas map. On failure it returns a zero-value
// info, ok=false, and a diagnostic explaining why schema verification was
// skipped.
func collectAdapterSchema(ctx context.Context, loader adapterhost.Loader, typeName string, lockStatus lockfileStatus, log *slog.Logger) (workflow.AdapterInfo, *hcl.Diagnostic, bool) {
	p, resolvedDigest, resolveErr := resolveAdapter(ctx, loader, typeName, lockStatus, log)
	if resolveErr != nil {
		if log != nil {
			log.Debug("schema collection: could not resolve adapter", "adapter_type", typeName, "err", resolveErr)
		}
		return workflow.AdapterInfo{}, unverifiedAdapterDiag(typeName, resolvedDigest, lockStatus, resolveErr), false
	}
	info, err := p.Info(ctx)
	p.Kill()
	if err != nil {
		if log != nil {
			log.Debug("schema collection: Info() failed", "adapter_type", typeName, "err", err)
		}
		return workflow.AdapterInfo{}, unverifiedAdapterDiag(typeName, resolvedDigest, lockStatus, err), false
	}
	if len(info.AdapterInfo.OutputSchema) == 0 {
		return info.AdapterInfo, noOutputSchemaDiag(typeName, resolvedDigest), true
	}
	return info.AdapterInfo, nil, true
}

// resolveAdapter tries to resolve an adapter binary for typeName. If the
// lockfile pins a digest for the type, it uses digest-addressed discovery so
// the exact locked artifact is consulted. It returns the resolved digest (if
// any) so diagnostics can report which artifact was used, and any resolution
// error.
func resolveAdapter(ctx context.Context, loader adapterhost.Loader, typeName string, lockStatus lockfileStatus, log *slog.Logger) (adapterhost.Handle, string, error) {
	if dl, ok := loader.(discoveryLoader); ok {
		if digestStr := lockStatus.digestForType(typeName); digestStr != "" {
			digestEncoded := adapterhost.EncodeDigest(digest.Digest(digestStr))
			p, err := dl.ResolveWithDiscovery(ctx, typeName, func(name string) (string, error) {
				return adapterhost.DiscoverBinaryAt(name, digestEncoded)
			}, nil)
			if err == nil {
				return p, digestStr, nil
			}
			if log != nil {
				log.Debug("schema collection: digest-addressed resolution failed, falling back to by-name", "adapter_type", typeName, "digest", digestStr, "err", err)
			}
		}
	}
	p, err := loader.Resolve(ctx, typeName)
	return p, "", err
}

// lockfileStatus captures the result of reading the workflow's lockfile so
// diagnostics can accurately report what was consulted.
type lockfileStatus struct {
	path       string
	present    bool
	readErr    error
	byType     map[string]string // type -> resolved_digest
	allDigests []string          // all digests seen (for diagnostics)
}

func readLockfileStatus(workflowDir string) lockfileStatus {
	status := lockfileStatus{
		path:   filepath.Join(workflowDir, workflow.LockfileName),
		byType: make(map[string]string),
	}
	lf, err := lockfile.ReadFromDir(workflowDir)
	if err != nil {
		status.readErr = err
		status.present = true // file exists but could not be read
		return status
	}
	if lf == nil {
		return status
	}
	status.present = true
	for i := range lf.Adapters {
		ad := &lf.Adapters[i]
		if ad.Type == "" {
			continue
		}
		if _, ok := status.byType[ad.Type]; !ok && ad.ResolvedDigest != "" {
			status.byType[ad.Type] = ad.ResolvedDigest
		}
		if ad.ResolvedDigest != "" {
			status.allDigests = append(status.allDigests, ad.ResolvedDigest)
		}
	}
	return status
}

func (s lockfileStatus) digestForType(adapterType string) string {
	return s.byType[adapterType]
}

// unverifiedAdapterDiag builds the warning emitted when an adapter's schema
// cannot be resolved at compile time, so the graph is only permissively
// validated for that adapter. The detail names the consulted sources so the
// operator gets actionable guidance.
func unverifiedAdapterDiag(typeName, resolvedDigest string, lockStatus lockfileStatus, err error) *hcl.Diagnostic {
	var consulted []string
	if !lockStatus.present {
		consulted = append(consulted, fmt.Sprintf("lockfile %q (not present)", lockStatus.path))
	} else if lockStatus.readErr != nil {
		consulted = append(consulted, fmt.Sprintf("lockfile %q (read error: %v)", lockStatus.path, lockStatus.readErr))
	} else if digest := lockStatus.digestForType(typeName); digest != "" {
		consulted = append(consulted, fmt.Sprintf("lockfile %q (pin: %s)", lockStatus.path, digest))
	} else {
		consulted = append(consulted, fmt.Sprintf("lockfile %q (no entry for adapter type %q)", lockStatus.path, typeName))
	}

	// Mention the OCI cache / digest-addressed install path when the lockfile
	// pins a digest, even if digest-addressed resolution failed and we fell back
	// to by-name. The operator needs to see the cache path was consulted.
	if pinDigest := lockStatus.digestForType(typeName); pinDigest != "" {
		encoded := adapterhost.EncodeDigest(digest.Digest(pinDigest))
		attempted := ""
		if resolvedDigest == "" {
			attempted = " (attempted, not found)"
		}
		if root, rerr := adapterhost.InstallRoot(); rerr == nil {
			consulted = append(consulted, fmt.Sprintf("OCI cache at %q (digest %s%s)", filepath.Join(root, encoded, adapterhost.AdapterBinaryName(typeName)), pinDigest, attempted))
		} else {
			consulted = append(consulted, fmt.Sprintf("OCI cache (digest %s%s)", pinDigest, attempted))
		}
	}

	if roots := adapterhost.AdaptersRoots(); len(roots) > 0 {
		paths := make([]string, 0, len(roots))
		for _, root := range roots {
			paths = append(paths, filepath.Join(root, adapterhost.AdapterBinaryName(typeName)))
		}
		consulted = append(consulted, fmt.Sprintf("by-name install path(s): %s", strings.Join(paths, ", ")))
	}

	advice := "Ensure the adapter binary is available: cached and extracted to the digest-addressed path, " +
		"or installed by name at the by-name path. Pass --warnings-as-errors to fail fast " +
		"if schema verification is required."
	if !lockStatus.present {
		advice = "Run `criteria adapter lock` to generate a lockfile and cache the adapter binary, " +
			"then validate again. " + advice
	}

	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  fmt.Sprintf("adapter %q schema unverified", typeName),
		Detail: fmt.Sprintf(
			"Could not resolve adapter %q (%v). Its config/input/output schemas are unknown, "+
				"so the workflow graph was validated permissively for this adapter and errors "+
				"may only surface at runtime. Consulted: %s. %s",
			typeName, err, strings.Join(consulted, "; "), advice),
	}
}

// noOutputSchemaDiag builds the distinct warning emitted when an adapter is
// resolved but its manifest declares no output_schema, so step output fields
// cannot be checked.
func noOutputSchemaDiag(typeName, resolvedDigest string) *hcl.Diagnostic {
	var detail string
	if resolvedDigest != "" {
		detail = fmt.Sprintf("Adapter %q resolved (digest %s) but declares no output_schema, so its outputs cannot be checked.", typeName, resolvedDigest)
	} else {
		detail = fmt.Sprintf("Adapter %q resolved but declares no output_schema, so its outputs cannot be checked.", typeName)
	}
	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  fmt.Sprintf("adapter %q resolved but declares no output schema", typeName),
		Detail:   detail,
	}
}
