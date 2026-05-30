//go:build darwin

package sandbox

import (
	"fmt"
	"net"
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
	ips       []string
	cachedAt  time.Time
}

// FromPolicy translates a ResolvedPolicy into an SBPL Profile.
func FromPolicy(p workflow.ResolvedPolicy, adapterBinary string) Profile {
	prof := Profile{
		DefaultDeny: true,
		AllowExec:   []string{},
	}

	if adapterBinary != "" {
		prof.AllowExec = append(prof.AllowExec, adapterBinary)
	}

	fsObj := cty.NilVal
	if p.TypeSpecific != nil {
		if v, ok := p.TypeSpecific["filesystem"]; ok {
			fsObj = v
		}
	}
	readPaths := pathListFromObject(fsObj, "read")
	writePaths := pathListFromObject(fsObj, "write")
	for _, path := range readPaths {
		prof.AllowFileReads = append(prof.AllowFileReads, path)
	}
	for _, path := range writePaths {
		prof.AllowFileWrites = append(prof.AllowFileWrites, path)
	}
	// Also support read_write as a combined key for convenience.
	rwPaths := pathListFromObject(fsObj, "read_write")
	for _, path := range rwPaths {
		prof.AllowFileReads = append(prof.AllowFileReads, path)
		prof.AllowFileWrites = append(prof.AllowFileWrites, path)
	}
	for _, path := range append(readPaths, append(writePaths, rwPaths...)...) {
		if err := validatePath(path); err != nil {
			prof.resolveWarnings = append(prof.resolveWarnings, resolveWarn{
				host: path,
				err:  err,
			})
		}
	}

	netObj := cty.NilVal
	if p.TypeSpecific != nil {
		if v, ok := p.TypeSpecific["network"]; ok {
			netObj = v
		}
	}
	netAllow := pathListFromObject(netObj, "allow")
	for _, host := range netAllow {
		resolved := resolveHost(host)
		if len(resolved) > 0 {
			for _, ip := range resolved {
				prof.AllowNetworkHosts = append(prof.AllowNetworkHosts, ip)
			}
		} else {
			prof.resolveWarnings = append(prof.resolveWarnings, resolveWarn{
				host: host,
				err:  fmt.Errorf("no IP addresses resolved"),
			})
		}
	}

	prof.BlockSysctl = boolFromObject(fsObj, "block_sysctl", false)
	prof.BlockMachLookup = boolFromObject(fsObj, "block_mach_lookup", false)

	return prof
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
