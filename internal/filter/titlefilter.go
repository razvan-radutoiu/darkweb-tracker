// Package filter provides fast title-based pre-filtering of dark web forum posts
// before they reach the LLM analysis pipeline.
//
// All pattern lists are loaded from filter_rules.yaml at startup via the rules
// package — no recompile needed to add/remove patterns.
package filter

import (
	"strings"

	"darkweb-tracker/internal/rules"
)

// Action indicates what to do with a post after title analysis.
type Action int

const (
	// ActionAnalyze — send to LLM, push if score >= threshold.
	ActionAnalyze Action = iota
	// ActionSkip — store in DB (for future dedup) but skip LLM and push.
	ActionSkip
)

// Result is the output of QuickFilter.
type Result struct {
	Action Action
	Reason string // human-readable explanation for logging
}

// Filter holds compiled rule sets loaded at startup.
// Create once via New() and reuse across all calls.
type Filter struct {
	fr *rules.FilterRules
}

// New creates a Filter from the given rule set.
func New(fr *rules.FilterRules) *Filter {
	return &Filter{fr: fr}
}

// QuickFilter classifies a post title without calling any external service.
// It takes O(n) time in title length — essentially free compared to an LLM call.
//
// Logic:
//  1. If title matches a named high-value target → Analyze (named target wins).
//  2. If title matches a skip pattern → Skip.
//  3. Otherwise → Analyze (let LLM decide).
func (f *Filter) QuickFilter(title, _ string) Result {
	low := strings.ToLower(title)

	// Phase 1: named high-value targets always pass through.
	for _, kw := range f.fr.NamedTargets {
		if strings.Contains(low, kw) {
			return Result{Action: ActionAnalyze, Reason: "named target detected"}
		}
	}

	// Phase 2: skip known low-value patterns.
	for _, p := range f.fr.SkipPatterns {
		if strings.Contains(low, p.Keyword) {
			return Result{Action: ActionSkip, Reason: p.Reason}
		}
	}

	// Phase 3: default — pass to analyzer.
	return Result{Action: ActionAnalyze, Reason: "passed default"}
}

// QuickFilter is a package-level convenience wrapper using a provided Filter.
// Kept for call-site compatibility; prefer Filter.QuickFilter when possible.
func QuickFilter(title, siteName string, f *Filter) Result {
	return f.QuickFilter(title, siteName)
}
