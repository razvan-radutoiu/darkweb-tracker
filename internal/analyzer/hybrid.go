// Package analyzer — HybridAnalyzer chains multiple ItemAnalyzers.
//
// Strategy: try each provider in order; on per-item failure fall back to the
// next one; the last entry is always the RulesEngine (zero cost, never fails).
//
// Anti-卡死 design:
//   - Each LLM call already has perRequestTimeout (8s) in analyzer.go.
//   - If a provider returns a 429 the Analyzer retries up to maxRetries with
//     the correct retry-after wait. If it still fails, HybridAnalyzer moves on.
//   - The RulesEngine fallback ensures every item always gets a result.
package analyzer

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
)

// HybridAnalyzer tries providers[0], [1], … in order per item.
// The last provider MUST be a RulesEngine (always succeeds).
type HybridAnalyzer struct {
	providers []namedAnalyzer
	log       *slog.Logger
}

type namedAnalyzer struct {
	name     string
	analyzer ItemAnalyzer
	// consecutive failures tracked for circuit-breaker-lite
	failures atomic.Int32
}

// NewHybridAnalyzer builds a HybridAnalyzer from an ordered list of providers.
// providers[last] should be a *RulesEngine.
func NewHybridAnalyzer(log *slog.Logger, providers ...struct {
	Name     string
	Analyzer ItemAnalyzer
}) *HybridAnalyzer {
	named := make([]namedAnalyzer, len(providers))
	for i, p := range providers {
		named[i] = namedAnalyzer{name: p.Name, analyzer: p.Analyzer}
	}
	return &HybridAnalyzer{providers: named, log: log}
}

// AnalyzeBatch satisfies ItemAnalyzer. Each item is processed independently:
// provider[0] is tried first; on error the next provider is tried; and so on.
func (h *HybridAnalyzer) AnalyzeBatch(ctx context.Context, items []BatchItem, workers int) []BatchResult {
	if workers <= 0 {
		workers = 3
	}
	if len(items) == 0 {
		return nil
	}

	jobs := make(chan BatchItem, len(items))
	results := make(chan BatchResult, len(items))

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				results <- h.analyzeOne(ctx, item)
			}
		}()
	}

	for _, item := range items {
		jobs <- item
	}
	close(jobs)

	go func() { wg.Wait(); close(results) }()

	out := make([]BatchResult, 0, len(items))
	for r := range results {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// analyzeOne sends a single item through the provider chain.
func (h *HybridAnalyzer) analyzeOne(ctx context.Context, item BatchItem) BatchResult {
	for i := range h.providers {
		p := &h.providers[i]

		// Skip providers that have failed too many times in a row (soft circuit breaker).
		// Allow the last provider (rules engine) through unconditionally.
		if i < len(h.providers)-1 && p.failures.Load() >= 10 {
			h.log.Debug("hybrid: skipping provider (circuit open)",
				"provider", p.name, "failures", p.failures.Load())
			continue
		}

		// Wrap in a single-item batch call.
		results := p.analyzer.AnalyzeBatch(ctx, []BatchItem{item}, 1)
		if len(results) == 0 || results[0].Err != nil {
			err := results[0].Err
			if len(results) == 0 {
				err = fmt.Errorf("empty result from provider %s", p.name)
			}
			p.failures.Add(1)
			h.log.Warn("hybrid: provider failed, trying next",
				"provider", p.name,
				"item_id", item.ID,
				"err", err,
			)
			continue
		}

		// Success — reset failure counter.
		p.failures.Store(0)
		if i > 0 {
			h.log.Debug("hybrid: used fallback provider",
				"provider", p.name,
				"item_id", item.ID,
			)
		}
		return results[0]
	}

	// Should never reach here if the last provider is always RulesEngine.
	// Emergency fallback: return a generic low-score result.
	return BatchResult{
		ID: item.ID,
		Result: &AnalysisResult{
			Score:            1,
			Category:         "other",
			Tags:             []string{"fallback"},
			Summary:          "所有分析提供者均失败，返回默认低分结果。",
			AffectedTargets:  []string{},
			EstimatedRecords: -1,
			IsUrgent:         false,
			ConfidenceLevel:  "low",
			Reasoning:        "HybridAnalyzer: all providers exhausted",
		},
	}
}
