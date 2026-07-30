package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
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
	dg := digest.FromString(ref.String())
	entry, ok := f.entries[ref.String()]
	if !ok {
		entry = lockfile.LockedAdapter{
			Reference:          ref.String(),
			Version:            ref.Tag,
			ResolvedDigest:     dg.String(),
			SDKProtocolVersion: 2,
		}
	}
	// Ensure the returned entry carries the reference/version of the requested
	// ref even when the caller pre-registered a custom entry.
	entry.Reference = ref.String()
	entry.Version = ref.Tag
	entry.ResolvedDigest = dg.String()
	return dg, entry, nil
}

func (f *fakeLockResolver) withBlob(dg digest.Digest) *fakeLockResolver {
	f.blobs[dg] = true
	return f
}

func (f *fakeLockResolver) withTag(tag string) *fakeLockResolver {
	f.tags = append(f.tags, tag)
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

	assert.Empty(t, resolver.pulledRefs, "up-to-date pin should not be re-resolved")
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

	assert.Empty(t, fake.pulledRefs, "no pull should happen when lockfile already matches declarations")
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
