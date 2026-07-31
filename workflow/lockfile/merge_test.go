package lockfile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestMerge_BothNil(t *testing.T) {
	out := lockfile.Merge(nil, nil)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.SchemaVersion)
	assert.Empty(t, out.Adapters)
	assert.Empty(t, out.WorkflowRefs)
}

func TestMerge_ParentNil(t *testing.T) {
	child := &lockfile.Lockfile{
		SchemaVersion: 7,
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:child", SourceURL: "https://child.example", SDKProtocolVersion: 2},
		},
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	out := lockfile.Merge(nil, child)
	require.NotNil(t, out)
	// Merge does not propagate the child's schema version; the default schema
	// version is used when the parent is nil.
	assert.Equal(t, 1, out.SchemaVersion)
	assert.Len(t, out.Adapters, 1)
	assert.Equal(t, "a.x", keyFor(&out.Adapters[0]))
	assert.Len(t, out.WorkflowRefs, 1)
	assert.Equal(t, "loop", out.WorkflowRefs[0].Name)
}

func TestMerge_ChildNil(t *testing.T) {
	parent := &lockfile.Lockfile{
		SchemaVersion: 3,
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:parent", SourceURL: "https://parent.example", SDKProtocolVersion: 2},
		},
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "root", Source: "./root", ResolvedRef: "def", Kind: "git"},
		},
	}
	out := lockfile.Merge(parent, nil)
	require.NotNil(t, out)
	assert.Equal(t, 3, out.SchemaVersion)
	assert.Len(t, out.Adapters, 1)
	assert.Equal(t, "a.x", keyFor(&out.Adapters[0]))
	assert.Len(t, out.WorkflowRefs, 1)
	assert.Equal(t, "root", out.WorkflowRefs[0].Name)
}

func TestMerge_DisjointKeys(t *testing.T) {
	parent := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:p1", SourceURL: "https://p.example", SDKProtocolVersion: 2},
		},
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "first", Source: "./first", ResolvedRef: "p1", Kind: "git"},
		},
	}
	child := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "b", Name: "y", Reference: "r", ResolvedDigest: "sha256:c1", SourceURL: "https://c.example", SDKProtocolVersion: 2},
		},
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "second", Source: "./second", ResolvedRef: "c1", Kind: "git"},
		},
	}
	out := lockfile.Merge(parent, child)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.SchemaVersion)

	adapters := adaptersByKey(out.Adapters)
	assert.Len(t, adapters, 2)
	assert.Equal(t, "sha256:p1", adapters["a.x"].ResolvedDigest)
	assert.Equal(t, "sha256:c1", adapters["b.y"].ResolvedDigest)

	require.Len(t, out.WorkflowRefs, 2)
	assert.Equal(t, "first", out.WorkflowRefs[0].Name)
	assert.Equal(t, "second", out.WorkflowRefs[1].Name)
}

func TestMerge_ChildOverridesParent(t *testing.T) {
	parent := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "parent-ref", ResolvedDigest: "sha256:parent", SourceURL: "https://parent.example",
				SDKProtocolVersion: 2, Version: "1.0.0",
				Platforms: []string{"linux/amd64"},
				Signature: &lockfile.LockedSignature{
					Keyless: &lockfile.LockedKeyless{Issuer: "parent-issuer", Subject: "parent-subj"},
				},
				ContainerImage:                 &lockfile.LockedContainerImage{Ref: "parent-ci", Digest: "sha256:parent-ci"},
				Remote:                         &lockfile.LockedRemote{ListenAddress: "0.0.0.0:1", ServerCertFingerprint: "sha256:parent-cert"},
				CompatibleEnvironmentsOverride: []string{"shell"},
				OverriddenBy:                   "parent.hcl:1",
			},
		},
	}
	child := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type: "a", Name: "x", Reference: "child-ref", ResolvedDigest: "sha256:child", SourceURL: "https://child.example",
				SDKProtocolVersion: 3, Version: "2.0.0",
				Platforms: []string{"linux/arm64"},
				Signature: &lockfile.LockedSignature{
					Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: "sha256:child-fp"},
				},
				ContainerImage:                 &lockfile.LockedContainerImage{Ref: "child-ci", Digest: "sha256:child-ci"},
				Remote:                         &lockfile.LockedRemote{ListenAddress: "0.0.0.0:2", ServerCertFingerprint: "sha256:child-cert"},
				CompatibleEnvironmentsOverride: []string{"docker"},
				OverriddenBy:                   "child.hcl:2",
			},
		},
	}
	out := lockfile.Merge(parent, child)
	require.NotNil(t, out)
	adapters := adaptersByKey(out.Adapters)
	require.Len(t, adapters, 1)

	a := adapters["a.x"]
	assert.Equal(t, "child-ref", a.Reference)
	assert.Equal(t, "sha256:child", a.ResolvedDigest)
	assert.Equal(t, "https://child.example", a.SourceURL)
	assert.Equal(t, 3, a.SDKProtocolVersion)
	assert.Equal(t, "2.0.0", a.Version)
	assert.Equal(t, []string{"linux/arm64"}, a.Platforms)
	require.NotNil(t, a.Signature)
	require.NotNil(t, a.Signature.Key)
	assert.Equal(t, "sha256:child-fp", a.Signature.Key.Fingerprint)
	assert.Nil(t, a.Signature.Keyless)
	require.NotNil(t, a.ContainerImage)
	assert.Equal(t, "child-ci", a.ContainerImage.Ref)
	require.NotNil(t, a.Remote)
	assert.Equal(t, "0.0.0.0:2", a.Remote.ListenAddress)
	assert.Equal(t, []string{"docker"}, a.CompatibleEnvironmentsOverride)
	assert.Equal(t, "child.hcl:2", a.OverriddenBy)
}

func TestMerge_MixedCollisionAndDisjoint(t *testing.T) {
	parent := &lockfile.Lockfile{
		SchemaVersion: 2,
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:p1", SourceURL: "https://p.example", SDKProtocolVersion: 2},
			{Type: "a", Name: "y", Reference: "r", ResolvedDigest: "sha256:p2", SourceURL: "https://p.example", SDKProtocolVersion: 2},
		},
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "keep", Source: "./keep", ResolvedRef: "p", Kind: "git"},
		},
	}
	child := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:c1", SourceURL: "https://c.example", SDKProtocolVersion: 2},
			{Type: "b", Name: "z", Reference: "r", ResolvedDigest: "sha256:c2", SourceURL: "https://c.example", SDKProtocolVersion: 2},
		},
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "add", Source: "./add", ResolvedRef: "c", Kind: "git"},
		},
	}
	out := lockfile.Merge(parent, child)
	require.NotNil(t, out)
	assert.Equal(t, 2, out.SchemaVersion)

	adapters := adaptersByKey(out.Adapters)
	assert.Len(t, adapters, 3)
	assert.Equal(t, "sha256:p2", adapters["a.y"].ResolvedDigest)
	assert.Equal(t, "sha256:c1", adapters["a.x"].ResolvedDigest)
	assert.Equal(t, "sha256:c2", adapters["b.z"].ResolvedDigest)

	require.Len(t, out.WorkflowRefs, 2)
	assert.Equal(t, "keep", out.WorkflowRefs[0].Name)
	assert.Equal(t, "add", out.WorkflowRefs[1].Name)
}

func TestMerge_AdaptersOnly(t *testing.T) {
	parent := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:p", SourceURL: "https://p.example", SDKProtocolVersion: 2},
		},
	}
	child := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "b", Name: "y", Reference: "r", ResolvedDigest: "sha256:c", SourceURL: "https://c.example", SDKProtocolVersion: 2},
		},
	}
	out := lockfile.Merge(parent, child)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.SchemaVersion)
	assert.Len(t, out.Adapters, 2)
	assert.Empty(t, out.WorkflowRefs)
}

func TestMerge_RefsOnly(t *testing.T) {
	parent := &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "first", Source: "./first", ResolvedRef: "p", Kind: "git"},
		},
	}
	child := &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "second", Source: "./second", ResolvedRef: "c", Kind: "git"},
		},
	}
	out := lockfile.Merge(parent, child)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.SchemaVersion)
	assert.Empty(t, out.Adapters)
	require.Len(t, out.WorkflowRefs, 2)
	assert.Equal(t, "first", out.WorkflowRefs[0].Name)
	assert.Equal(t, "second", out.WorkflowRefs[1].Name)
}

func TestMerge_ChildLargerThanParent(t *testing.T) {
	parent := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x", Reference: "r", ResolvedDigest: "sha256:p", SourceURL: "https://p.example", SDKProtocolVersion: 2},
		},
	}
	var childAdapters []lockfile.LockedAdapter
	for i := 0; i < 10; i++ {
		childAdapters = append(childAdapters, lockfile.LockedAdapter{
			Type: "b", Name: nameFor(i), Reference: "r", ResolvedDigest: "sha256:c" + nameFor(i),
			SourceURL: "https://c.example", SDKProtocolVersion: 2,
		})
	}
	child := &lockfile.Lockfile{Adapters: childAdapters}
	out := lockfile.Merge(parent, child)
	require.NotNil(t, out)
	adapters := adaptersByKey(out.Adapters)
	assert.Len(t, adapters, 11)
	assert.Equal(t, "sha256:p", adapters["a.x"].ResolvedDigest)
	for i := 0; i < 10; i++ {
		name := nameFor(i)
		assert.Equal(t, "sha256:c"+name, adapters["b."+name].ResolvedDigest)
	}
}

func keyFor(a *lockfile.LockedAdapter) string {
	return a.Type + "." + a.Name
}

func adaptersByKey(adapters []lockfile.LockedAdapter) map[string]lockfile.LockedAdapter {
	m := make(map[string]lockfile.LockedAdapter, len(adapters))
	for i := range adapters {
		m[keyFor(&adapters[i])] = adapters[i]
	}
	return m
}

func nameFor(i int) string {
	return string(rune('a' + i))
}
