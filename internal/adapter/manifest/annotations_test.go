package manifest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
)

func TestAnnotationMap(t *testing.T) {
	m := &manifest.Manifest{
		SchemaVersion:      1,
		Name:               "test-adapter",
		Version:            "1.2.3",
		SourceURL:          "https://github.com/brokenbots/criteria",
		SDKProtocolVersion: 2,
		Capabilities:       []string{"parallel_safe", "pause"},
		Platforms: []manifest.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "darwin", Arch: "arm64"},
		},
	}

	am := manifest.AnnotationMap(m)

	assert.Equal(t, "test-adapter", am[manifest.AnnotationName])
	assert.Equal(t, "1.2.3", am[manifest.AnnotationVersion])
	assert.Equal(t, "https://github.com/brokenbots/criteria", am[manifest.AnnotationSourceURL])
	assert.Equal(t, "2", am[manifest.AnnotationProtoVer])
	assert.Equal(t, "1", am[manifest.AnnotationSchemaVer])
	assert.Equal(t, "parallel_safe,pause", am[manifest.AnnotationCapabilities])
	assert.Equal(t, "linux/amd64,darwin/arm64", am[manifest.AnnotationPlatforms])
}

func TestAnnotationMap_NoCapabilities(t *testing.T) {
	m := &manifest.Manifest{
		SchemaVersion:      1,
		Name:               "bare",
		Version:            "0.1.0",
		SourceURL:          "https://example.com",
		SDKProtocolVersion: 2,
		Platforms:          []manifest.Platform{{OS: "linux", Arch: "amd64"}},
	}

	am := manifest.AnnotationMap(m)

	_, ok := am[manifest.AnnotationCapabilities]
	assert.False(t, ok, "capabilities annotation should be omitted when empty")
}

func TestAnnotationMap_NoPlatforms(t *testing.T) {
	m := &manifest.Manifest{
		SchemaVersion:      1,
		Name:               "bare",
		Version:            "0.1.0",
		SourceURL:          "https://example.com",
		SDKProtocolVersion: 2,
		Platforms:          nil,
	}

	am := manifest.AnnotationMap(m)

	_, ok := am[manifest.AnnotationPlatforms]
	assert.False(t, ok, "platforms annotation should be omitted when empty")
}

func TestAnnotationMap_RoundTrip(t *testing.T) {
	m := &manifest.Manifest{
		SchemaVersion:      1,
		Name:               "round-trip",
		Version:            "1.0.0",
		SourceURL:          "https://example.com/src",
		SDKProtocolVersion: 2,
		Capabilities:       []string{"a", "b"},
		Platforms: []manifest.Platform{
			{OS: "linux", Arch: "amd64"},
		},
	}

	am := manifest.AnnotationMap(m)

	assert.Equal(t, m.Name, am[manifest.AnnotationName])
	assert.Equal(t, m.Version, am[manifest.AnnotationVersion])
	assert.Equal(t, m.SourceURL, am[manifest.AnnotationSourceURL])
	assert.Equal(t, "2", am[manifest.AnnotationProtoVer])
	assert.Equal(t, "1", am[manifest.AnnotationSchemaVer])
	assert.Equal(t, "a,b", am[manifest.AnnotationCapabilities])
	assert.Equal(t, "linux/amd64", am[manifest.AnnotationPlatforms])
}
