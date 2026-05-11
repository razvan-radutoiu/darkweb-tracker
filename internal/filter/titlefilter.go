// Package filter provides fast title-based pre-filtering of dark web forum posts
// before they reach the LLM analysis pipeline.
//
// Most dark web RSS content is sparse (login-gated). Accurate LLM analysis
// depends heavily on the title. This package classifies titles into:
//
//   - ActionSkip   — clearly low-value (combo lists, tools, generic aggregations)
//                    → store to DB for dedup, skip LLM, skip push
//   - ActionAnalyze — likely worth LLM analysis (named victim, ransomware, etc.)
//
// The filter is deliberately conservative: when uncertain it passes to LLM.
package filter

import (
	"strings"
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

// QuickFilter classifies a post title without calling any external service.
// It takes O(n) time in title length — essentially free compared to an LLM call.
//
// Logic:
//  1. If title matches a "definitely low-value" pattern → Skip (unless a named
//     target overrides it).
//  2. Otherwise → Analyze (let LLM decide).
func QuickFilter(title, siteName string) Result {
	low := strings.ToLower(title)

	// ── Phase 1: detect named high-value targets ──────────────────────────────
	// If ANY of these appear we always pass to LLM regardless of other signals.
	if hasNamedTarget(low) {
		return Result{Action: ActionAnalyze, Reason: "named target detected"}
	}

	// ── Phase 2: skip known low-value patterns ────────────────────────────────
	for _, p := range skipPatterns {
		if strings.Contains(low, p.keyword) {
			return Result{Action: ActionSkip, Reason: p.reason}
		}
	}

	// ── Phase 3: default — pass to LLM ───────────────────────────────────────
	return Result{Action: ActionAnalyze, Reason: "passed default"}
}

// ---------------------------------------------------------------------------
// Named-target detection
// ---------------------------------------------------------------------------

// hasNamedTarget returns true when the title contains signals that indicate
// a specific, identifiable victim — the main predicate for intelligence value.
func hasNamedTarget(low string) bool {
	for _, kw := range namedTargetKeywords {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

// namedTargetKeywords is the whitelist of terms that signal a real named victim.
// Organized by sector for maintainability.
var namedTargetKeywords = []string{
	// ── Ransomware groups (format: "[GROUP] VictimName") ──────────────────────
	"lockbit", "alphav", "cl0p", "clop", "blackcat", "ransomhub", "akira",
	"play ransomware", "black basta", "medusa", "8base", "rhysida", "hunters",
	"noescaperansomware", "meow", "killsec", "monti", "incransom",
	"dispossessor", "darkvault", "cicada3301", "fog ransomware",

	// ── Financial / Banking ──────────────────────────────────────────────────
	"bank", "banking", "fintech", "credit union", "insurance", "payment",
	"broker", "mortgage", "investment", "financial", "trading platform",
	"visa", "mastercard", "american express", "paypal", "stripe",

	// ── Healthcare ───────────────────────────────────────────────────────────
	"hospital", "clinic", "medical", "health", "pharmacy", "patient data",
	"healthcare", "nhs", "medicare", "medicaid", "dental", "laboratory",

	// ── Government / National scale ──────────────────────────────────────────
	"government", "ministry", "ministry of", "department of", "federal",
	"national id", "national database", "citizen", "municipality", "police",
	"military", "intelligence", "agency", "senate", "parliament",
	"electoral", "tax authority", "immigration", "border",

	// ── Critical infrastructure ──────────────────────────────────────────────
	"power grid", "electricity", "utility", "water treatment", "pipeline",
	"airport", "railway", "metro", "telecom", "isp", "carrier",

	// ── Education / Research ─────────────────────────────────────────────────
	"university", "college", "school district", "research institute",

	// ── Large platforms with specific breach context ──────────────────────────
	// (generic "netflix database" alone is suspicious, but combined with
	// record counts or download links it may be real — LLM decides)
	"employee data", "customer database", "user database", "internal data",
	"source code", "proprietary", "confidential",
}

// ---------------------------------------------------------------------------
// Skip patterns (low-value, no named victim)
// ---------------------------------------------------------------------------

type skipPattern struct {
	keyword string
	reason  string
}

// skipPatterns are applied only after hasNamedTarget returns false.
// Order matters: more specific patterns first.
var skipPatterns = []skipPattern{
	// Dark web vendor/seller ads — selling identity packages, not a breach
	{keyword: "fullz", reason: "identity document vendor post"},
	{keyword: "full info", reason: "fullz vendor post"},
	{keyword: "real docs", reason: "document vendor post"},
	{keyword: "price from $", reason: "vendor pricing post"},
	{keyword: "price from €", reason: "vendor pricing post"},
	{keyword: " for sale ", reason: "item for sale listing"},
	{keyword: "buy now", reason: "vendor listing"},
	{keyword: "dm me", reason: "vendor contact post"},
	{keyword: "contact me", reason: "vendor contact post"},
	{keyword: "telegram @", reason: "vendor contact post"},

	// Cracked/sold accounts without named breach source
	{keyword: "netflix account", reason: "cracked streaming account"},
	{keyword: "spotify account", reason: "cracked streaming account"},
	{keyword: "disney account", reason: "cracked streaming account"},
	{keyword: "hbo account", reason: "cracked streaming account"},
	{keyword: "gaming account", reason: "cracked gaming accounts"},
	{keyword: "cracked account", reason: "cracked account selling"},
	{keyword: "verified account", reason: "cracked account selling"},

	// Security news headlines (analysis articles, not breach data)
	{keyword: " variant ", reason: "malware analysis article"},
	{keyword: " routes ", reason: "malware analysis article"},
	{keyword: "android trojan", reason: "malware analysis article"},
	{keyword: "vulnerability discovered", reason: "vulnerability news"},
	{keyword: "patch tuesday", reason: "patch news"},
	{keyword: "zero-day in", reason: "vulnerability news"},
	{keyword: "researchers found", reason: "research article"},
	{keyword: "researchers discover", reason: "research article"},

	// Generic/introduction posts without intel value
	{keyword: "hello everyone", reason: "introduction post"},
	{keyword: "hi everyone", reason: "introduction post"},
	{keyword: "introduction post", reason: "introduction post"},
	{keyword: "new member", reason: "introduction post"},
	{keyword: "i am here", reason: "introduction post"},

	// Credential combo aggregations — no specific victim, just mixed dump
	{keyword: "combo list", reason: "email combo aggregation"},
	{keyword: "combolist", reason: "email combo aggregation"},
	{keyword: "combos ", reason: "email combo aggregation"},
	{keyword: "email:pass", reason: "email:pass combo dump"},
	{keyword: "mail:pass", reason: "email:pass combo dump"},
	{keyword: "user:pass", reason: "credential combo dump"},
	{keyword: "login:pass", reason: "credential combo dump"},
	{keyword: "id:pass", reason: "credential combo dump"},
	{keyword: "fresh combo", reason: "fresh combo list"},
	{keyword: "free combo", reason: "free combo list"},
	{keyword: "hq combo", reason: "high-quality combo list"},
	{keyword: "private combo", reason: "combo list"},
	{keyword: "combo pack", reason: "combo pack aggregation"},

	// Cracking tools and configs
	{keyword: "openbullet", reason: "cracking tool config"},
	{keyword: "silverbullet", reason: "cracking tool config"},
	{keyword: "sentry mba", reason: "cracking tool config"},
	{keyword: "hq config", reason: "cracking tool config"},
	{keyword: "checker config", reason: "cracking tool config"},
	{keyword: "config file", reason: "cracking config"},
	{keyword: "cracking tool", reason: "cracking tool"},
	{keyword: "brute forcer", reason: "brute force tool"},
	{keyword: " checker ", reason: "credential checker tool"},
	{keyword: "checker v", reason: "credential checker tool"},

	// Wordlists and hash cracking
	{keyword: "wordlist", reason: "wordlist/dictionary file"},
	{keyword: "word list", reason: "wordlist/dictionary file"},
	{keyword: "dictionary ", reason: "dictionary file"},
	{keyword: "rainbow table", reason: "hash rainbow table"},
	{keyword: "dehash", reason: "hash cracking service"},
	{keyword: "hash crack", reason: "hash cracking"},
	{keyword: "ntlm hash", reason: "hash dump without named org"},
	{keyword: "md5 hash", reason: "hash dump without named org"},

	// Stealer logs without named victim
	{keyword: "stealer log", reason: "infostealer logs aggregation"},
	{keyword: "infostealer log", reason: "infostealer logs aggregation"},
	{keyword: "redline log", reason: "redline stealer logs"},
	{keyword: "vidar log", reason: "vidar stealer logs"},
	{keyword: "raccoon log", reason: "raccoon stealer logs"},
	{keyword: "fresh log", reason: "stealer logs aggregation"},
	{keyword: "logs fresh", reason: "stealer logs aggregation"},
	{keyword: "private logs", reason: "stealer logs aggregation"},

	// Generic email lists (no company name = no intelligence value)
	{keyword: "email list", reason: "generic email list"},
	{keyword: "mail list", reason: "generic email list"},
	{keyword: "email database", reason: "generic email database"},
	{keyword: "email leads", reason: "email marketing leads"},
	{keyword: "spam list", reason: "spam email list"},

	// Aggregated collections (not a breach, just re-packaging)
	{keyword: "collection #", reason: "aggregate collection"},
	{keyword: "anti-public", reason: "anti-public combo"},
	{keyword: "breach compilation", reason: "compiled breach aggregation"},
	{keyword: "data compilation", reason: "compiled data aggregation"},
	{keyword: "mega leak", reason: "generic mega-leak aggregation"},

	// Malware/tool releases (not data breaches)
	{keyword: "rat release", reason: "RAT malware release"},
	{keyword: "keylogger", reason: "keylogger tool"},
	{keyword: "botnet", reason: "botnet tool/panel"},
	{keyword: "ddos tool", reason: "DDoS tool"},
	{keyword: "stresser", reason: "stresser/booter tool"},
	{keyword: "crypter fud", reason: "malware crypter"},

	// Proxy/VPN/account selling without breach context
	{keyword: "socks5 list", reason: "proxy list"},
	{keyword: "proxy list", reason: "proxy list"},
	{keyword: "fresh proxy", reason: "proxy list"},
	{keyword: "vpn account", reason: "VPN account selling"},
	{keyword: "netflix account", reason: "cracked streaming account"},
	{keyword: "spotify account", reason: "cracked streaming account"},
	{keyword: "disney account", reason: "cracked streaming account"},
	{keyword: "hbo account", reason: "cracked streaming account"},
	{keyword: "gaming account", reason: "cracked gaming accounts"},
}
