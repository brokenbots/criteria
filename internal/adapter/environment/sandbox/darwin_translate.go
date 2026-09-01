//go:build darwin

package sandbox

import (
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// dnsCacheTTL is the maximum age of a cached DNS entry.
const dnsCacheTTL = 5 * time.Minute

// dnsCache stores per-process hostname → IP lookups.
var (
	dnsCacheMu sync.RWMutex
	dnsCache   = make(map[string]dnsEntry)
)

type dnsEntry struct {
	ips      []string
	cachedAt time.Time
}

// FromPolicy translates a ResolvedPolicy into an SBPL Profile.
func FromPolicy(p workflow.ResolvedPolicy, adapterBinary string) Profile {
	prof := Profile{
		DefaultDeny: true,
		AllowExec:   []string{},
	}

	if adapterBinary != "" {
		prof.AllowExec = append(prof.AllowExec, adapterBinary)
		prof.AllowFileReads = append(prof.AllowFileReads, adapterBinary)
		binDir := filepath.Dir(adapterBinary)
		if binDir != "" && binDir != "/" {
			prof.AllowFileReads = append(prof.AllowFileReads, binDir)
		}
		// Local adapter execution uses go-plugin's Unix-domain socket transport.
		// macOS sandbox-exec classifies UDS bind() as network-bind, so grant it
		// unconditionally; it is independent of user-declared network egress.
		prof.AllowNetworkBind = true
	}

	fsObj := typeSpecific(p, "filesystem")
	applyFilesystemPolicy(&prof, fsObj)
	applyNetworkPolicy(&prof, typeSpecific(p, "network"))

	prof.BlockKextLoad = boolFromObject(fsObj, "block_kext_load", false)
	prof.BlockMachLookup = boolFromObject(fsObj, "block_mach_lookup", false)

	return prof
}

// typeSpecific returns the named entry from a policy's TypeSpecific map, or
// cty.NilVal when absent.
func typeSpecific(p workflow.ResolvedPolicy, key string) cty.Value {
	if p.TypeSpecific == nil {
		return cty.NilVal
	}
	if v, ok := p.TypeSpecific[key]; ok {
		return v
	}
	return cty.NilVal
}

// applyFilesystemPolicy translates the filesystem object's read/write/read_write
// path lists into the profile's allow lists and records validation warnings.
func applyFilesystemPolicy(prof *Profile, fsObj cty.Value) {
	readPaths := pathListFromObject(fsObj, "read")
	writePaths := pathListFromObject(fsObj, "write")
	rwPaths := pathListFromObject(fsObj, "read_write")

	prof.AllowFileReads = append(prof.AllowFileReads, readPaths...)
	prof.AllowFileWrites = append(prof.AllowFileWrites, writePaths...)
	// read_write is a combined key for convenience.
	prof.AllowFileReads = append(prof.AllowFileReads, rwPaths...)
	prof.AllowFileWrites = append(prof.AllowFileWrites, rwPaths...)

	for _, path := range append(readPaths, append(writePaths, rwPaths...)...) {
		if err := validatePath(path); err != nil {
			prof.resolveWarnings = append(prof.resolveWarnings, resolveWarn{host: path, err: err})
		}
	}
}

// applyNetworkPolicy resolves the network object's allow hosts to IPs and
// records warnings for hosts that fail to resolve.
func applyNetworkPolicy(prof *Profile, netObj cty.Value) {
	for _, host := range pathListFromObject(netObj, "allow") {
		resolved := resolveHost(host)
		if len(resolved) == 0 {
			prof.resolveWarnings = append(prof.resolveWarnings, resolveWarn{
				host: host,
				err:  fmt.Errorf("no IP addresses resolved"),
			})
			continue
		}
		prof.AllowNetworkHosts = append(prof.AllowNetworkHosts, resolved...)
	}
}

// resolveHost resolves a hostname:port into "ip:port" strings.
// Results are cached for up to dnsCacheTTL.
func resolveHost(host string) []string {
	// Strip port if present.
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name = host
		port = ""
	}

	dnsCacheMu.RLock()
	entry, ok := dnsCache[name]
	dnsCacheMu.RUnlock()
	if ok && time.Since(entry.cachedAt) < dnsCacheTTL {
		return formatResolved(entry.ips, port)
	}

	ips, err := net.LookupHost(name)
	if err != nil || len(ips) == 0 {
		return nil
	}

	dnsCacheMu.Lock()
	dnsCache[name] = dnsEntry{ips: ips, cachedAt: time.Now()}
	dnsCacheMu.Unlock()

	return formatResolved(ips, port)
}

func formatResolved(ips []string, port string) []string {
	var out []string
	for _, ip := range ips {
		if port != "" {
			out = append(out, net.JoinHostPort(ip, port))
		} else {
			out = append(out, ip)
		}
	}
	return out
}
