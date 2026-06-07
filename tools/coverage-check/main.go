// Command coverage-check enforces a per-package coverage ratchet (WS44).
//
// It reads one or more Go coverage profiles (the raw `-coverprofile` output,
// not `go tool cover -func`), aggregates statement-weighted coverage per
// repo-relative package, and compares each package against the floors recorded
// in tools/coverage-floors.txt.
//
// This is a non-regression gate, not a minimum-percentage gate: the committed
// floors are the current numbers. Future work may raise a floor but must not
// lower it without a documented reason in PR review.
//
// Usage:
//
//	coverage-check [-floors FILE] profile.out [profile2.out ...]   # check
//	coverage-check -capture profile.out [...]                      # emit floors
//
// Statement-weighted coverage matches `go tool cover -func`'s total and is
// stable across platforms for packages that compile the same files everywhere,
// which is why OS-divergent packages are excluded from -capture.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

const modulePrefix = "github.com/brokenbots/criteria/"

// capturePkgExclusions lists package-path prefixes omitted from -capture output.
// They are poor ratchet targets: OS-divergent packages compile different files
// per platform (so a macOS-captured floor can spuriously fail Linux CI), and
// external adapter binaries / test fixtures are low-signal (the conformance
// suite gates adapters, not coverage). A package may still be floored manually.
var capturePkgExclusions = []string{
	"internal/adapter/environment/sandbox", // build-tagged linux/darwin
	"internal/adapterhost",                 // build-tagged linux/darwin
	"cmd/criteria-adapter",                 // external adapter binaries (WS44 out-of-scope)
	"sdk/pb",                               // generated proto bindings (not a ratchet target)
}

func main() {
	floorsPath := flag.String("floors", "tools/coverage-floors.txt", "per-package coverage floors file")
	capture := flag.Bool("capture", false, "print floor lines for qualifying packages instead of checking")
	minStmts := flag.Int("min-stmts", 100, "minimum statements for a package to qualify under -capture")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: coverage-check [-floors FILE] [-capture] profile.out [profile2.out ...]")
		os.Exit(2)
	}

	pkgs := newPkgCov()
	for _, f := range flag.Args() {
		if err := pkgs.addProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(2)
		}
	}

	if *capture {
		captureFloors(pkgs, *minStmts)
		return
	}

	floors, err := readFloors(*floorsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(2)
	}
	if check(pkgs, floors) {
		os.Exit(1)
	}
}

// pkgCov accumulates covered and total statements per repo-relative package.
type pkgCov struct {
	covered map[string]int
	total   map[string]int
}

func newPkgCov() *pkgCov {
	return &pkgCov{covered: map[string]int{}, total: map[string]int{}}
}

// pct returns statement-weighted coverage for a package, or false if unmeasured.
func (p *pkgCov) pct(pkg string) (float64, bool) {
	t, ok := p.total[pkg]
	if !ok || t == 0 {
		return 0, false
	}
	return 100 * float64(p.covered[pkg]) / float64(t), true
}

// addProfile parses one coverage profile and folds it into the accumulator.
// Profile lines after the `mode:` header look like:
//
//	github.com/brokenbots/criteria/internal/cli/apply.go:20.13,22.2 3 1
//	<file>:<startLine>.<col>,<endLine>.<col> <numStmt> <execCount>
func (p *pkgCov) addProfile(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return fmt.Errorf("%s: malformed profile line: %q", file, line)
		}
		numStmt, err := strconv.Atoi(fields[1])
		if err != nil {
			return fmt.Errorf("%s: bad statement count in %q: %w", file, line, err)
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			return fmt.Errorf("%s: bad exec count in %q: %w", file, line, err)
		}
		// fields[0] is "<path>.go:<range>"; Go file paths contain no ':'.
		src := fields[0]
		if i := strings.LastIndex(src, ":"); i >= 0 {
			src = src[:i]
		}
		pkg := path.Dir(strings.TrimPrefix(src, modulePrefix))

		p.total[pkg] += numStmt
		if count > 0 {
			p.covered[pkg] += numStmt
		}
	}
	return sc.Err()
}

// floorDown rounds a percentage down to the nearest 0.5 to leave a small buffer
// against measurement jitter.
func floorDown(pct float64) float64 {
	return math.Floor(pct*2) / 2
}

func isExcluded(pkg string) bool {
	for _, pre := range capturePkgExclusions {
		// Match the package itself, a subpackage (pre/...), or a sibling in the
		// same naming family (pre-...), e.g. "cmd/criteria-adapter-mcp".
		if pkg == pre || strings.HasPrefix(pkg, pre+"/") || strings.HasPrefix(pkg, pre+"-") {
			return true
		}
	}
	return false
}

func captureFloors(p *pkgCov, minStmts int) {
	pkgs := make([]string, 0, len(p.total))
	for pkg := range p.total {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)

	fmt.Println("# Per-package coverage floors (WS44 ratchet). Statement-weighted,")
	fmt.Println("# rounded down to the nearest 0.5%. The floor only ever rises:")
	fmt.Println("# a PR that drops a package below its floor must add tests, or")
	fmt.Println("# lower the floor here with a documented reason in review.")
	fmt.Println("# On conflict, keep the higher floor for each package.")
	for _, pkg := range pkgs {
		// A package with no covered statements has nothing to ratchet (this also
		// drops test-helper and generated packages that report 0%).
		if isExcluded(pkg) || p.total[pkg] < minStmts || p.covered[pkg] == 0 {
			continue
		}
		pct, _ := p.pct(pkg)
		fmt.Printf("%s %.1f\n", pkg, floorDown(pct))
	}
}

type floor struct {
	pkg    string
	minPct float64
}

func readFloors(file string) ([]floor, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var floors []floor
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s: malformed floor line: %q", file, line)
		}
		minPct, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, fmt.Errorf("%s: bad floor value in %q: %w", file, line, err)
		}
		floors = append(floors, floor{pkg: fields[0], minPct: minPct})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(floors) == 0 {
		return nil, fmt.Errorf("%s: no floors defined", file)
	}
	return floors, nil
}

// check reports each package's status and returns true if any package failed.
func check(p *pkgCov, floors []floor) bool {
	failed := false
	for _, fl := range floors {
		pct, ok := p.pct(fl.pkg)
		switch {
		case !ok:
			fmt.Printf("FAIL: %s has no coverage data (floor %.1f%%)\n", fl.pkg, fl.minPct)
			failed = true
		case pct+1e-9 < fl.minPct:
			fmt.Printf("FAIL: %s coverage %.1f%% < floor %.1f%%\n", fl.pkg, pct, fl.minPct)
			failed = true
		default:
			fmt.Printf("OK:   %s coverage %.1f%% >= floor %.1f%%\n", fl.pkg, pct, fl.minPct)
		}
	}
	if failed {
		fmt.Println()
		fmt.Println("Coverage regressed below the ratchet floor. Either:")
		fmt.Println("  1. Add tests so coverage rises again, or")
		fmt.Println("  2. If intentional (e.g. removed dead code), lower the floor in")
		fmt.Println("     tools/coverage-floors.txt and justify it in PR review.")
	}
	return failed
}
