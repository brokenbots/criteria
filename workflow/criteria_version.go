package workflow

import (
	"fmt"
	"strings"

	"github.com/blang/semver"
	"github.com/hashicorp/hcl/v2"

	"github.com/brokenbots/criteria/workflow/version"
)

// CriteriaVersionConstraint is a conjunction of semantic-version comparators.
// An empty constraint list means "any version" and Allow returns true.
type CriteriaVersionConstraint struct {
	ranges []criteriaRange
	raw    string
}

type criteriaRange struct {
	op  string
	ver semver.Version
}

var supportedOps = map[string]bool{
	"=":  true,
	"==": true,
	"!=": true,
	">":  true,
	">=": true,
	"<":  true,
	"<=": true,
}

// ParseCriteriaVersionConstraint parses a semantic-version constraint string.
//
// Supported forms:
//   - exact: "0.5.8" or "=0.5.8"
//   - lower bound: ">=0.5.8"
//   - upper bound: "<0.6.0"
//   - conjunction: ">=0.5.8, <0.6.0" (comma-separated, all must hold)
//
// Leading "v" on version numbers is accepted and stripped. Prerelease bounds
// are accepted; a stable lower bound does not satisfy a prerelease running
// version unless the constraint explicitly includes a prerelease bound (see
// Allow).
//
// Empty strings, unknown operators, and invalid versions return an error.
func ParseCriteriaVersionConstraint(s string) (CriteriaVersionConstraint, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return CriteriaVersionConstraint{}, fmt.Errorf("criteria_version constraint cannot be empty")
	}

	parts := strings.Split(s, ",")
	ranges := make([]criteriaRange, 0, len(parts))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return CriteriaVersionConstraint{}, fmt.Errorf("criteria_version constraint has an empty segment at position %d", i)
		}
		op, verStr := "=", part
		for _, candidate := range []string{">=", "<=", "!=", "==", ">", "<", "="} {
			if strings.HasPrefix(part, candidate) {
				op = candidate
				verStr = strings.TrimSpace(strings.TrimPrefix(part, candidate))
				break
			}
		}
		if !supportedOps[op] {
			return CriteriaVersionConstraint{}, fmt.Errorf("criteria_version constraint %q uses unsupported operator %q", part, op)
		}
		verStr = strings.TrimPrefix(verStr, "v")
		ver, err := semver.Parse(verStr)
		if err != nil {
			return CriteriaVersionConstraint{}, fmt.Errorf("criteria_version constraint %q contains invalid version %q: %w", part, verStr, err)
		}
		ranges = append(ranges, criteriaRange{op: op, ver: ver})
	}
	return CriteriaVersionConstraint{ranges: ranges, raw: s}, nil
}

// Allow reports whether v satisfies every comparator in the constraint.
//
// Per SemVer precedence, build metadata does not alter comparison. Prerelease
// versions do not satisfy a stable lower bound unless the constraint explicitly
// declares a prerelease lower bound that v also satisfies. This prevents a
// workflow with a stable minimum bound from silently running on an untested
// prerelease engine.
func (c CriteriaVersionConstraint) Allow(v semver.Version) bool {
	if len(c.ranges) == 0 {
		return true
	}

	for i := range c.ranges {
		if !c.ranges[i].satisfies(v) {
			return false
		}
	}

	// If the running version is a prerelease and any *satisfied* lower bound is
	// stable, require an explicit prerelease lower bound in the constraint that
	// this prerelease also satisfies. Example:
	//   - constraint ">=0.5.8" on engine 0.5.9-rc1 -> false.
	//   - constraint ">=0.5.8, >=0.5.9-rc1" on engine 0.5.9-rc1 -> true.
	if len(v.Pre) > 0 && c.stableLowerRejectsPrerelease(v) {
		return false
	}

	return true
}

// ExampleVersion returns a concrete stable semantic version that satisfies the
// constraint, used to recommend an explicit value for development builds. It
// prefers lower bounds, then exact bounds, then upper bounds. If no stable
// version can be derived it returns 0.0.0.
func (c CriteriaVersionConstraint) ExampleVersion() semver.Version {
	for _, r := range c.ranges {
		if (r.op == ">" || r.op == ">=") && len(r.ver.Pre) == 0 {
			return r.ver
		}
	}
	for _, r := range c.ranges {
		if (r.op == "=" || r.op == "==") && len(r.ver.Pre) == 0 {
			return r.ver
		}
	}
	for _, r := range c.ranges {
		if r.op == "<=" && len(r.ver.Pre) == 0 {
			return r.ver
		}
		if r.op == "<" && len(r.ver.Pre) == 0 {
			return semver.Version{}
		}
	}
	return semver.Version{}
}

// stableLowerRejectsPrerelease reports whether a satisfied stable lower bound
// exists but no satisfied prerelease lower bound does. When true, a prerelease
// running engine must be rejected.
func (c CriteriaVersionConstraint) stableLowerRejectsPrerelease(v semver.Version) bool {
	foundStable := false
	for i := range c.ranges {
		r := &c.ranges[i]
		if r.op != ">" && r.op != ">=" {
			continue
		}
		if len(r.ver.Pre) > 0 && r.satisfies(v) {
			return false
		}
		if len(r.ver.Pre) == 0 && r.satisfies(v) {
			foundStable = true
		}
	}
	return foundStable
}

func (r *criteriaRange) satisfies(v semver.Version) bool {
	switch r.op {
	case "=", "==":
		return v.Compare(r.ver) == 0
	case "!=":
		return v.Compare(r.ver) != 0
	case ">":
		return v.Compare(r.ver) > 0
	case ">=":
		return v.Compare(r.ver) >= 0
	case "<":
		return v.Compare(r.ver) < 0
	case "<=":
		return v.Compare(r.ver) <= 0
	default:
		return false
	}
}

// CheckGraphCriteriaVersion returns diagnostics if the compiled workflow's
// declared engine constraint is not satisfied by the running engine. The name
// chain identifies ancestor workflow names for nested subworkflow diagnostics.
func CheckGraphCriteriaVersion(g *FSMGraph, nameChain []string) hcl.Diagnostics {
	if g == nil {
		return nil
	}
	return checkCriteriaVersion(g.Name, g.CriteriaVersion, g.CriteriaVersionRange, nameChain)
}

// CheckSpecCriteriaVersion returns diagnostics if the parsed workflow's declared
// engine constraint is not satisfied by the running engine.
func CheckSpecCriteriaVersion(spec *Spec, nameChain []string) hcl.Diagnostics {
	if spec == nil || spec.Header == nil {
		return nil
	}
	return checkCriteriaVersion(spec.Header.Name, spec.Header.CriteriaVersion, spec.Header.CriteriaVersionRange, nameChain)
}

func checkCriteriaVersion(name, constraint string, r *hcl.Range, nameChain []string) hcl.Diagnostics {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" {
		return nil
	}

	engine := version.Current()
	if !engine.Known {
		return hcl.Diagnostics{unknownVersionDiag(name, constraint, nameChain, r)}
	}

	parsed, err := ParseCriteriaVersionConstraint(constraint)
	if err != nil {
		return hcl.Diagnostics{invalidConstraintDiag(name, constraint, err, r)}
	}

	if !parsed.Allow(engine.Version) {
		return hcl.Diagnostics{mismatchDiag(name, constraint, engine.Display, nameChain, r)}
	}
	return nil
}

func mismatchDiag(name, constraint, display string, chain []string, r *hcl.Range) *hcl.Diagnostic {
	label := "workflow"
	if len(chain) > 0 {
		label = "subworkflow"
	}
	d := &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("%s %q requires Criteria %s; running engine is %s", label, name, constraint, display),
	}
	if len(chain) > 0 {
		d.Detail = fmt.Sprintf("required by %s", strings.Join(append(chain, name), " -> "))
	}
	if r != nil {
		d.Subject = r
	}
	return d
}

func unknownVersionDiag(name, constraint string, chain []string, r *hcl.Range) *hcl.Diagnostic {
	label := "workflow"
	if len(chain) > 0 {
		label = "subworkflow"
	}

	example := semver.Version{Major: 0, Minor: 0, Patch: 0}
	if parsed, err := ParseCriteriaVersionConstraint(constraint); err == nil {
		example = parsed.ExampleVersion()
	}
	exampleStr := "v" + example.String()

	d := &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("%s %q requires Criteria %s; running engine version is unknown", label, name, constraint),
		Detail: fmt.Sprintf(
			"The Criteria binary did not embed a parseable semantic version. "+
				"For development and testing, set %s to an explicit semver such as %q, "+
				"or build a release binary with %q.",
			version.OverrideEnv(), exampleStr, version.LdflagsExample(exampleStr),
		),
	}
	if len(chain) > 0 {
		d.Detail += fmt.Sprintf("\nrequired by %s", strings.Join(append(chain, name), " -> "))
	}
	if r != nil {
		d.Subject = r
	}
	return d
}

func invalidConstraintDiag(name, constraint string, err error, r *hcl.Range) *hcl.Diagnostic {
	d := &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("workflow %q declares invalid criteria_version %q", name, constraint),
		Detail:   err.Error(),
	}
	if r != nil {
		d.Subject = r
	}
	return d
}
