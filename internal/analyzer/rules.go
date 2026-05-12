package analyzer

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	appRules "darkweb-tracker/internal/rules"
)

// ---------------------------------------------------------------------------
// Interface: drop-in contract for both LLM and rules-based analyzers
// ---------------------------------------------------------------------------

// ItemAnalyzer is the interface implemented by both the LLM-based Analyzer
// and the rule-based RulesEngine.
type ItemAnalyzer interface {
	AnalyzeBatch(ctx context.Context, items []BatchItem, workers int) []BatchResult
}

var (
	_ ItemAnalyzer = (*Analyzer)(nil)
	_ ItemAnalyzer = (*RulesEngine)(nil)
)

// ---------------------------------------------------------------------------
// RulesEngine
// ---------------------------------------------------------------------------

// RulesEngine is a pure rule-based scoring engine driven by the loaded
// FilterRules configuration. Zero network calls, zero cost.
type RulesEngine struct {
	fr  *appRules.FilterRules
	log *slog.Logger
}

// NewRulesEngine creates a RulesEngine from the given rule set.
func NewRulesEngine(fr *appRules.FilterRules, log *slog.Logger) *RulesEngine {
	return &RulesEngine{fr: fr, log: log}
}

// ---------------------------------------------------------------------------
// Pre-compiled regex patterns (package level)
// ---------------------------------------------------------------------------

var (
	reBillions  = regexp.MustCompile(`(?i)\b(\d[\d,\.]*)\s*(billion|bln|b)\b`)
	reMillions  = regexp.MustCompile(`(?i)\b(\d[\d,\.]*)\s*(million|mln|m)\b`)
	reThousands = regexp.MustCompile(`(?i)\b(\d[\d,\.]*)\s*(k|thousand)\b`)
	reRecordUnits = regexp.MustCompile(`(?i)\b(\d[\d,\.]*)\s*(records?|rows?|accounts?|users?|emails?|lines?|entries|creds?|credentials?|passwords?|combos?)\b`)
)

// ---------------------------------------------------------------------------
// ScoreItem
// ---------------------------------------------------------------------------

// ScoreItem applies deterministic rules to produce an AnalysisResult.
// All pattern lists come from the loaded FilterRules, not hardcoded constants.
func (r *RulesEngine) ScoreItem(title, content, siteName string) *AnalysisResult {
	rc := r.fr.Rules
	text := strings.ToLower(title + " " + content)
	siteLower := strings.ToLower(siteName)

	// ── Vendor / spam pre-filter ─────────────────────────────────────────────
	for _, p := range rc.VendorPatterns {
		if strings.Contains(text, p) {
			return &AnalysisResult{
				Score:            2,
				Category:         "general_discussion",
				Tags:             []string{"vendor-ad", "low-value"},
				Summary:          fmt.Sprintf("%s论坛发现供应商广告或低价值帖，无实际泄露价值。", siteName),
				AffectedTargets:  []string{},
				EstimatedRecords: -1,
				DataTypes:        []string{},
				IsUrgent:         false,
				ConfidenceLevel:  "low",
				Reasoning:        fmt.Sprintf("Rule-based: vendor/spam pattern matched: %q", p),
			}
		}
	}

	// Step 1: Record count → base score
	estimatedRecords := extractMaxRecords(text)
	baseScore := recordsToScore(estimatedRecords)

	// Step 2: Data type detection (from config)
	detectedTypes := r.detectDataTypes(text)

	// Combo marker → hard-cap at 4 (aggregate, not a fresh breach)
	if containsStr(detectedTypes, "combo") {
		detectedTypes = removeStr(detectedTypes, "combo")
		score := minInt(baseScore, 4)
		return &AnalysisResult{
			Score:            score,
			Category:         "credential_leak",
			Tags:             append(detectedTypes, "combo-aggregate"),
			Summary:          fmt.Sprintf("%s论坛发现凭证组合包（combo list），为聚合数据，非新鲜泄露。", siteName),
			AffectedTargets:  []string{},
			EstimatedRecords: estimatedRecords,
			DataTypes:        detectedTypes,
			IsUrgent:         false,
			ConfidenceLevel:  "low",
			Reasoning:        fmt.Sprintf("Rule-based: combo aggregate detected, score capped at %d", score),
		}
	}

	dataTypeBoost := r.calcDataTypeBoost(detectedTypes)

	// Step 3: Freshness
	hasFreshPos, hasFreshNeg := r.detectFreshness(text)
	freshnessBoost := 0
	if hasFreshPos && !hasFreshNeg {
		freshnessBoost = 1
	} else if hasFreshNeg {
		freshnessBoost = -2
	}

	// Step 4: Site tier bonus
	siteBoost := 0
	for _, s := range rc.Tier1Sites {
		if strings.Contains(siteLower, s) {
			siteBoost = 1
			break
		}
	}

	// Step 5: Category
	category := r.classifyCategory(text, detectedTypes, siteLower)

	// Step 6: Final score
	finalScore := baseScore + dataTypeBoost + freshnessBoost + siteBoost
	if finalScore < 1 {
		finalScore = 1
	}
	if finalScore > 10 {
		finalScore = 10
	}

	isUrgent := finalScore >= 8 && !hasFreshNeg &&
		(hasFreshPos || estimatedRecords >= 100_000)

	confidenceLevel := "low"
	if estimatedRecords > 0 && len(detectedTypes) >= 2 {
		confidenceLevel = "high"
	} else if estimatedRecords > 0 || len(detectedTypes) >= 1 {
		confidenceLevel = "medium"
	}

	summary := r.buildSummary(siteName, category, estimatedRecords, detectedTypes, hasFreshPos, hasFreshNeg)
	tags := r.buildTags(detectedTypes, category, hasFreshPos, hasFreshNeg, siteLower)

	reasoning := fmt.Sprintf(
		"Rule-based: base=%d, datatype_boost=%d, freshness=%d, site=%d -> final=%d. Records=%d, types=%v",
		baseScore, dataTypeBoost, freshnessBoost, siteBoost, finalScore,
		estimatedRecords, detectedTypes,
	)

	return &AnalysisResult{
		Score:            finalScore,
		Category:         category,
		Tags:             tags,
		Summary:          summary,
		AffectedTargets:  []string{},
		EstimatedRecords: estimatedRecords,
		DataTypes:        detectedTypes,
		IsUrgent:         isUrgent,
		ConfidenceLevel:  confidenceLevel,
		Reasoning:        reasoning,
	}
}

// ---------------------------------------------------------------------------
// AnalyzeBatch
// ---------------------------------------------------------------------------

func (r *RulesEngine) AnalyzeBatch(ctx context.Context, items []BatchItem, workers int) []BatchResult {
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
				res := r.ScoreItem(item.Title, item.Content, item.SiteName)
				results <- BatchResult{ID: item.ID, Result: res}
			}
		}()
	}

	for _, item := range items {
		jobs <- item
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]BatchResult, 0, len(items))
	for br := range results {
		out = append(out, br)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	r.log.Debug("rules engine batch complete", "items", len(items), "results", len(out))
	return out
}

// ---------------------------------------------------------------------------
// Helpers driven by loaded config
// ---------------------------------------------------------------------------

func (r *RulesEngine) detectDataTypes(text string) []string {
	found := make([]string, 0)
	for typeName, keywords := range r.fr.Rules.DataTypes {
		for _, kw := range keywords {
			if strings.Contains(text, kw) {
				found = append(found, typeName)
				break
			}
		}
	}
	sort.Strings(found)
	return found
}

func (r *RulesEngine) calcDataTypeBoost(types []string) int {
	set := make(map[string]bool, len(types))
	for _, t := range types {
		set[t] = true
	}
	boost := 0
	if set["email"] && set["password"] {
		boost += 2
	}
	if set["credit_card"] || set["financial"] {
		boost += 2
	}
	if set["medical"] || set["government"] {
		boost += 2
	}
	if set["ssn"] || set["dob"] {
		boost++
	}
	if set["cookie"] {
		boost++
	}
	if boost > 3 {
		boost = 3
	}
	return boost
}

func (r *RulesEngine) detectFreshness(text string) (pos, neg bool) {
	for _, kw := range r.fr.Rules.FreshPositive {
		if strings.Contains(text, kw) {
			pos = true
			break
		}
	}
	for _, kw := range r.fr.Rules.FreshNegative {
		if strings.Contains(text, kw) {
			neg = true
			break
		}
	}
	return
}

func (r *RulesEngine) classifyCategory(text string, dataTypes []string, siteLower string) string {
	typeSet := make(map[string]bool, len(dataTypes))
	for _, t := range dataTypes {
		typeSet[t] = true
	}

	if typeSet["combo"] || strings.Contains(text, "combolist") || strings.Contains(text, "combo list") {
		return "credential_leak"
	}
	for _, kw := range []string{"database", "db dump", "sql dump", "backup", "table dump"} {
		if strings.Contains(text, kw) {
			return "database_dump"
		}
	}
	if typeSet["credit_card"] || typeSet["financial"] {
		return "financial_data"
	}
	if typeSet["medical"] {
		return "personal_info"
	}
	if typeSet["government"] {
		return "database_dump"
	}
	if typeSet["source_code"] {
		return "other"
	}
	for _, kw := range []string{"access", "rdp", "shell", "vpn access", "ssh", "admin panel"} {
		if strings.Contains(text, kw) {
			return "access_sale"
		}
	}
	if typeSet["email"] || typeSet["password"] {
		return "credential_leak"
	}
	if len(dataTypes) > 0 {
		return "database_dump"
	}
	for _, kw := range r.fr.Rules.RansomSites {
		if strings.Contains(text, kw) || strings.Contains(siteLower, kw) {
			return "database_dump"
		}
	}
	for _, kw := range []string{"tutorial", "guide", "how to", "discussion", "question", "help", "ask"} {
		if strings.Contains(text, kw) {
			return "general_discussion"
		}
	}
	return "other"
}

var categoryCN = map[string]string{
	"credential_leak":    "凭证泄露",
	"database_dump":      "数据库泄露",
	"financial_data":     "金融数据泄露",
	"personal_info":      "个人信息泄露",
	"access_sale":        "访问权限出售",
	"general_discussion": "一般讨论",
}

func (r *RulesEngine) buildSummary(siteName, category string, records int, dataTypes []string, freshPos, freshNeg bool) string {
	catCN, ok := categoryCN[category]
	if !ok {
		catCN = "数据泄露"
	}
	var recordsStr string
	switch {
	case records >= 1_000_000:
		recordsStr = fmt.Sprintf("涉及约%d百万条记录", records/1_000_000)
	case records >= 1_000:
		recordsStr = fmt.Sprintf("涉及约%d条记录", records)
	default:
		recordsStr = "规模不明"
	}
	typesStr := "未知"
	if len(dataTypes) > 0 {
		typesStr = strings.Join(dataTypes, "、")
	}
	var freshnessStr string
	if freshPos && !freshNeg {
		freshnessStr = "数据较新。"
	} else if freshNeg {
		freshnessStr = "疑为旧数据。"
	}
	if siteName == "" {
		siteName = "未知来源"
	}
	return fmt.Sprintf("%s论坛发现%s，%s，数据类型：%s。%s", siteName, catCN, recordsStr, typesStr, freshnessStr)
}

func (r *RulesEngine) buildTags(dataTypes []string, category string, freshPos, freshNeg bool, siteLower string) []string {
	tags := make([]string, 0, len(dataTypes)+4)
	tags = append(tags, dataTypes...)
	tags = append(tags, category)
	if freshPos {
		tags = append(tags, "fresh")
	}
	if freshNeg {
		tags = append(tags, "stale")
	}
	for _, s := range r.fr.Rules.Tier1Sites {
		if strings.Contains(siteLower, s) {
			tags = append(tags, "tier1-forum")
			break
		}
	}
	for _, s := range r.fr.Rules.Tier2Sites {
		if strings.Contains(siteLower, s) {
			tags = append(tags, "tier2-forum")
			break
		}
	}
	for _, s := range r.fr.Rules.RansomSites {
		if strings.Contains(siteLower, s) {
			tags = append(tags, "ransomware-site")
			break
		}
	}
	return tags
}

// ---------------------------------------------------------------------------
// Pure numeric helpers (no config dependency)
// ---------------------------------------------------------------------------

func parseNumber(s string) float64 {
	s = strings.ReplaceAll(s, ",", "")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func extractMaxRecords(text string) int {
	var maxVal float64
	for _, m := range reBillions.FindAllStringSubmatch(text, -1) {
		if v := parseNumber(m[1]) * 1_000_000_000; v > maxVal {
			maxVal = v
		}
	}
	for _, m := range reMillions.FindAllStringSubmatch(text, -1) {
		if v := parseNumber(m[1]) * 1_000_000; v > maxVal {
			maxVal = v
		}
	}
	for _, m := range reThousands.FindAllStringSubmatch(text, -1) {
		if v := parseNumber(m[1]) * 1_000; v > maxVal {
			maxVal = v
		}
	}
	for _, m := range reRecordUnits.FindAllStringSubmatch(text, -1) {
		if v := parseNumber(m[1]); v > maxVal {
			maxVal = v
		}
	}
	if maxVal <= 0 {
		return -1
	}
	if maxVal > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(maxVal)
}

func recordsToScore(records int) int {
	switch {
	case records >= 100_000_000:
		return 9
	case records >= 10_000_000:
		return 8
	case records >= 1_000_000:
		return 7
	case records >= 100_000:
		return 6
	case records >= 10_000:
		return 5
	case records >= 1_000:
		return 4
	case records > 0:
		return 3
	default:
		return 2
	}
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func removeStr(slice []string, s string) []string {
	out := slice[:0:0]
	for _, v := range slice {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
