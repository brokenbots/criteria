package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// workflowFetcher materialises a remote workflow source into a local directory
// and returns a pin to record in the parent's lockfile.
type workflowFetcher interface {
	Fetch(ctx context.Context, callerDir, source string) (string, *lockfile.LockedWorkflowRef, error)
}

// lockTree recursively locks every workflow directory reachable from rootDir,
// writing each directory's own .criteria.lock.hcl. For remote subworkflow
// sources it records a workflow_ref pin in the parent's lockfile.
func lockTree(ctx context.Context, rootDir string, upgrade, allowUnsigned bool, trustedKeyPaths []string, layout *oci.Layout, resolver lockResolver, fetcher workflowFetcher, out io.Writer) error {
	root, err := buildLockNode(ctx, rootDir, allowUnsigned, trustedKeyPaths, layout, resolver, fetcher, out)
	if err != nil {
		return err
	}

	if err := resolveLockNode(ctx, root, upgrade, out); err != nil {
		return err
	}

	written, err := writeLockNode(root)
	if err != nil {
		return err
	}

	printLockSummary(root, written, out)
	return nil
}

// lockNode represents a single workflow directory during recursive locking.
type lockNode struct {
	state    *lockState
	children []lockChild
	newLF    *lockfile.Lockfile
	// frozen is true for fetched workflow directories whose lockfile is complete
	// and valid. Their pins are used as-is and their lockfile must not be
	// rewritten.
	frozen bool
}

// lockChild records a subworkflow declared in the parent and its resolved
// directory. For remote sources, pin carries the resolved identifier to record
// in the parent's lockfile.
type lockChild struct {
	name      string
	source    string
	resolved  string
	remotePin *lockfile.LockedWorkflowRef
	childNode *lockNode
}

func buildLockNode(ctx context.Context, workflowDir string, allowUnsigned bool, trustedKeyPaths []string, layout *oci.Layout, resolver lockResolver, fetcher workflowFetcher, _ io.Writer) (*lockNode, error) {
	state, err := prepareLockState(workflowDir, allowUnsigned, trustedKeyPaths, layout, resolver)
	if err != nil {
		return nil, err
	}

	node := &lockNode{state: state}

	for _, sw := range state.spec.Subworkflows {
		source := sw.Source
		resolved, pin, err := resolveSubworkflowForLock(ctx, state.workflowDir, source, fetcher)
		if err != nil {
			return nil, fmt.Errorf("subworkflow %q in %q: %w", sw.Name, state.workflowDir, err)
		}

		var child *lockNode
		if pin != nil {
			// Fetched remote workflows with a complete lockfile are frozen: the
			// author's tested pins are used as-is and the lockfile is not rewritten.
			child, err = buildFetchedLockNode(resolved)
			if err != nil {
				return nil, fmt.Errorf("subworkflow %q in %q: %w", sw.Name, state.workflowDir, err)
			}
		} else {
			child, err = buildLockNode(ctx, resolved, allowUnsigned, trustedKeyPaths, layout, resolver, fetcher, nil)
			if err != nil {
				return nil, fmt.Errorf("subworkflow %q in %q: %w", sw.Name, state.workflowDir, err)
			}
		}
		node.children = append(node.children, lockChild{
			name:      sw.Name,
			source:    source,
			resolved:  resolved,
			remotePin: pin,
			childNode: child,
		})
	}

	return node, nil
}

// buildFetchedLockNode builds a frozen lockNode for a fetched workflow whose
// lockfile has already been verified complete. Its existing lockfile is used as
// the resolved lockfile and no re-resolution or rewriting occurs.
func buildFetchedLockNode(resolvedDir string) (*lockNode, error) {
	lf, err := lockfile.ReadFromDir(resolvedDir)
	if err != nil {
		return nil, fmt.Errorf("read fetched lockfile: %w", err)
	}

	spec, diags := workflow.ParseDir(resolvedDir)
	if diags.HasErrors() || spec == nil {
		// Parsing errors surface during normal compile; here we only need the
		// adapter count for the summary.
		spec = &workflow.Spec{}
	}

	return &lockNode{
		frozen: true,
		state: &lockState{
			workflowDir: resolvedDir,
			spec:        spec,
			oldLF:       lf,
			wfAdapters:  collectWorkflowAdapters(spec),
		},
		newLF: lf,
	}, nil
}

func resolveSubworkflowForLock(ctx context.Context, callerDir, source string, fetcher workflowFetcher) (string, *lockfile.LockedWorkflowRef, error) {
	if isRemoteWorkflowSource(source) {
		if fetcher == nil {
			return "", nil, fmt.Errorf("remote workflow sources are not yet supported in this build")
		}
		// If the parent already has a pin for this source, fetch by the resolved
		// identifier so re-locking does not re-resolve a mutable branch or tag, and
		// preserve the existing pin so the tree remains reproducible.
		fetchSource := source
		existingRef := ""
		if parentLF, _ := lockfile.ReadFromDir(callerDir); parentLF != nil {
			for _, wr := range parentLF.WorkflowRefs {
				if wr.Source == source && wr.ResolvedRef != "" {
					existingRef = wr.ResolvedRef
					fetchSource = resolvedWorkflowSource(source, wr.ResolvedRef)
					break
				}
			}
		}
		dir, pin, err := fetcher.Fetch(ctx, callerDir, fetchSource)
		if err != nil {
			return "", nil, err
		}
		// Preserve the original source reference in the lockfile. Carry over the
		// existing resolved identifier when re-locking from a pin, otherwise use the
		// value returned by the fresh fetch.
		pin.Source = source
		if existingRef != "" {
			pin.ResolvedRef = existingRef
		}
		return dir, pin, assertFetchedWorkflowLockfileComplete(dir, source)
	}
	local := &workflow.LocalSubWorkflowResolver{}
	dir, err := local.ResolveSource(ctx, callerDir, source)
	if err != nil {
		return "", nil, err
	}
	return dir, nil, nil
}

// resolvedWorkflowSource replaces the ref portion of a git URL with the resolved
// commit SHA. For simple URLs without a query, it appends ?ref=<resolved>.
func resolvedWorkflowSource(source, resolvedRef string) string {
	if idx := strings.Index(source, "?"); idx != -1 {
		base := source[:idx]
		q, err := url.ParseQuery(source[idx+1:])
		if err == nil {
			q.Set("ref", resolvedRef)
			return base + "?" + q.Encode()
		}
	}
	return source + "?ref=" + resolvedRef
}

func isRemoteWorkflowSource(source string) bool {
	if len(source) < 4 {
		return false
	}
	prefix := source[:4]
	if prefix == "git+" || prefix == "http" || prefix == "ftp:" || prefix == "ftps" {
		return true
	}
	return looksLikeGitURL(source)
}

// assertFetchedWorkflowLockfileComplete validates that a fetched workflow tree
// has a complete lockfile covering all its declared adapters and that each
// pinned version still satisfies the workflow's declared constraint. Absent,
// partial, or stale lockfiles fail with an error naming the fetched workflow and
// the command that updates the pins.
func assertFetchedWorkflowLockfileComplete(dir, source string) error {
	lf, err := lockfile.ReadFromDir(dir)
	if err != nil {
		return fmt.Errorf("read lockfile for fetched workflow %q: %w", source, err)
	}
	if lf == nil {
		return fmt.Errorf("fetched workflow %q has no lockfile; run `criteria adapter lock` against it to generate pins", source)
	}

	spec, diags := workflow.ParseDir(dir)
	if diags.HasErrors() || spec == nil {
		return nil // parsing errors surface during normal compile; here we only care about lockfile completeness
	}

	aliases, _ := collectWorkflowAliases(dir, spec)
	adapters := collectWorkflowAdapters(spec)
	var unpinned []string
	for key, wa := range adapters {
		if wa.Source == "" {
			continue
		}
		entry := findLocked(lf, wa.Type, wa.Name)
		if entry == nil {
			unpinned = append(unpinned, key)
			continue
		}
		if entry.Reference == "" || entry.ResolvedDigest == "" || entry.SourceURL == "" {
			unpinned = append(unpinned, key)
			continue
		}
		if reason := declMatchesPin(wa, entry, aliases); reason != "" {
			unpinned = append(unpinned, fmt.Sprintf("%s (%s)", key, reason))
		}
	}
	if len(unpinned) > 0 {
		return fmt.Errorf("fetched workflow %q has an incomplete lockfile; unpinned adapter(s): %v; run `criteria adapter lock` against it to generate pins", source, unpinned)
	}
	return nil
}

func resolveLockNode(ctx context.Context, node *lockNode, upgrade bool, out io.Writer) error {
	if node.frozen {
		// Fetched workflows with a complete lockfile keep their author's pins.
		return nil
	}

	state := node.state
	newLF := &lockfile.Lockfile{SchemaVersion: 1}
	if state.oldLF != nil {
		newLF.SchemaVersion = state.oldLF.SchemaVersion
	}

	for key, wa := range state.wfAdapters {
		entry, err := resolveOneAdapter(ctx, wa, state.oldLF, state.resolver, state.aliases, upgrade, &state.policy, out)
		if err != nil {
			return fmt.Errorf("adapter %q in %q: %w", key, state.workflowDir, err)
		}
		newLF.Adapters = append(newLF.Adapters, entry)
	}

	for _, child := range node.children {
		if err := resolveLockNode(ctx, child.childNode, upgrade, out); err != nil {
			return err
		}
		if child.remotePin != nil {
			newLF.WorkflowRefs = append(newLF.WorkflowRefs, *child.remotePin)
		}
	}

	node.newLF = newLF
	return nil
}

func writeLockNode(node *lockNode) (int, error) {
	if node.frozen {
		// Fetched workflow lockfiles are intentionally left untouched.
		return 0, nil
	}

	written := 0
	for _, child := range node.children {
		n, err := writeLockNode(child.childNode)
		if err != nil {
			return written, err
		}
		written += n
	}

	changed := lockfileChanged(node.state.oldLF, node.newLF)
	lockPath := filepath.Join(node.state.workflowDir, workflow.LockfileName)
	if changed {
		if err := lockfile.Write(lockPath, node.newLF); err != nil {
			return written, fmt.Errorf("write lockfile %q: %w", lockPath, err)
		}
		written++
	}
	return written, nil
}

// lockfileChanged reports whether the new lockfile differs from the old one.
// It ignores schema-version carry-over and compares functional content.
func lockfileChanged(oldLF, newLF *lockfile.Lockfile) bool {
	if oldLF == nil {
		return len(newLF.Adapters) > 0 || len(newLF.WorkflowRefs) > 0
	}
	changes := lockfile.Diff(oldLF, newLF)
	return len(changes) > 0
}

func printLockSummary(node *lockNode, filesChanged int, out io.Writer) {
	if out == nil {
		out = os.Stderr
	}
	root := node.state.workflowDir
	var report []string
	collectSummary(node, root, "", &report)

	if filesChanged == 0 {
		fmt.Fprintf(out, "lockfile tree up to date, %d workflow(s)\n", len(report))
	} else {
		fmt.Fprintf(out, "locked %d workflow(s), wrote %d lockfile(s)\n", len(report), filesChanged)
	}
	for _, line := range report {
		fmt.Fprintln(out, line)
	}
}

func collectSummary(node *lockNode, rootDir, indent string, report *[]string) {
	rel, err := filepath.Rel(rootDir, node.state.workflowDir)
	if err != nil || rel == "." {
		rel = node.state.workflowDir
	}
	*report = append(*report, fmt.Sprintf("%s%s: %d adapter(s)", indent, rel, len(node.newLF.Adapters)))
	for _, child := range node.children {
		collectSummary(child.childNode, rootDir, indent+"  ", report)
	}
}
