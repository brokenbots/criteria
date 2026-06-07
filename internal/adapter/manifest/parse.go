package manifest

import (
	"fmt"
	"io"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

// Parse reads an adapter.yaml manifest from r and returns the decoded Manifest.
func Parse(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("manifest: read: %w", err)
	}

	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: unmarshal: %w", err)
	}

	return &m, nil
}

// ParseFile reads an adapter.yaml manifest from the given file path.
func ParseFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: open %q: %w", path, err)
	}
	defer f.Close()

	m, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("manifest: parse %q: %w", path, err)
	}
	return m, nil
}

// ParseFromFS reads an adapter.yaml manifest from an fs.FS, typically the
// artifact FS returned by oci.Layout.Open.
func ParseFromFS(fsys fs.FS, name string) (*Manifest, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("manifest: open %q from fs: %w", name, err)
	}
	defer f.Close()

	m, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("manifest: parse %q from fs: %w", name, err)
	}
	return m, nil
}
