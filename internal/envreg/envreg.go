// Package envreg is the single registry of every CRITERIA_* environment
// variable production code reads: name, value kind, default, owning
// consumer surface, and one-line operator documentation. The generated
// variable table in docs/env-vars.md is rendered from this registry and a
// package test drift-checks the two.
//
// Two postures are supported. Typed accessors (String, StringOr) serve the
// common "read one known name" path; name-based reads must go through
// Lookup, which is the single audit point — reading an unregistered name
// panics at call time instead of silently widening the documented surface.
// Consumers keep their own value parsing so the strict/lenient override
// posture stays at the site that owns the failure mode.
//
// The registry deliberately carries no runner-era phone-home variables
// (that era was retired with the runner); a repo-wide sweep test in this
// package fails if a raw CRITERIA_* read appears outside this package.
package envreg

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Kind classifies the documented value shape of one registered variable.
// It is documentation metadata: consumers own parsing so malformed values
// fail at the site that knows the posture (strict abort vs lenient
// default).
type Kind string

const (
	KindBool     Kind = "bool"
	KindString   Kind = "string"
	KindEnum     Kind = "enum"
	KindInt      Kind = "int"
	KindSize     Kind = "size"
	KindDuration Kind = "duration"
)

// Valid reports whether k is one of the declared kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindBool, KindString, KindEnum, KindInt, KindSize, KindDuration:
		return true
	}
	return false
}

// Var is one registry row. Fields are unexported so every entry is a
// declared package variable in this package — callers can only reference
// registered names, never synthesize new ones.
type Var struct {
	name   string
	kind   Kind
	def    string
	scope  string
	doc    string
	prefix bool
	hidden bool
}

// Env returns the full environment variable name ("CRITERIA_...").
func (v *Var) Env() string { return v.name }

// Default returns the built-in default as rendered in the docs table;
// an empty default means the variable is unset by default.
func (v *Var) Default() string { return v.def }

// Kind returns the variable's documented value shape.
func (v *Var) Kind() Kind { return v.kind }

// Scope returns the owning consumer surface (package or command).
func (v *Var) Scope() string { return v.scope }

// Doc returns the one-line description rendered in the docs table.
func (v *Var) Doc() string { return v.doc }

// Prefix reports whether the row is a family variable whose member names
// are composed as Name + "_" + a caller-supplied suffix (for example
// CRITERIA_APPROVAL_<NODE>).
func (v *Var) Prefix() bool { return v.prefix }

// Hidden reports whether the row is excluded from the generated docs
// table: engine-set protocol names or test-harness gates an operator does
// not set by hand. Hidden rows stay registered and audited.
func (v *Var) Hidden() bool { return v.hidden }

// String returns the raw, untrimmed value from the process environment.
func (v *Var) String() string { return os.Getenv(v.name) }

// StringOr returns the raw value, falling back when unset or empty.
func (v *Var) StringOr(fallback string) string {
	if s := os.Getenv(v.name); s != "" {
		return s
	}
	return fallback
}

// FamilyPrefix returns the name prefix shared by every family member
// (Name + "_"); only valid on prefix rows.
func (v *Var) FamilyPrefix() string {
	v.mustBePrefix("FamilyPrefix")
	return v.name + "_"
}

// SuffixName composes the family variable's full name for one suffix (for
// example CRITERIA_APPROVAL + "MY_NODE"); only valid on prefix rows.
func (v *Var) SuffixName(suffix string) string {
	v.mustBePrefix("SuffixName")
	return v.name + "_" + suffix
}

// LookupSuffix reads one family member by suffix through Lookup, keeping
// the audit point in force for composed names; only valid on prefix rows.
func (v *Var) LookupSuffix(suffix string) string {
	return Lookup(v.SuffixName(suffix))
}

func (v *Var) mustBePrefix(op string) {
	if !v.prefix {
		panic(fmt.Sprintf("envreg: %s is not a prefix family variable: %s", v.name, op))
	}
}

// names maps every exact registered name to its row; prefixes indexes the
// family rows for suffix matching. Both are read-only after init.
var (
	names    = map[string]Var{}
	prefixes []Var
)

// Lookup reads one CRITERIA_* variable by full name through the registry's
// single audit point: a read of an unregistered name panics at call time,
// so name-based lookups cannot silently widen the documented surface. Use
// the typed accessors (String, StringOr) when the row is known statically;
// use Lookup when the name arrives as data (the tunables lookup function,
// test harnesses).
func Lookup(name string) string {
	v, _ := LookupOK(name)
	return v
}

// LookupOK resolves name through the audit point with the os.LookupEnv
// shape for consumers whose contract distinguishes "set" from "empty"
// (boot gate identity probes, value-injection helpers). It panics on an
// unregistered name exactly like Lookup.
func LookupOK(name string) (string, bool) {
	if !Known(name) {
		panic("envreg: read of unregistered environment variable " + name +
			": add it to internal/envreg/registry.go")
	}
	return os.LookupEnv(name)
}

// Known reports whether name is registered: either an exact row or a
// member of a registered prefix family.
func Known(name string) bool {
	if _, ok := names[name]; ok {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p.FamilyPrefix()) {
			return true
		}
	}
	return false
}

// All returns every registered variable — hidden rows included — sorted
// by name.
func All() []Var {
	out := make([]Var, 0, len(names))
	for _, v := range names {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Documented returns the variables rendered in the generated docs table:
// every registered row except hidden ones, sorted by name.
func Documented() []Var {
	all := All()
	out := make([]Var, 0, len(all))
	for _, v := range all {
		if !v.hidden {
			out = append(out, v)
		}
	}
	return out
}

func init() {
	for _, v := range registry {
		if v.name == "" || !v.kind.Valid() || v.scope == "" || v.doc == "" {
			panic("envreg: incomplete registry entry " + v.name)
		}
		if _, dup := names[v.name]; dup {
			panic("envreg: duplicate registration " + v.name)
		}
		names[v.name] = *v
		if v.prefix {
			prefixes = append(prefixes, *v)
		}
	}
}
