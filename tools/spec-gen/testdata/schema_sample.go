// Package testdata provides sample schema types for spec-gen unit tests.
// This file intentionally mirrors the shape of workflow/schema.go so the
// extractor can be exercised without loading the full workflow package.
package testdata

import "github.com/hashicorp/hcl/v2"

// Spec is the root spec used in tests.
type Spec struct {
	Widgets []*WidgetSpec `hcl:"widget,block"`
	Rules   []*RuleSpec   `hcl:"rule,block"`
}

// WidgetSpec defines a widget block.
type WidgetSpec struct {
	// Name of the widget instance.
	Name    string `hcl:"name,label"`
	// Title is the display label shown in the UI.
	Title   string `hcl:"title,attr"`
	// Enabled controls whether the widget is active.
	Enabled *bool  `hcl:"enabled,optional"`
	Remain  hcl.Body `hcl:",remain"`
}

// RuleSpec defines a rule block.
type RuleSpec struct {
	// ID uniquely identifies this rule.
	ID       string `hcl:"id,label"`
	// Priority sets the evaluation order; higher values run first.
	Priority int    `hcl:"priority,attr"`
}
