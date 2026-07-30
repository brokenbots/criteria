package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// fakeLockResolver is a test double for the OCI layout + puller used by the
// lock command. It records which references were pulled and returns
// caller-supplied metadata for each tag.
type fakeLockResolver struct {
	blobs      map[digest.Digest]bool
	tags       []string
	entries    map[string]lockfile.LockedAdapter
	pulledRefs []oci.Reference
	layout     *oci.Layout
}

func newFakeResolver() *fakeLockResolver {
	return &fakeLockResolver{
		blobs:   make(map[digest.Digest]bool),
		entries: make(map[string]lockfile.LockedAdapter),
	}
}

func (f *fakeLockResolver) HasBlob(d digest.Digest) bool {
	return f.blobs[d]
}

func (f *fakeLockResolver) ListTags(ctx context.Context, ref oci.Reference) ([]string, error) {
	return f.tags, nil
}

func (f *fakeLockResolver) PullAndBuild(ctx context.Context, ref oci.Reference, policy *signing.Policy) (digest.Digest, lockfile.LockedAdapter, error) {
	f.pulledRefs = append(f.pulledRefs, ref)
	entry, ok := f.entries[ref.String()]
	if !ok {
		entry = lockfile.LockedAdapter{
			Reference:          ref.String(),
			Version:            ref.Tag,
			SDKProtocolVersion: 2,
		}
	}
	// Use a caller-supplied digest when present so tests can simulate digest
	// drift under a tag. Otherwise derive a deterministic digest from the ref.
	dg := entry.ResolvedDigest
	if dg == "" {
		dg = digest.FromString(ref.String()).String()
	}
	// Ensure the returned entry carries the reference/version of the requested
	// ref even when the caller pre-registered a custom entry.
	entry.Reference = ref.String()
	entry.Version = ref.Tag
	entry.ResolvedDigest = dg
	return digest.Digest(dg), entry, nil
}

func (f *fakeLockResolver) Extract(d digest.Digest, adapterType string) (string, error) {
	if f.layout == nil {
		return "", nil
	}
	return extractOCIAdapterBinary(f.layout, d, adapterType)
}

func (f *fakeLockResolver) withBlob(dg digest.Digest) *fakeLockResolver {
	f.blobs[dg] = true
	return f
}

func (f *fakeLockResolver) withTag(tag string) *fakeLockResolver {
	f.tags = append(f.tags, tag)
	return f
}

func (f *fakeLockResolver) withEntry(ref string, entry *lockfile.LockedAdapter) *fakeLockResolver {
	if entry == nil {
		delete(f.entries, ref)
		return f
	}
	f.entries[ref] = *entry
	return f
}

func (f *fakeLockResolver) withLayout(layout *oci.Layout) *fakeLockResolver {
	f.layout = layout
	return f
}

func TestResolveOneAdapter_BumpsDeclaredVersion(t *testing.T) {
	ctx := context.Background()
	oldDigest := digest.FromString("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1")
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: oldDigest.String(),
	}}}
	wa := &workflowAdapter{
		Type:    "noop",
		Name:    "default",
		Source:  "ghcr.io/brokenbots/criteria-adapter-noop",
		Version: "0.5.2",
	}

	resolver := newFakeResolver().
		withBlob(oldDigest).
		withTag("0.5.2")

	var out bytes.Buffer
	entry, err := resolveOneAdapter(ctx, wa, old, resolver, nil, false, &signing.Policy{}, &out)
	require.NoError(t, err)

	assert.Len(t, resolver.pulledRefs, 1, "plain lock should re-resolve when the declared version changes")
	assert.Equal(t, "0.5.2", resolver.pulledRefs[0].Tag)
	assert.Equal(t, "0.5.2", entry.Version)
	assert.Contains(t, out.String(), "locked noop.default")
	assert.Contains(t, out.String(), "(0.5.2)")
}

func TestResolveOneAdapter_ReusesPinWhenUpToDate(t *testing.T) {
	ctx := context.Background()
	oldDigest := digest.FromString("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1")
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: oldDigest.String(),
	}}}
	wa := &workflowAdapter{
		Type:    "noop",
		Name:    "default",
		Source:  "ghcr.io/brokenbots/criteria-adapter-noop",
		Version: "0.5.1",
	}

	resolver := newFakeResolver().withBlob(oldDigest)

	var out bytes.Buffer
	entry, err := resolveOneAdapter(ctx, wa, old, resolver, nil, false, &signing.Policy{}, &out)
	require.NoError(t, err)

	assert.Len(t, resolver.pulledRefs, 1, "plain lock should re-resolve even when the constraint has not changed")
	assert.Equal(t, "0.5.1", resolver.pulledRefs[0].Tag)
	assert.Equal(t, "0.5.1", entry.Version)
	assert.Equal(t, oldDigest.String(), entry.ResolvedDigest)
}

func TestResolveOneAdapter_UpgradeReResolvesUnchangedConstraint(t *testing.T) {
	ctx := context.Background()
	oldDigest := digest.FromString("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1")
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: oldDigest.String(),
	}}}
	wa := &workflowAdapter{
		Type:    "noop",
		Name:    "default",
		Source:  "ghcr.io/brokenbots/criteria-adapter-noop",
		Version: "^0.5.0",
	}

	resolver := newFakeResolver().
		withBlob(oldDigest).
		withTag("0.5.1").
		withTag("0.5.3")

	var out bytes.Buffer
	entry, err := resolveOneAdapter(ctx, wa, old, resolver, nil, true, &signing.Policy{}, &out)
	require.NoError(t, err)

	assert.Len(t, resolver.pulledRefs, 1, "--upgrade should re-resolve even when the constraint has not changed")
	assert.Equal(t, "0.5.3", resolver.pulledRefs[0].Tag, "--upgrade should pick the highest tag allowed by the constraint")
	assert.Equal(t, "0.5.3", entry.Version)
}

func TestPrintLockDiff_UpToDateMessage(t *testing.T) {
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: "sha256:abc123",
	}}}
	newLF := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{*findLocked(old, "noop", "default")}}

	var out bytes.Buffer
	printLockDiff(old, newLF, &out, 1)

	assert.Contains(t, out.String(), "lockfile up to date")
	assert.Contains(t, out.String(), "1 adapter(s)")
}

func TestPrintLockDiff_SignerChangeIsProminent(t *testing.T) {
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: "sha256:old",
		Signature: &lockfile.LockedSignature{
			Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: "fp-old"},
		},
	}}}
	newLF := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: "sha256:new",
		Signature: &lockfile.LockedSignature{
			Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: "fp-new"},
		},
	}}}

	var out bytes.Buffer
	printLockDiff(old, newLF, &out, 1)
	lines := out.String()

	require.Contains(t, lines, "! noop.default signer changed", "signer changes must be surfaced prominently")
	require.Contains(t, lines, "~ noop.default digest sha256:old -> sha256:new")
	// Ensure the prominent signer line precedes the ordinary digest line.
	signerIdx := bytes.Index([]byte(lines), []byte("! noop.default signer changed"))
	digestIdx := bytes.Index([]byte(lines), []byte("~ noop.default digest"))
	assert.Less(t, signerIdx, digestIdx)
}

func TestAssertLockfileMatchesDeclarations_ReportsVersionMismatch(t *testing.T) {
	lf := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: "sha256:abc",
	}}}
	adapters := map[string]*workflowAdapter{
		"noop.default": {
			Type:    "noop",
			Name:    "default",
			Source:  "ghcr.io/brokenbots/criteria-adapter-noop",
			Version: "0.5.2",
		},
	}

	err := assertLockfileMatchesDeclarations(lf, adapters, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lockfile does not match workflow adapter declarations")
	assert.Contains(t, err.Error(), "noop.default")
	assert.Contains(t, err.Error(), "criteria adapter lock")
}

func TestDeclMatchesPin_ExactVersionMismatch(t *testing.T) {
	oldA := &lockfile.LockedAdapter{
		Reference: "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:   "0.5.1",
	}
	wa := &workflowAdapter{Source: "ghcr.io/brokenbots/criteria-adapter-noop", Version: "0.5.2"}
	assert.Equal(t, "version changed (0.5.1 -> 0.5.2)", declMatchesPin(wa, oldA, nil))
}

func TestDeclMatchesPin_SourceMismatch(t *testing.T) {
	oldA := &lockfile.LockedAdapter{
		Reference: "ghcr.io/oldorg/criteria-adapter-noop:0.5.1",
		Version:   "0.5.1",
	}
	wa := &workflowAdapter{Source: "ghcr.io/neworg/criteria-adapter-noop", Version: "0.5.1"}
	assert.Contains(t, declMatchesPin(wa, oldA, nil), "source changed")
}

func TestDeclMatchesPin_ConstraintSatisfiedByPin(t *testing.T) {
	oldA := &lockfile.LockedAdapter{
		Reference: "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:   "0.5.1",
	}
	wa := &workflowAdapter{Source: "ghcr.io/brokenbots/criteria-adapter-noop", Version: "^0.5.0"}
	assert.Empty(t, declMatchesPin(wa, oldA, nil))
}

func TestDeclMatchesPin_ConstraintNoLongerSatisfied(t *testing.T) {
	oldA := &lockfile.LockedAdapter{
		Reference: "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:   "0.5.1",
	}
	wa := &workflowAdapter{Source: "ghcr.io/brokenbots/criteria-adapter-noop", Version: "^0.5.2"}
	assert.Contains(t, declMatchesPin(wa, oldA, nil), "pinned version 0.5.1 no longer satisfies constraint")
}

func TestPrepareLockState_BuildsPullerForVersionBump(t *testing.T) {
	dir := writeLockFixture(t, "0.5.2", func(lf *lockfile.Lockfile) {
		lf.Adapters[0].Version = "0.5.1"
		lf.Adapters[0].Reference = "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1"
		lf.Adapters[0].ResolvedDigest = digest.FromString("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1").String()
	})

	state, err := prepareLockState(dir, false, true, nil, nil)
	require.NoError(t, err)

	ociResolver, ok := state.resolver.(*ociLockResolver)
	require.True(t, ok, "prepareLockState should build the production OCI resolver")
	require.NotNil(t, ociResolver.puller, "prepareLockState must build a puller when any OCI-sourced adapter exists so a declared version bump can re-resolve")
}

func TestRunLock_VersionBumpReResolvesEndToEnd(t *testing.T) {
	dir := writeLockFixture(t, "0.5.2", func(lf *lockfile.Lockfile) {
		lf.Adapters[0].Version = "0.5.1"
		lf.Adapters[0].Reference = "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1"
		lf.Adapters[0].ResolvedDigest = digest.FromString("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1").String()
	})

	oldDigest := digest.FromString("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1")
	fake := newFakeResolver().
		withBlob(oldDigest).
		withTag("0.5.2")

	var out bytes.Buffer
	err := runLock(context.Background(), dir, false, true, nil, &out, fake)
	require.NoError(t, err)

	require.Len(t, fake.pulledRefs, 1, "plain lock must re-resolve an already-locked adapter when the declared version changes")
	assert.Equal(t, "0.5.2", fake.pulledRefs[0].Tag)
	assert.Contains(t, out.String(), "locked noop.default")
	assert.Contains(t, out.String(), "(0.5.2)")
	assert.Contains(t, out.String(), "~ noop.default digest")

	newLF, err := lockfile.ReadFromDir(dir)
	require.NoError(t, err)
	require.Len(t, newLF.Adapters, 1)
	assert.Equal(t, "0.5.2", newLF.Adapters[0].Version)
	assert.Equal(t, digest.FromString("ghcr.io/brokenbots/criteria-adapter-noop:0.5.2").String(), newLF.Adapters[0].ResolvedDigest)
}

func TestRunLock_UpToDateMessage(t *testing.T) {
	oldDigest := digest.FromString("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1")
	dir := writeLockFixture(t, "0.5.1", func(lf *lockfile.Lockfile) {
		lf.Adapters[0].ResolvedDigest = oldDigest.String()
	})

	fake := newFakeResolver().withBlob(oldDigest)

	var out bytes.Buffer
	err := runLock(context.Background(), dir, false, true, nil, &out, fake)
	require.NoError(t, err)

	assert.Len(t, fake.pulledRefs, 1, "plain lock must re-resolve every OCI adapter even when declarations are unchanged")
	assert.Contains(t, out.String(), "locked noop.default")
	assert.Contains(t, out.String(), "lockfile up to date")
	assert.Contains(t, out.String(), "1 adapter(s)")
}

// writeLockFixture creates a temp workflow directory containing a single OCI
// adapter declared at the given version and a lockfile that the caller can
// customize via editFn.
func writeLockFixture(t *testing.T, declaredVersion string, editFn func(*lockfile.Lockfile)) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	workflowHCL := `workflow {
  name    = "w"
  version = "0.1"
}

adapter "noop" "default" {
  source  = "ghcr.io/brokenbots/criteria-adapter-noop"
  version = "` + declaredVersion + `"
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflow.hcl"), []byte(workflowHCL), 0o600))

	lf := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:               "noop",
		Name:               "default",
		Reference:          "ghcr.io/brokenbots/criteria-adapter-noop:" + declaredVersion,
		Version:            declaredVersion,
		ResolvedDigest:     digest.FromString("ghcr.io/brokenbots/criteria-adapter-noop:" + declaredVersion).String(),
		SDKProtocolVersion: 2,
	}}}
	if editFn != nil {
		editFn(lf)
	}
	require.NoError(t, lockfile.Write(filepath.Join(dir, workflow.LockfileName), lf))
	return dir
}

func TestResolveOneAdapter_ReResolvesUnchangedConstraintAndDetectsMovedDigest(t *testing.T) {
	ctx := context.Background()
	oldDigest := digest.FromString("old-digest")
	newDigest := digest.FromString("new-digest")
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: oldDigest.String(),
	}}}
	wa := &workflowAdapter{
		Type:    "noop",
		Name:    "default",
		Source:  "ghcr.io/brokenbots/criteria-adapter-noop",
		Version: "^0.5.0",
	}

	resolver := newFakeResolver().
		withBlob(oldDigest).
		withBlob(newDigest).
		withTag("0.5.1").
		withTag("0.5.2").
		withEntry("ghcr.io/brokenbots/criteria-adapter-noop:0.5.2", &lockfile.LockedAdapter{
			ResolvedDigest: newDigest.String(),
		})

	var out bytes.Buffer
	entry, err := resolveOneAdapter(ctx, wa, old, resolver, nil, false, &signing.Policy{}, &out)
	require.NoError(t, err)

	assert.Len(t, resolver.pulledRefs, 1, "plain lock must re-resolve even when the constraint is unchanged")
	assert.Equal(t, "0.5.2", resolver.pulledRefs[0].Tag)
	assert.Equal(t, newDigest.String(), entry.ResolvedDigest)
	assert.Contains(t, out.String(), "locked noop.default")
}

func TestResolveOneAdapter_MutableConstraintLocksToDigestAndReResolves(t *testing.T) {
	ctx := context.Background()
	firstDigest := digest.FromString("first-digest")
	secondDigest := digest.FromString("second-digest")

	// First lock: nothing pinned yet. The ^0.5.0 constraint is mutable and must
	// lock to a concrete digest.
	wa := &workflowAdapter{
		Type:    "noop",
		Name:    "default",
		Source:  "ghcr.io/brokenbots/criteria-adapter-noop",
		Version: "^0.5.0",
	}
	resolver := newFakeResolver().
		withBlob(firstDigest).
		withTag("0.5.1").
		withTag("0.5.2").
		withEntry("ghcr.io/brokenbots/criteria-adapter-noop:0.5.2", &lockfile.LockedAdapter{
			ResolvedDigest: firstDigest.String(),
		})

	var out bytes.Buffer
	entry, err := resolveOneAdapter(ctx, wa, nil, resolver, nil, false, &signing.Policy{}, &out)
	require.NoError(t, err)
	assert.Equal(t, "0.5.2", resolver.pulledRefs[0].Tag)
	assert.Equal(t, firstDigest.String(), entry.ResolvedDigest)
	assert.Contains(t, out.String(), "locked noop.default")

	// Second lock with the same mutable constraint: resolver now points to a new
	// digest. The lockfile must update to the new concrete digest.
	resolver2 := newFakeResolver().
		withBlob(firstDigest).
		withBlob(secondDigest).
		withTag("0.5.1").
		withTag("0.5.3").
		withEntry("ghcr.io/brokenbots/criteria-adapter-noop:0.5.3", &lockfile.LockedAdapter{
			ResolvedDigest: secondDigest.String(),
		})
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{*findLockedByEntry(&entry)}}

	out.Reset()
	entry2, err := resolveOneAdapter(ctx, wa, old, resolver2, nil, false, &signing.Policy{}, &out)
	require.NoError(t, err)
	assert.Len(t, resolver2.pulledRefs, 1)
	assert.Equal(t, "0.5.3", resolver2.pulledRefs[0].Tag)
	assert.Equal(t, secondDigest.String(), entry2.ResolvedDigest)
	assert.Contains(t, out.String(), "locked noop.default")
}

func TestResolveOneAdapter_LatestTagLocksToConcreteDigestAndReResolves(t *testing.T) {
	ctx := context.Background()
	firstDigest := digest.FromString("first-digest")
	secondDigest := digest.FromString("second-digest")

	// Empty version ("latest") should resolve to the highest semver tag and
	// record a concrete version+digest in the lockfile.
	wa := &workflowAdapter{
		Type:    "noop",
		Name:    "default",
		Source:  "ghcr.io/brokenbots/criteria-adapter-noop",
		Version: "",
	}
	resolver := newFakeResolver().
		withBlob(firstDigest).
		withTag("1.0.0").
		withEntry("ghcr.io/brokenbots/criteria-adapter-noop:1.0.0", &lockfile.LockedAdapter{
			ResolvedDigest: firstDigest.String(),
		})

	var out bytes.Buffer
	entry, err := resolveOneAdapter(ctx, wa, nil, resolver, nil, false, &signing.Policy{}, &out)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", entry.Version)
	assert.Equal(t, firstDigest.String(), entry.ResolvedDigest)
	assert.Contains(t, out.String(), "locked noop.default")

	resolver2 := newFakeResolver().
		withBlob(firstDigest).
		withBlob(secondDigest).
		withTag("1.0.0").
		withTag("2.0.0").
		withEntry("ghcr.io/brokenbots/criteria-adapter-noop:2.0.0", &lockfile.LockedAdapter{
			ResolvedDigest: secondDigest.String(),
		})
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{*findLockedByEntry(&entry)}}

	out.Reset()
	entry2, err := resolveOneAdapter(ctx, wa, old, resolver2, nil, false, &signing.Policy{}, &out)
	require.NoError(t, err)
	assert.Len(t, resolver2.pulledRefs, 1)
	assert.Equal(t, "2.0.0", resolver2.pulledRefs[0].Tag)
	assert.Equal(t, secondDigest.String(), entry2.ResolvedDigest)
	assert.Equal(t, "2.0.0", entry2.Version)
}

func TestResolveOneAdapter_ImmutablePinDigestDriftIsError(t *testing.T) {
	ctx := context.Background()
	oldDigest := digest.FromString("old-digest")
	newDigest := digest.FromString("new-digest")
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: oldDigest.String(),
	}}}
	wa := &workflowAdapter{
		Type:    "noop",
		Name:    "default",
		Source:  "ghcr.io/brokenbots/criteria-adapter-noop",
		Version: "0.5.1",
	}

	resolver := newFakeResolver().
		withBlob(oldDigest).
		withBlob(newDigest).
		withEntry("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1", &lockfile.LockedAdapter{
			ResolvedDigest: newDigest.String(),
		})

	var out bytes.Buffer
	_, err := resolveOneAdapter(ctx, wa, old, resolver, nil, false, &signing.Policy{}, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable pin digest drift")
	assert.Contains(t, err.Error(), oldDigest.String())
	assert.Contains(t, err.Error(), newDigest.String())
	assert.Contains(t, err.Error(), "--upgrade")
}

func TestResolveOneAdapter_ImmutablePinDigestDriftAcceptedWithUpgrade(t *testing.T) {
	ctx := context.Background()
	oldDigest := digest.FromString("old-digest")
	newDigest := digest.FromString("new-digest")
	old := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{{
		Type:           "noop",
		Name:           "default",
		Reference:      "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1",
		Version:        "0.5.1",
		ResolvedDigest: oldDigest.String(),
	}}}
	wa := &workflowAdapter{
		Type:    "noop",
		Name:    "default",
		Source:  "ghcr.io/brokenbots/criteria-adapter-noop",
		Version: "0.5.1",
	}

	resolver := newFakeResolver().
		withBlob(oldDigest).
		withBlob(newDigest).
		withEntry("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1", &lockfile.LockedAdapter{
			ResolvedDigest: newDigest.String(),
		})

	var out bytes.Buffer
	entry, err := resolveOneAdapter(ctx, wa, old, resolver, nil, true, &signing.Policy{}, &out)
	require.NoError(t, err)
	assert.Equal(t, newDigest.String(), entry.ResolvedDigest)
	assert.Contains(t, out.String(), "immutable pin digest drift")
	assert.Contains(t, out.String(), "accepted by --upgrade")
}

func TestRunLock_ExtractsBinaryForSchemaVerification(t *testing.T) {
	stateDir := t.TempDir()
	adaptersDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_ADAPTERS", adaptersDir)

	cacheRoot := filepath.Join(stateDir, "cache", "oci")
	layout, err := oci.Open(cacheRoot)
	require.NoError(t, err)

	const adapterType = "noop"
	binContent := []byte("#!/bin/sh\necho locked\n")
	manifestDigest, _ := fakeArtifactFixture(t, layout, adapterType, binContent)

	dir := writeLockFixture(t, "0.5.1", func(lf *lockfile.Lockfile) {
		lf.Adapters[0].ResolvedDigest = manifestDigest.String()
	})

	resolver := newFakeResolver().
		withBlob(manifestDigest).
		withEntry("ghcr.io/brokenbots/criteria-adapter-noop:0.5.1", &lockfile.LockedAdapter{
			ResolvedDigest: manifestDigest.String(),
		}).
		withLayout(layout)

	var out bytes.Buffer
	err = runLock(context.Background(), dir, false, true, nil, &out, resolver)
	require.NoError(t, err)

	resolved, err := adapterhost.DiscoverBinaryAt(adapterType, adapterhost.EncodeDigest(manifestDigest))
	require.NoError(t, err)
	assert.FileExists(t, resolved)

	got, err := os.ReadFile(resolved)
	require.NoError(t, err)
	assert.Equal(t, binContent, got)
}

func TestLockThenValidate_FullSchemaVerification(t *testing.T) {
	ctx := context.Background()

	// Save the real noop binary built by TestMain before overriding
	// CRITERIA_ADAPTERS with a fresh directory. Use a unique adapter type label
	// in the workflow so there is no by-name install to mask a missing extraction.
	builtNoopPath := filepath.Join(os.Getenv("CRITERIA_ADAPTERS"), "criteria-adapter-noop")
	noopBinary, err := os.ReadFile(builtNoopPath)
	require.NoError(t, err, "read built noop adapter binary from %s", builtNoopPath)

	adaptersDir := t.TempDir()
	t.Setenv("CRITERIA_ADAPTERS", adaptersDir)

	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	workflowDir := t.TempDir()

	cacheRoot := filepath.Join(stateDir, "cache", "oci")
	layout, err := oci.Open(cacheRoot)
	require.NoError(t, err)

	// Use a unique adapter type label that does not exist as a by-name install in
	// any adapters root, so pre-fix behavior (no extraction) falls back to
	// permissive "schema unverified" validation.
	const adapterType = "lockvalidate"
	manifestDigest, _ := fakeArtifactFixture(t, layout, adapterType, noopBinary)

	ref, err := oci.Parse("ghcr.io/brokenbots/criteria-adapter-lockvalidate:0.5.1")
	require.NoError(t, err)

	resolver := newFakeResolver().
		withLayout(layout).
		withBlob(manifestDigest).
		withTag("0.5.1").
		withEntry(ref.String(), &lockfile.LockedAdapter{
			ResolvedDigest: manifestDigest.String(),
		})

	workflowSrc := `workflow {
  name = "lock-validate-test"
  version = "1.0"
  initial_state = "hello"
  target_state = "hello"
}

adapter "lockvalidate" "default" {
  source = "ghcr.io/brokenbots/criteria-adapter-lockvalidate"
  version = "0.5.1"
  config {}
}

step "hello" {
  target = adapter.lockvalidate.default
  input { delay_ms = "0" }
  outcome "success" { next = state.hello }
}
`
	workflowPath := filepath.Join(workflowDir, "workflow.hcl")
	require.NoError(t, os.WriteFile(workflowPath, []byte(workflowSrc), 0o644))

	var lockOut bytes.Buffer
	err = runLock(ctx, workflowDir, false, true, nil, &lockOut, resolver)
	require.NoError(t, err)
	assert.Contains(t, lockOut.String(), "locked lockvalidate.default")

	// Validate must resolve the extracted binary via the lockfile digest and
	// perform full schema verification. The only acceptable warning is the
	// "declares no output schema" notice from the real noop adapter; there must
	// be no "schema unverified" permissive-validation warning.
	out := captureOutput(t, func() {
		ok := validatePath(ctx, workflowPath, nil, true, false)
		assert.True(t, ok)
	})

	var diags []validateDiagnostic
	require.NoError(t, json.Unmarshal([]byte(out), &diags), "validate output must be JSON: %s", out)

	for _, d := range diags {
		assert.NotContains(t, d.Summary, "schema unverified",
			"lock followed by validate should resolve the adapter via the lockfile digest; got diagnostic: %s", d.Summary)
	}
}

// findLockedByEntry returns a copy of the locked adapter so it can be used to
// build a synthetic old lockfile in tests.
func findLockedByEntry(a *lockfile.LockedAdapter) *lockfile.LockedAdapter {
	if a == nil {
		return nil
	}
	b := *a
	return &b
}
