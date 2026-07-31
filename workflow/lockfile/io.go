package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// LockfileName is the on-disk name of the adapter lockfile that lives alongside
// a workflow's source files.
const LockfileName = ".criteria.lock.hcl"

// Read parses a .criteria.lock.hcl file from disk.
func Read(path string) (*Lockfile, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read lockfile %q: %w", path, err)
	}

	parser := hclparse.NewParser()
	f, diags := parser.ParseHCL(src, path)
	if diags.HasErrors() {
		return nil, diags
	}
	if f == nil {
		return nil, fmt.Errorf("parse lockfile %q: nil file", path)
	}

	var lf Lockfile
	if decodeDiags := gohcl.DecodeBody(f.Body, nil, &lf); decodeDiags.HasErrors() {
		return nil, decodeDiags
	}
	return &lf, nil
}

// Write serialises lf to path using canonical HCL formatting.
//
// The output is deterministic: adapters are sorted by "<type>.<name>",
// attributes appear in a fixed order, and nested blocks are always emitted
// in the order signature, container_image, remote.
func Write(path string, lf *Lockfile) error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()

	body.SetAttributeValue("schema_version", cty.NumberIntVal(int64(lf.SchemaVersion)))

	sorted := make([]LockedAdapter, len(lf.Adapters))
	copy(sorted, lf.Adapters)
	sort.Slice(sorted, func(i, j int) bool {
		ki := sorted[i].Type + "." + sorted[i].Name
		kj := sorted[j].Type + "." + sorted[j].Name
		return ki < kj
	})

	for i := range sorted {
		writeAdapterBlock(body, &sorted[i])
	}

	wfSorted := make([]LockedWorkflowRef, len(lf.WorkflowRefs))
	copy(wfSorted, lf.WorkflowRefs)
	sort.Slice(wfSorted, func(i, j int) bool {
		return wfSorted[i].Name < wfSorted[j].Name
	})
	for i := range wfSorted {
		writeWorkflowRefBlock(body, &wfSorted[i])
	}

	if err := os.WriteFile(path, f.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write lockfile %q: %w", path, err)
	}
	return nil
}

func writeWorkflowRefBlock(body *hclwrite.Body, w *LockedWorkflowRef) {
	blk := body.AppendNewBlock("workflow_ref", []string{w.Name})
	wb := blk.Body()
	wb.SetAttributeValue("source", cty.StringVal(w.Source))
	wb.SetAttributeValue("resolved_ref", cty.StringVal(w.ResolvedRef))
	wb.SetAttributeValue("kind", cty.StringVal(w.Kind))
}

func writeAdapterBlock(body *hclwrite.Body, a *LockedAdapter) { //nolint:funlen // WS07: canonical emitter for all adapter block attributes and nested blocks
	blk := body.AppendNewBlock("adapter", []string{a.Type, a.Name})
	ab := blk.Body()

	ab.SetAttributeValue("reference", cty.StringVal(a.Reference))
	if a.Version != "" {
		ab.SetAttributeValue("version", cty.StringVal(a.Version))
	}
	ab.SetAttributeValue("resolved_digest", cty.StringVal(a.ResolvedDigest))
	ab.SetAttributeValue("source_url", cty.StringVal(a.SourceURL))
	ab.SetAttributeValue("sdk_protocol_version", cty.NumberIntVal(int64(a.SDKProtocolVersion)))

	if len(a.Platforms) > 0 {
		vals := make([]cty.Value, len(a.Platforms))
		for i, p := range a.Platforms {
			vals[i] = cty.StringVal(p)
		}
		ab.SetAttributeValue("platforms", cty.ListVal(vals))
	}

	if a.Signature != nil {
		sigBlk := ab.AppendNewBlock("signature", nil)
		sigB := sigBlk.Body()
		if a.Signature.Keyless != nil {
			klBlk := sigB.AppendNewBlock("keyless", nil)
			klB := klBlk.Body()
			klB.SetAttributeValue("issuer", cty.StringVal(a.Signature.Keyless.Issuer))
			klB.SetAttributeValue("subject", cty.StringVal(a.Signature.Keyless.Subject))
		}
		if a.Signature.Key != nil {
			kBlk := sigB.AppendNewBlock("key", nil)
			kB := kBlk.Body()
			kB.SetAttributeValue("algorithm", cty.StringVal(a.Signature.Key.Algorithm))
			kB.SetAttributeValue("fingerprint", cty.StringVal(a.Signature.Key.Fingerprint))
		}
	}

	if a.ContainerImage != nil {
		ciBlk := ab.AppendNewBlock("container_image", nil)
		ciB := ciBlk.Body()
		ciB.SetAttributeValue("ref", cty.StringVal(a.ContainerImage.Ref))
		ciB.SetAttributeValue("digest", cty.StringVal(a.ContainerImage.Digest))
	}

	if a.Remote != nil {
		rBlk := ab.AppendNewBlock("remote", nil)
		rB := rBlk.Body()
		rB.SetAttributeValue("listen_address", cty.StringVal(a.Remote.ListenAddress))
		rB.SetAttributeValue("server_cert_fingerprint", cty.StringVal(a.Remote.ServerCertFingerprint))
	}

	if len(a.CompatibleEnvironmentsOverride) > 0 {
		vals := make([]cty.Value, len(a.CompatibleEnvironmentsOverride))
		for i, e := range a.CompatibleEnvironmentsOverride {
			vals[i] = cty.StringVal(e)
		}
		ab.SetAttributeValue("compatible_environments_override", cty.ListVal(vals))
	}

	if a.OverriddenBy != "" {
		ab.SetAttributeValue("overridden_by", cty.StringVal(a.OverriddenBy))
	}
}

// ReadFromDir looks for a file named .criteria.lock.hcl inside workflowDir
// and returns the parsed Lockfile. If the file does not exist, it returns
// (nil, nil).
func ReadFromDir(workflowDir string) (*Lockfile, error) {
	path := filepath.Join(workflowDir, LockfileName)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat lockfile %q: %w", path, err)
	}
	return Read(path)
}
