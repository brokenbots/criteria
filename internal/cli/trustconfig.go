package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/brokenbots/criteria/internal/adapter/signing"
)

// trustConfig is the schema of a trust HCL file (~/.criteria/trust.hcl or a
// trust.hcl alongside a workflow). It declares the public keys accepted for
// key-mode adapter verification (WS47).
type trustConfig struct {
	Keys []trustedKeyBlock `hcl:"trusted_key,block"`
}

type trustedKeyBlock struct {
	// Algorithm is informational; the real algorithm is derived from the key.
	Algorithm string `hcl:"algorithm,optional"`
	// Key is an inline PEM public key. Mutually exclusive with Path.
	Key string `hcl:"key,optional"`
	// Path is a PEM public-key file, resolved relative to the config file.
	Path string `hcl:"path,optional"`
}

// loadTrustedKeys resolves the union of trusted public keys from, in order:
//   - the global trust config (~/.criteria/trust.hcl, honoring CRITERIA_STATE_DIR)
//   - a trust.hcl in workflowDir (when non-empty)
//   - extraKeyPaths (ad-hoc --trusted-key PEM files)
//
// Keys with the same fingerprint are de-duplicated (global ∪ workflow ∪ flags).
// A missing global or workflow trust file is not an error; an unreadable or
// invalid one is.
func loadTrustedKeys(workflowDir string, extraKeyPaths []string) ([]signing.KeyIdentity, error) {
	seen := map[string]struct{}{}
	var out []signing.KeyIdentity
	add := func(k signing.KeyIdentity) {
		if _, ok := seen[k.Fingerprint]; ok {
			return
		}
		seen[k.Fingerprint] = struct{}{}
		out = append(out, k)
	}

	globalPath, err := defaultTrustConfigPath()
	if err != nil {
		return nil, err
	}
	globalKeys, err := loadTrustFile(globalPath)
	if err != nil {
		return nil, err
	}
	for _, k := range globalKeys {
		add(k)
	}

	if workflowDir != "" {
		wfKeys, err := loadTrustFile(filepath.Join(workflowDir, "trust.hcl"))
		if err != nil {
			return nil, err
		}
		for _, k := range wfKeys {
			add(k)
		}
	}

	for _, p := range extraKeyPaths {
		pem, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read trusted key %q: %w", p, err)
		}
		k, err := signing.NewTrustedKey(pem)
		if err != nil {
			return nil, fmt.Errorf("trusted key %q: %w", p, err)
		}
		add(k)
	}

	return out, nil
}

// loadTrustFile parses a single trust HCL file. A non-existent file yields no
// keys and no error.
func loadTrustFile(path string) ([]signing.KeyIdentity, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read trust config %q: %w", path, err)
	}
	f, diags := hclparse.NewParser().ParseHCL(data, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse trust config %q: %s", path, diags.Error())
	}
	var cfg trustConfig
	if diags := gohcl.DecodeBody(f.Body, nil, &cfg); diags.HasErrors() {
		return nil, fmt.Errorf("decode trust config %q: %s", path, diags.Error())
	}
	out := make([]signing.KeyIdentity, 0, len(cfg.Keys))
	for i := range cfg.Keys {
		pem, err := resolveKeyPEM(&cfg.Keys[i], path)
		if err != nil {
			return nil, fmt.Errorf("trust config %q trusted_key[%d]: %w", path, i, err)
		}
		k, err := signing.NewTrustedKey(pem)
		if err != nil {
			return nil, fmt.Errorf("trust config %q trusted_key[%d]: %w", path, i, err)
		}
		out = append(out, k)
	}
	return out, nil
}

func resolveKeyPEM(blk *trustedKeyBlock, configPath string) ([]byte, error) {
	switch {
	case blk.Key != "" && blk.Path != "":
		return nil, errors.New("set either `key` (inline PEM) or `path`, not both")
	case blk.Key != "":
		return []byte(blk.Key), nil
	case blk.Path != "":
		p := blk.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(configPath), p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read key file %q: %w", p, err)
		}
		return data, nil
	default:
		return nil, errors.New("trusted_key requires `key` (inline PEM) or `path`")
	}
}
