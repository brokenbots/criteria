package lockfile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestDiff_NoChanges(t *testing.T) {
	a := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d1", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	b := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d1", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	changes := lockfile.Diff(a, b)
	assert.Empty(t, changes)
}

func TestDiff_NilInputs(t *testing.T) {
	changes := lockfile.Diff(nil, nil)
	assert.Empty(t, changes)
}

func TestDiff_Added(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2},
			{Type: "b", Name: "y", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, "b.y", changes[0].Adapter)
	assert.Equal(t, lockfile.Added, changes[0].Kind)
}

func TestDiff_Removed(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2},
			{Type: "b", Name: "y", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, "b.y", changes[0].Adapter)
	assert.Equal(t, lockfile.Removed, changes[0].Kind)
}

func TestDiff_DigestChanged(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:old", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:new", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.DigestChanged, changes[0].Kind)
	assert.Equal(t, "sha256:old", changes[0].Before)
	assert.Equal(t, "sha256:new", changes[0].After)
}

func TestDiff_SignerChanged(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2,
				Signature: &lockfile.LockedSignature{
					Keyless: &lockfile.LockedKeyless{Issuer: "old-issuer", Subject: "old-subj"},
				},
			},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2,
				Signature: &lockfile.LockedSignature{
					Keyless: &lockfile.LockedKeyless{Issuer: "new-issuer", Subject: "new-subj"},
				},
			},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.SignerChanged, changes[0].Kind)
}

func TestDiff_PlatformsChanged(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2, Platforms: []string{"linux/amd64"}},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2, Platforms: []string{"linux/amd64", "linux/arm64"}},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.PlatformsChanged, changes[0].Kind)
	assert.Equal(t, []string{"linux/amd64"}, changes[0].Before)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, changes[0].After)
}

func TestDiff_ContainerImageChanged(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2,
				ContainerImage: &lockfile.LockedContainerImage{Ref: "old-ref", Digest: "sha256:old"},
			},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2,
				ContainerImage: &lockfile.LockedContainerImage{Ref: "new-ref", Digest: "sha256:new"},
			},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.ContainerImageChanged, changes[0].Kind)
}

func TestDiff_RemoteChanged(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2,
				Remote: &lockfile.LockedRemote{ListenAddress: "0.0.0.0:1111", ServerCertFingerprint: "sha256:old"},
			},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2,
				Remote: &lockfile.LockedRemote{ListenAddress: "0.0.0.0:2222", ServerCertFingerprint: "sha256:new"},
			},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.RemoteChanged, changes[0].Kind)
}

func TestDiff_OverrideChanged(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2,
				CompatibleEnvironmentsOverride: []string{"shell"},
				OverriddenBy:                   "workflow.hcl:1",
			},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2,
				CompatibleEnvironmentsOverride: []string{"shell", "docker"},
				OverriddenBy:                   "workflow.hcl:2",
			},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.OverrideChanged, changes[0].Kind)
}

func TestDiff_MultipleChangesSorted(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d1", SourceURL: "https://example.com", SDKProtocolVersion: 2},
			{Type: "b", Name: "y", Reference: "r", ResolvedDigest: "sha256:d2", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:new", SourceURL: "https://example.com", SDKProtocolVersion: 2},
			{Type: "c", Name: "z", Reference: "r", ResolvedDigest: "sha256:d3", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 3)

	// Sorted by adapter key, then kind.
	assert.Equal(t, "a.x", changes[0].Adapter)
	assert.Equal(t, lockfile.DigestChanged, changes[0].Kind)

	assert.Equal(t, "b.y", changes[1].Adapter)
	assert.Equal(t, lockfile.Removed, changes[1].Kind)

	assert.Equal(t, "c.z", changes[2].Adapter)
	assert.Equal(t, lockfile.Added, changes[2].Kind)
}

func TestDiff_WorkflowRefAddedRemoved(t *testing.T) {
	old := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	nextLF := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "release", Source: "https://example.com/release", ResolvedRef: "v1.0.0", Kind: "git"},
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, "workflow_ref.release", changes[0].Adapter)
	assert.Equal(t, lockfile.WorkflowRefChanged, changes[0].Kind)
	// Added: Before is nil.
	assert.Nil(t, changes[0].Before)
	after := changes[0].After
	require.NotNil(t, after)
	afterVal, ok := after.(lockfile.LockedWorkflowRef)
	require.True(t, ok)
	assert.Equal(t, "release", afterVal.Name)
	assert.Equal(t, "v1.0.0", afterVal.ResolvedRef)
}

func TestDiff_WorkflowRefRemoved(t *testing.T) {
	old := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
			{Name: "release", Source: "https://example.com/release", ResolvedRef: "v1.0.0", Kind: "git"},
		},
	}
	nextLF := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, "workflow_ref.release", changes[0].Adapter)
	assert.Equal(t, lockfile.WorkflowRefChanged, changes[0].Kind)
	before := changes[0].Before
	require.NotNil(t, before)
	beforeVal, ok := before.(lockfile.LockedWorkflowRef)
	require.True(t, ok)
	assert.Equal(t, "release", beforeVal.Name)
	// Removed: After is nil.
	assert.Nil(t, changes[0].After)
}

func TestDiff_WorkflowRefResolvedRefChanged(t *testing.T) {
	old := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	nextLF := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "def", Kind: "git"},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, "workflow_ref.loop", changes[0].Adapter)
	assert.Equal(t, lockfile.WorkflowRefChanged, changes[0].Kind)
	before := changes[0].Before.(lockfile.LockedWorkflowRef)
	after := changes[0].After.(lockfile.LockedWorkflowRef)
	assert.Equal(t, "abc", before.ResolvedRef)
	assert.Equal(t, "def", after.ResolvedRef)
}

func TestDiff_WorkflowRefSourceChanged(t *testing.T) {
	old := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	nextLF := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop2", ResolvedRef: "abc", Kind: "git"},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.WorkflowRefChanged, changes[0].Kind)
}

func TestDiff_WorkflowRefKindChanged(t *testing.T) {
	old := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	nextLF := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "archive"},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.WorkflowRefChanged, changes[0].Kind)
}

func TestDiff_WorkflowRefUnchanged(t *testing.T) {
	old := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	nextLF := &lockfile.Lockfile{
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	assert.Empty(t, changes)
}

func TestDiff_PlatformsLengthMismatch(t *testing.T) {
	// stringSliceEqual length-mismatch branch.
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2, Platforms: []string{"linux/amd64"}},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2, Platforms: []string{"linux/amd64", "linux/arm64"}},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.PlatformsChanged, changes[0].Kind)
}

func TestDiff_PlatformsSameLengthContentMismatch(t *testing.T) {
	// stringSliceEqual element-mismatch branch.
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2, Platforms: []string{"linux/amd64"}},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2, Platforms: []string{"linux/arm64"}},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, lockfile.PlatformsChanged, changes[0].Kind)
	assert.Equal(t, []string{"linux/amd64"}, changes[0].Before)
	assert.Equal(t, []string{"linux/arm64"}, changes[0].After)
}

func TestDiff_NilOldInput(t *testing.T) {
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	changes := lockfile.Diff(nil, nextLF)
	require.Len(t, changes, 1)
	assert.Equal(t, "a.x", changes[0].Adapter)
	assert.Equal(t, lockfile.Added, changes[0].Kind)
}

func TestDiff_NilNextInput(t *testing.T) {
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	changes := lockfile.Diff(old, nil)
	require.Len(t, changes, 1)
	assert.Equal(t, "a.x", changes[0].Adapter)
	assert.Equal(t, lockfile.Removed, changes[0].Kind)
}

func TestDiff_SameAdapterMultipleChanges(t *testing.T) {
	// Two changes for the same adapter key exercise Diff's secondary sort
	// criterion (by Kind) and stringSliceEqual/override logic.
	old := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:old",
				SourceURL: "https://example.com", SDKProtocolVersion: 2,
				Platforms:                      []string{"linux/amd64"},
				CompatibleEnvironmentsOverride: []string{"shell"},
				OverriddenBy:                   "workflow.hcl:1",
			},
		},
	}
	nextLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:new",
				SourceURL: "https://example.com", SDKProtocolVersion: 2,
				Platforms:                      []string{"linux/arm64"},
				CompatibleEnvironmentsOverride: []string{"docker"},
				OverriddenBy:                   "workflow.hcl:2",
			},
		},
	}
	changes := lockfile.Diff(old, nextLF)
	require.Len(t, changes, 3)
	// Sorted by adapter key (all "a.x") then Kind.
	assert.Equal(t, "a.x", changes[0].Adapter)
	assert.Equal(t, lockfile.DigestChanged, changes[0].Kind)
	assert.Equal(t, "a.x", changes[1].Adapter)
	assert.Equal(t, lockfile.PlatformsChanged, changes[1].Kind)
	assert.Equal(t, "a.x", changes[2].Adapter)
	assert.Equal(t, lockfile.OverrideChanged, changes[2].Kind)
}
