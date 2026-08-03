package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	hplugin "github.com/hashicorp/go-plugin"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

func newAdapterListCmd() *cobra.Command {
	var (
		installed  bool
		referenced bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cached or workflow-referenced adapters",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if !installed && !referenced {
				installed = true
			}
			return runList(cmd.OutOrStdout(), installed, referenced)
		},
	}

	cmd.Flags().BoolVar(&installed, "installed", false, "List adapters present in the local OCI cache")
	cmd.Flags().BoolVar(&referenced, "referenced", false, "List adapters referenced by the current workflow's lockfile")
	return cmd
}

func runList(out io.Writer, installed, referenced bool) error {
	if installed {
		if err := listInstalled(out); err != nil {
			return err
		}
	}
	if referenced {
		if err := listReferenced(out); err != nil {
			return err
		}
	}
	return nil
}

// listEntry is a single line emitted by `criteria adapter list`.
type listEntry struct {
	// key is the adapter name used for sorting.
	key string
	// line is the formatted output for this entry.
	line string
}

func listInstalled(out io.Writer) error {
	entries, err := collectInstalledEntries()
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		fmt.Fprintln(out, "(no cached adapters)")
		return nil
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].key < entries[j].key
	})

	for _, e := range entries {
		fmt.Fprint(out, e.line)
	}
	return nil
}

// collectInstalledEntries gathers cached OCI manifests and filesystem-installed
// adapter binaries, returning a printable line per entry. It never fails because
// of a single broken binary; unresponsive installed adapters are reported and
// the listing continues.
func collectInstalledEntries() ([]listEntry, error) {
	entries, err := collectOCIEntries()
	if err != nil {
		return nil, err
	}

	entries = append(entries, collectFilesystemEntries()...)

	return entries, nil
}

func collectOCIEntries() ([]listEntry, error) {
	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return nil, err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return nil, fmt.Errorf("open cache: %w", err)
	}
	ix, err := layout.Index()
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}

	entries := make([]listEntry, 0, len(ix.Manifests))
	for _, m := range ix.Manifests {
		dg := m.Digest.String()
		name := "(unknown)"
		artFS, err := layout.Open(m.Digest)
		if err == nil {
			if mf, err := manifest.ParseFromFS(artFS, "adapter.yaml"); err == nil {
				name = mf.Name
			}
		}
		ref := m.Annotations[oci.AnnotationReference]
		source := m.Annotations[oci.AnnotationSourceURL]
		if ref == "" {
			ref = "(unattributed)"
		}
		if source == "" {
			source = "(unattributed)"
		}
		key := name
		if key == "(unknown)" {
			key = dg
		}
		entries = append(entries, listEntry{
			key:  key,
			line: fmt.Sprintf("%s  %s  %s  %s\n", dg, name, ref, source),
		})
	}

	return entries, nil
}

func collectFilesystemEntries() []listEntry {
	entries := make([]listEntry, 0)
	seen := map[string]struct{}{}
	prefix := adapterhost.AdapterBinaryName("")

	for _, root := range adapterhost.AdaptersRoots() {
		files, err := os.ReadDir(root)
		if err != nil {
			// A missing root simply means no adapters installed there.
			continue
		}
		for _, f := range files {
			if e, ok := installedEntryFor(root, f, prefix, seen); ok {
				entries = append(entries, e)
			}
		}
	}

	return entries
}

// installedEntryFor converts a single directory entry into a list entry when it
// names a runnable adapter binary. It updates seen for the adapter name when the
// entry is accepted so earlier discovery roots take precedence. Broken binaries
// still produce an entry marked unresponsive.
func installedEntryFor(root string, f os.DirEntry, prefix string, seen map[string]struct{}) (listEntry, bool) {
	if f.IsDir() {
		return listEntry{}, false
	}
	name := f.Name()
	if !strings.HasPrefix(name, prefix) {
		return listEntry{}, false
	}
	adapterName := strings.TrimPrefix(name, prefix)
	if !adapterhost.IsValidAdapterName(adapterName) {
		return listEntry{}, false
	}

	path := filepath.Join(root, name)
	if !adapterhost.IsRunnableFile(path) {
		return listEntry{}, false
	}
	if _, ok := seen[adapterName]; ok {
		return listEntry{}, false
	}
	seen[adapterName] = struct{}{}

	info, err := queryAdapterInfo(path, adapterName)
	if err != nil {
		return listEntry{
			key:  adapterName,
			line: fmt.Sprintf("%s (unresponsive)  -  %s\n", adapterName, path),
		}, true
	}
	version := info.Version
	if version == "" {
		version = "-"
	}
	return listEntry{
		key:  info.Name,
		line: fmt.Sprintf("%s  %s  %s\n", info.Name, version, path),
	}, true
}

// queryAdapterInfo starts the adapter binary at path long enough to perform a
// single Info() RPC, then kills the process. It returns the adapter's reported
// name and version.
func queryAdapterInfo(path, fallbackName string) (adapterhost.Info, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := hplugin.NewClient(&hplugin.ClientConfig{
		HandshakeConfig:  adapterhost.HandshakeConfig,
		Plugins:          adapterhost.AdapterMap(),
		Cmd:              osExecCommand(path),
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		StartTimeout:     10 * time.Second,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "adapter-list",
			Output: io.Discard,
			Level:  hclog.Error,
		}),
	})
	defer client.Kill()

	rpcClient, err := client.Client()
	if err != nil {
		return adapterhost.Info{}, err
	}
	raw, err := rpcClient.Dispense(adapterhost.AdapterName)
	if err != nil {
		return adapterhost.Info{}, err
	}
	adapterClient, ok := raw.(adapterhost.Client)
	if !ok {
		return adapterhost.Info{}, fmt.Errorf("unexpected adapter client type %T", raw)
	}

	handle := adapterhost.NewRPCHandle(fallbackName, client, adapterClient)
	return handle.Info(ctx)
}

// osExecCommand returns an exec.Cmd for the adapter binary. It is factored out
// so the real list path can be tested without actually starting a plugin.
func osExecCommand(path string) *exec.Cmd {
	return exec.Command(path)
}

func listReferenced(out io.Writer) error {
	lf, err := lockfile.ReadFromDir(".")
	if err != nil {
		return fmt.Errorf("read lockfile: %w", err)
	}
	if lf == nil {
		fmt.Fprintln(out, "(no lockfile in current directory)")
		return nil
	}

	sorted := make([]lockfile.LockedAdapter, len(lf.Adapters))
	copy(sorted, lf.Adapters)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Type+"."+sorted[i].Name < sorted[j].Type+"."+sorted[j].Name
	})

	for i := range sorted {
		fmt.Fprintf(out, "%s.%s  %s  %s\n", sorted[i].Type, sorted[i].Name, sorted[i].Reference, sorted[i].ResolvedDigest)
	}
	return nil
}
