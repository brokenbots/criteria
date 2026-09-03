// Command coverage-floor-guard prevents autonomous coverage-floor ratchet
// relaxations (CRI-93). It compares two versions of the per-package coverage
// floor file and fails if any package's numeric floor decreased, unless the
// caller passes --approved to signal explicit human approval.
//
// The tool does not compute coverage; it only guards the committed floor file.
//
// Usage:
//
//	coverage-floor-guard --base FILE --head FILE [--approved]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

const help = `Usage: coverage-floor-guard --base FILE --head FILE [--approved]

Compare two coverage floor files and fail if any package's floor value
decreased. Floor increases are always allowed. When --approved is set, a
decrease is reported but does not fail the process.
`

type floor struct {
	pkg    string
	minPct float64
}

func main() {
	base := flag.String("base", "", "base floor file path")
	head := flag.String("head", "", "head floor file path")
	approved := flag.Bool("approved", false, "allow floor decreases (human approval present)")
	flag.Usage = func() { fmt.Fprint(os.Stderr, help) }
	flag.Parse()

	if *base == "" || *head == "" {
		flag.Usage()
		os.Exit(2)
	}

	baseFloors, err := readFloors(*base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR reading base floors %q: %v\n", *base, err)
		os.Exit(2)
	}
	headFloors, err := readFloors(*head)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR reading head floors %q: %v\n", *head, err)
		os.Exit(2)
	}

	result := compareFloors(baseFloors, headFloors)
	printResult(result, *approved)

	if len(result.decreases) > 0 && !*approved {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "A coverage floor may not be lowered without human approval.")
		fmt.Fprintln(os.Stderr, "If the decrease is intentional, add the 'floor-change-approved' label to this PR and document the reason in review.")
		os.Exit(1)
	}
}

// readFloors parses a coverage-floors.txt file into a slice of floors.
// Comment lines starting with '#' and blank lines are ignored. Each floor
// line must contain a package path and a numeric value separated by whitespace.
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
			return nil, fmt.Errorf("malformed floor line: %q", line)
		}
		minPct, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, fmt.Errorf("bad floor value %q in line %q: %w", fields[1], line, err)
		}
		floors = append(floors, floor{pkg: fields[0], minPct: minPct})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return floors, nil
}

type floorChange struct {
	pkg  string
	base float64
	head float64
}

type compareResult struct {
	decreases []floorChange
	increases []floorChange
	added     []string
	removed   []string
}

// compareFloors compares base and head floor files and returns the set of
// changes. Only packages present in both files can produce a numeric change;
// additions and removals are tracked separately and do not fail the guard.
func compareFloors(base, head []floor) compareResult {
	baseMap := make(map[string]float64, len(base))
	for _, f := range base {
		baseMap[f.pkg] = f.minPct
	}
	headMap := make(map[string]float64, len(head))
	for _, f := range head {
		headMap[f.pkg] = f.minPct
	}

	var result compareResult

	// Detect decreases and increases using base as the authority. Packages only
	// in head are additions; packages only in base are removals.
	for _, f := range base {
		headPct, ok := headMap[f.pkg]
		if !ok {
			result.removed = append(result.removed, f.pkg)
			continue
		}
		switch {
		case headPct+1e-9 < f.minPct:
			result.decreases = append(result.decreases, floorChange{pkg: f.pkg, base: f.minPct, head: headPct})
		case headPct > f.minPct+1e-9:
			result.increases = append(result.increases, floorChange{pkg: f.pkg, base: f.minPct, head: headPct})
		}
	}
	for _, f := range head {
		if _, ok := baseMap[f.pkg]; !ok {
			result.added = append(result.added, f.pkg)
		}
	}

	sort.Slice(result.decreases, func(i, j int) bool { return result.decreases[i].pkg < result.decreases[j].pkg })
	sort.Slice(result.increases, func(i, j int) bool { return result.increases[i].pkg < result.increases[j].pkg })
	sort.Strings(result.added)
	sort.Strings(result.removed)

	return result
}

func printResult(r compareResult, approved bool) {
	for _, d := range r.decreases {
		if approved {
			fmt.Printf("ALLOWED: %s floor lowered from %.1f to %.1f (human approval present)\n", d.pkg, d.base, d.head)
		} else {
			fmt.Printf("FAIL: %s floor lowered from %.1f to %.1f\n", d.pkg, d.base, d.head)
		}
	}
	for _, i := range r.increases {
		fmt.Printf("OK: %s floor raised from %.1f to %.1f\n", i.pkg, i.base, i.head)
	}
	for _, p := range r.added {
		fmt.Printf("OK: %s added to floors\n", p)
	}
	for _, p := range r.removed {
		fmt.Printf("OK: %s removed from floors\n", p)
	}
	if len(r.decreases) == 0 && len(r.increases) == 0 && len(r.added) == 0 && len(r.removed) == 0 {
		fmt.Println("OK: no floor changes")
	}
}
