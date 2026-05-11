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
)

// ---------------------------------------------------------------------------
// Interface: drop-in contract for both LLM and rules-based analyzers
// ---------------------------------------------------------------------------

// ItemAnalyzer is the interface implemented by both the LLM-based Analyzer
// and the rule-based RulesEngine. The scheduler depends on this interface
// so either implementation can be used as a drop-in replacement.
type ItemAnalyzer interface {
	AnalyzeBatch(ctx context.Context, items []BatchItem, workers int) []BatchResult
}

// Compile-time interface satisfaction checks.
var (
	_ ItemAnalyzer = (*Analyzer)(nil)
	_ ItemAnalyzer = (*RulesEngine)(nil)
)

// ---------------------------------------------------------------------------
// RulesEngine: pure deterministic scoring — no network, no dependencies
// ---------------------------------------------------------------------------

// RulesEngine is a pure rule-based scoring engine that produces AnalysisResult
// identical to the LLM analyzer output. Zero network calls, zero cost.
type RulesEngine struct {
	log *slog.Logger
}

// NewRulesEngine creates a RulesEngine with the given logger.
func NewRulesEngine(log *slog.Logger) *RulesEngine {
	return &RulesEngine{log: log}
}

// ---------------------------------------------------------------------------
// Pre-compiled regex patterns (package level, not per-call)
// ---------------------------------------------------------------------------

var (
	// Number with multiplier suffix: "2.5M", "100 million", "1,000k"
	reBillions  = regexp.MustCompile(`(?i)\b(\d[\d,\.]*)\s*(billion|bln|b)\b`)
	reMillions  = regexp.MustCompile(`(?i)\b(\d[\d,\.]*)\s*(million|mln|m)\b`)
	reThousands = regexp.MustCompile(`(?i)\b(\d[\d,\.]*)\s*(k|thousand)\b`)

	// Raw number followed by record-type unit word
	reRecordUnits = regexp.MustCompile(`(?i)\b(\d[\d,\.]*)\s*(records?|rows?|accounts?|users?|emails?|lines?|entries|creds?|credentials?|passwords?|combos?)\b`)
)

// ---------------------------------------------------------------------------
// Keyword tables
// ---------------------------------------------------------------------------

var dataTypeKeywords = map[string][]string{
	"email":       {"email", "e-mail", "mail"},
	"password":    {"password", "passwd", "pass", "pwd", "hash", "md5", "sha1", "bcrypt", "ntlm"},
	"phone":       {"phone", "mobile", "tel", "gsm", "sms"},
	"address":     {"address", "addr", "location", "city", "zip", "postal"},
	"credit_card": {"credit card", "cc dump", "cvv", "cvc", "fullz", "card number"},
	"ssn":         {"ssn", "social security", "sin ", "national id"},
	"dob":         {"dob", "date of birth", "birthday", "birthdate"},
	"ip":          {"ip address", "ip log", "ipv4", "ipv6"},
	"cookie":      {"cookie", "session token", "auth token", "session cookie"},
	"source_code": {"source code", "github", "gitlab", "repo", "repository"},
	"financial":   {"bank", "iban", "swift", "routing", "account number", "balance", "transaction"},
	"medical":     {"medical", "health", "patient", "diagnosis", "prescription", "ssn", "hipaa"},
	"government":  {"government", "gov ", ".gov", "military", "dod ", "nsa ", "fbi ", "cia "},
	"combo":       {"combo", "combolist", "combo list", "mail:pass", "email:pass", "user:pass", "login:pass"},
}

var freshPositive = []string{"fresh", "new", "latest", "2024", "2025", "2026", "today", "just", "hot", "0day"}
var freshNegative = []string{"old", "2020", "2019", "2018", "2017", "2016", "2015", "leaked in 20", "from 20"}

var tier1Sites = []string{"breachforum", "leakbase", "thejavasea", "exploit-in", "xss-is"}
var tier2Sites = []string{"probiv", "darkforum", "altenens", "nulled", "leakforum", "mipped", "hard-tm"}
var ransomSites = []string{"ransomware", "ransom", "lockbit", "alphv", "blackcat"}

var categoryCN = map[string]string{
	"credential_leak":    "凭证泄露",
	"database_dump":      "数据库泄露",
	"financial_data":     "金融数据泄露",
	"personal_info":      "个人信息泄露",
	"access_sale":        "访问权限出售",
	"general_discussion": "一般讨论",
}

// ---------------------------------------------------------------------------
// ScoreItem — standalone scoring function, exported for testing
// ---------------------------------------------------------------------------

// ScoreItem applies deterministic rules to title+content+siteName and returns
// an AnalysisResult identical in shape to the LLM analyzer output.
func ScoreItem(title, content, siteName string) *AnalysisResult {
	text := strings.ToLower(title + " " + content)
	siteLower := strings.ToLower(siteName)

	// Step 1: Record count extraction -> base score
	estimatedRecords := extractMaxRecords(text)
	baseScore := recordsToScore(estimatedRecords)

	// Step 2: Data type detection -> score boost & DataTypes list
	detectedTypes := detectDataTypes(text)
	dataTypeBoost := calcDataTypeBoost(detectedTypes)

	// Step 3: Freshness signals -> score boost
	hasFreshPos, hasFreshNeg := detectFreshness(text)
	freshnessBoost := 0
	if hasFreshPos && !hasFreshNeg {
		freshnessBoost = 1
	} else if hasFreshNeg {
		freshnessBoost = -2
	}

	// Step 4: Site tier bonus
	siteBoost := 0
	for _, s := range tier1Sites {
		if strings.Contains(siteLower, s) {
			siteBoost = 1
			break
		}
	}

	// Step 5: Category classification
	category := classifyCategory(text, detectedTypes, siteLower)

	// Step 6: Clamp and finalize score
	finalScore := baseScore + dataTypeBoost + freshnessBoost + siteBoost
	if finalScore < 1 {
		finalScore = 1
	}
	if finalScore > 10 {
		finalScore = 10
	}

	// isUrgent: score>=8 AND no stale signals AND (fresh OR large dataset)
	isUrgent := finalScore >= 8 && !hasFreshNeg &&
		(hasFreshPos || estimatedRecords >= 100_000)

	// confidenceLevel
	confidenceLevel := "low"
	if estimatedRecords > 0 && len(detectedTypes) >= 2 {
		confidenceLevel = "high"
	} else if estimatedRecords > 0 || len(detectedTypes) >= 1 {
		confidenceLevel = "medium"
	}

	// Step 7: Template-based Chinese summary
	summary := buildRulesSummary(siteName, category, estimatedRecords, detectedTypes, hasFreshPos, hasFreshNeg)

	// Step 8: Tags
	tags := buildRulesTags(detectedTypes, category, hasFreshPos, hasFreshNeg, siteLower)

	// Reasoning (transparent scoring breakdown)
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
// AnalyzeBatch — identical signature to Analyzer.AnalyzeBatch
// ---------------------------------------------------------------------------

// AnalyzeBatch processes items concurrently using a worker pool.
// Results are returned sorted by BatchItem.ID for deterministic ordering.
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
				res := ScoreItem(item.Title, item.Content, item.SiteName)
				results <- BatchResult{
					ID:     item.ID,
					Result: res,
				}
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

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})

	r.log.Debug("rules engine batch complete", "items", len(items), "results", len(out))
	return out
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// parseNumber handles commas and decimals: "1,000,000" -> 1000000, "2.5" -> 2.5
func parseNumber(s string) float64 {
	s = strings.ReplaceAll(s, ",", "")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// extractMaxRecords finds the maximum record count from text using all patterns.
// Returns -1 when no numeric record indicators are found.
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

// recordsToScore converts estimated record count to base score.
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
	default: // -1 or 0 — no number found
		return 2
	}
}

// detectDataTypes scans text for keyword groups and returns matched type names.
func detectDataTypes(text string) []string {
	found := make([]string, 0)
	for typeName, keywords := range dataTypeKeywords {
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

// calcDataTypeBoost computes additive score boost from detected data types.
// Individual rules are additive but the total is capped at +3.
func calcDataTypeBoost(types []string) int {
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
		boost += 1
	}
	if set["combo"] {
		boost += 1
	}
	if set["cookie"] {
		boost += 1
	}

	if boost > 3 {
		boost = 3
	}
	return boost
}

// detectFreshness scans for freshness indicator keywords.
func detectFreshness(text string) (positive, negative bool) {
	for _, kw := range freshPositive {
		if strings.Contains(text, kw) {
			positive = true
			break
		}
	}
	for _, kw := range freshNegative {
		if strings.Contains(text, kw) {
			negative = true
			break
		}
	}
	return
}

// classifyCategory assigns primary category label using priority-ordered rules.
func classifyCategory(text string, dataTypes []string, siteLower string) string {
	typeSet := make(map[string]bool, len(dataTypes))
	for _, t := range dataTypes {
		typeSet[t] = true
	}

	// combo in dataTypes OR combolist/combo list in text
	if typeSet["combo"] || strings.Contains(text, "combolist") || strings.Contains(text, "combo list") {
		return "credential_leak"
	}

	// database/db dump/sql dump/backup/table dump
	for _, kw := range []string{"database", "db dump", "sql dump", "backup", "table dump"} {
		if strings.Contains(text, kw) {
			return "database_dump"
		}
	}

	// credit_card/financial in dataTypes
	if typeSet["credit_card"] || typeSet["financial"] {
		return "financial_data"
	}

	// medical in dataTypes
	if typeSet["medical"] {
		return "personal_info"
	}

	// government in dataTypes
	if typeSet["government"] {
		return "database_dump"
	}

	// source_code in dataTypes
	if typeSet["source_code"] {
		return "other"
	}

	// access/rdp/shell/vpn access/ssh/admin panel
	for _, kw := range []string{"access", "rdp", "shell", "vpn access", "ssh", "admin panel"} {
		if strings.Contains(text, kw) {
			return "access_sale"
		}
	}

	// has email OR password
	if typeSet["email"] || typeSet["password"] {
		return "credential_leak"
	}

	// has any dataType
	if len(dataTypes) > 0 {
		return "database_dump"
	}

	// ransomware keywords (site name or text)
	for _, kw := range ransomSites {
		if strings.Contains(text, kw) || strings.Contains(siteLower, kw) {
			return "database_dump"
		}
	}
	for _, kw := range []string{"ransomware", "ransom group", "victim", "extortion"} {
		if strings.Contains(text, kw) {
			return "database_dump"
		}
	}

	// general discussion / tutorial
	for _, kw := range []string{"tutorial", "guide", "how to", "discussion", "question", "help", "ask"} {
		if strings.Contains(text, kw) {
			return "general_discussion"
		}
	}

	return "other"
}

// buildRulesSummary generates a Chinese-language summary from extracted signals.
func buildRulesSummary(siteName, category string, records int, dataTypes []string, freshPos, freshNeg bool) string {
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

	site := siteName
	if site == "" {
		site = "未知来源"
	}

	return fmt.Sprintf("%s论坛发现%s，%s，数据类型：%s。%s",
		site, catCN, recordsStr, typesStr, freshnessStr)
}

// buildRulesTags collects all relevant tags for the analysis result.
func buildRulesTags(dataTypes []string, category string, freshPos, freshNeg bool, siteLower string) []string {
	tags := make([]string, 0, len(dataTypes)+4)

	// All detected data type keys
	tags = append(tags, dataTypes...)

	// Category
	tags = append(tags, category)

	// Freshness
	if freshPos {
		tags = append(tags, "fresh")
	}
	if freshNeg {
		tags = append(tags, "stale")
	}

	// Site tier tags
	for _, s := range tier1Sites {
		if strings.Contains(siteLower, s) {
			tags = append(tags, "tier1-forum")
			break
		}
	}
	for _, s := range tier2Sites {
		if strings.Contains(siteLower, s) {
			tags = append(tags, "tier2-forum")
			break
		}
	}
	for _, s := range ransomSites {
		if strings.Contains(siteLower, s) {
			tags = append(tags, "ransomware-site")
			break
		}
	}

	return tags
}
