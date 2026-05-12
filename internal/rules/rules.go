// Package rules loads and exposes filter_rules.yaml — all configurable
// pattern lists for title filtering and rules-based scoring.
//
// Separation of concerns:
//   - filter_rules.yaml  → human-editable, no recompile needed
//   - filter package     → consumes SkipPatterns + NamedTargets
//   - analyzer package   → consumes RulesConfig (vendor patterns, site tiers, data types…)
package rules

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// FilterRules is the top-level structure loaded from filter_rules.yaml.
type FilterRules struct {
	// SkipPatterns: titles matching these keywords are stored in DB but never
	// sent to the analyzer or pushed to notification channels.
	// Applied ONLY after NamedTargets check — a named target always wins.
	SkipPatterns []SkipPattern `yaml:"skip_patterns"`

	// NamedTargets: if ANY of these substrings appear in the title, the item
	// is ALWAYS forwarded to the analyzer, regardless of skip patterns.
	NamedTargets []string `yaml:"named_targets"`

	// Rules engine scoring configuration.
	Rules RulesConfig `yaml:"rules"`
}

// SkipPattern is a keyword + human-readable reason for logging.
type SkipPattern struct {
	Keyword string `yaml:"keyword"`
	Reason  string `yaml:"reason"`
}

// RulesConfig holds all knobs for the deterministic scoring engine.
type RulesConfig struct {
	// VendorPatterns: text matching any of these → immediately scored 2 (vendor ad).
	VendorPatterns []string `yaml:"vendor_patterns"`

	// Site tier lists — used for score bonuses.
	Tier1Sites  []string `yaml:"tier1_sites"`
	Tier2Sites  []string `yaml:"tier2_sites"`
	RansomSites []string `yaml:"ransom_sites"`

	// Freshness signals.
	FreshPositive []string `yaml:"fresh_positive"`
	FreshNegative []string `yaml:"fresh_negative"`

	// DataTypes maps type name → list of trigger keywords.
	// Presence of "combo" type caps score at 4.
	DataTypes map[string][]string `yaml:"data_types"`
}

// Load reads filter_rules.yaml from path and returns the parsed rules.
// If the file does not exist, built-in defaults are returned (no error).
// If the file exists but is malformed, an error is returned.
func Load(path string) (*FilterRules, error) {
	fr := defaults()

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fr, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Unmarshal on top of defaults so missing keys keep default values.
	if err := yaml.Unmarshal(data, fr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	return fr, nil
}

// defaults returns the built-in FilterRules used when filter_rules.yaml
// is absent. These are the same patterns that were previously hardcoded.
func defaults() *FilterRules {
	return &FilterRules{
		SkipPatterns: defaultSkipPatterns(),
		NamedTargets: defaultNamedTargets(),
		Rules:        defaultRulesConfig(),
	}
}

func defaultSkipPatterns() []SkipPattern {
	return []SkipPattern{
		// ── Vendor / seller ads ──────────────────────────────────────────────
		{Keyword: "fullz", Reason: "identity document vendor post"},
		{Keyword: "full info", Reason: "fullz vendor post"},
		{Keyword: "real docs", Reason: "document vendor post"},
		{Keyword: "price from $", Reason: "vendor pricing post"},
		{Keyword: "price from €", Reason: "vendor pricing post"},
		{Keyword: " for sale ", Reason: "item for sale listing"},
		{Keyword: "buy now", Reason: "vendor listing"},
		{Keyword: "dm me", Reason: "vendor contact post"},
		{Keyword: "contact me", Reason: "vendor contact post"},
		{Keyword: "telegram @", Reason: "vendor contact post"},
		{Keyword: "whatsapp:", Reason: "vendor contact post"},
		{Keyword: "whatsapp +", Reason: "vendor contact post"},
		{Keyword: "buy fake", Reason: "document vendor post"},
		{Keyword: "buy real and fake", Reason: "document vendor post"},
		{Keyword: "buy diplomatic", Reason: "document vendor post"},
		{Keyword: "buy passport", Reason: "document vendor post"},
		{Keyword: "sell clone card", Reason: "carding vendor post"},
		{Keyword: "sell cc ", Reason: "carding vendor post"},

		// ── Cracked accounts ─────────────────────────────────────────────────
		{Keyword: "netflix account", Reason: "cracked streaming account"},
		{Keyword: "spotify account", Reason: "cracked streaming account"},
		{Keyword: "disney account", Reason: "cracked streaming account"},
		{Keyword: "hbo account", Reason: "cracked streaming account"},
		{Keyword: "gaming account", Reason: "cracked gaming accounts"},
		{Keyword: "cracked account", Reason: "cracked account selling"},
		{Keyword: "verified account", Reason: "cracked account selling"},

		// ── Security news (analysis, not breach) ─────────────────────────────
		{Keyword: " variant ", Reason: "malware analysis article"},
		{Keyword: "android trojan", Reason: "malware analysis article"},
		{Keyword: "vulnerability discovered", Reason: "vulnerability news"},
		{Keyword: "patch tuesday", Reason: "patch news"},
		{Keyword: "zero-day in", Reason: "vulnerability news"},
		{Keyword: "researchers found", Reason: "research article"},
		{Keyword: "researchers discover", Reason: "research article"},

		// ── Introduction / off-topic posts ───────────────────────────────────
		{Keyword: "hello everyone", Reason: "introduction post"},
		{Keyword: "hi everyone", Reason: "introduction post"},
		{Keyword: "introduction post", Reason: "introduction post"},
		{Keyword: "new member", Reason: "introduction post"},
		{Keyword: "i am here", Reason: "introduction post"},

		// ── Combo lists / aggregations ───────────────────────────────────────
		{Keyword: "combo list", Reason: "email combo aggregation"},
		{Keyword: "combolist", Reason: "email combo aggregation"},
		{Keyword: "combos ", Reason: "email combo aggregation"},
		{Keyword: "combo mixed", Reason: "email combo aggregation"},
		{Keyword: "combo { ", Reason: "country combo list"},
		{Keyword: "k++ ] combo", Reason: "country combo list"},
		{Keyword: "k++ combo", Reason: "country combo list"},
		{Keyword: "mixed combo", Reason: "combo aggregation"},
		{Keyword: "email:pass", Reason: "email:pass combo dump"},
		{Keyword: "mail:pass", Reason: "email:pass combo dump"},
		{Keyword: "user:pass", Reason: "credential combo dump"},
		{Keyword: "login:pass", Reason: "credential combo dump"},
		{Keyword: "id:pass", Reason: "credential combo dump"},
		{Keyword: "fresh combo", Reason: "fresh combo list"},
		{Keyword: "free combo", Reason: "free combo list"},
		{Keyword: "hq combo", Reason: "high-quality combo list"},
		{Keyword: "private combo", Reason: "combo list"},
		{Keyword: "combo pack", Reason: "combo pack aggregation"},
		{Keyword: "collection #", Reason: "aggregate collection"},
		{Keyword: "anti-public", Reason: "anti-public combo"},
		{Keyword: "breach compilation", Reason: "compiled breach aggregation"},
		{Keyword: "data compilation", Reason: "compiled data aggregation"},
		{Keyword: "mega leak", Reason: "generic mega-leak aggregation"},

		// ── Cracking tools ───────────────────────────────────────────────────
		{Keyword: "openbullet", Reason: "cracking tool config"},
		{Keyword: "silverbullet", Reason: "cracking tool config"},
		{Keyword: "sentry mba", Reason: "cracking tool config"},
		{Keyword: "hq config", Reason: "cracking tool config"},
		{Keyword: "checker config", Reason: "cracking tool config"},
		{Keyword: "config file", Reason: "cracking config"},
		{Keyword: "cracking tool", Reason: "cracking tool"},
		{Keyword: "brute forcer", Reason: "brute force tool"},
		{Keyword: " checker ", Reason: "credential checker tool"},
		{Keyword: "checker v", Reason: "credential checker tool"},

		// ── Wordlists / hash cracking ─────────────────────────────────────────
		{Keyword: "wordlist", Reason: "wordlist/dictionary file"},
		{Keyword: "word list", Reason: "wordlist/dictionary file"},
		{Keyword: "rainbow table", Reason: "hash rainbow table"},
		{Keyword: "dehash", Reason: "hash cracking service"},
		{Keyword: "hash crack", Reason: "hash cracking"},
		{Keyword: "ntlm hash", Reason: "hash dump without named org"},
		{Keyword: "md5 hash", Reason: "hash dump without named org"},

		// ── Stealer logs (no named victim) ───────────────────────────────────
		{Keyword: "stealer log", Reason: "infostealer logs aggregation"},
		{Keyword: "infostealer log", Reason: "infostealer logs aggregation"},
		{Keyword: "redline log", Reason: "redline stealer logs"},
		{Keyword: "vidar log", Reason: "vidar stealer logs"},
		{Keyword: "raccoon log", Reason: "raccoon stealer logs"},
		{Keyword: "fresh log", Reason: "stealer logs aggregation"},
		{Keyword: "logs fresh", Reason: "stealer logs aggregation"},
		{Keyword: "private logs", Reason: "stealer logs aggregation"},

		// ── Generic email lists ───────────────────────────────────────────────
		{Keyword: "email list", Reason: "generic email list"},
		{Keyword: "mail list", Reason: "generic email list"},
		{Keyword: "email database", Reason: "generic email database"},
		{Keyword: "email leads", Reason: "email marketing leads"},
		{Keyword: "spam list", Reason: "spam email list"},

		// ── Malware / tool releases ───────────────────────────────────────────
		{Keyword: "rat release", Reason: "RAT malware release"},
		{Keyword: "keylogger", Reason: "keylogger tool"},
		{Keyword: "botnet", Reason: "botnet tool/panel"},
		{Keyword: "ddos tool", Reason: "DDoS tool"},
		{Keyword: "stresser", Reason: "stresser/booter tool"},
		{Keyword: "crypter fud", Reason: "malware crypter"},

		// ── Proxy / VPN selling ───────────────────────────────────────────────
		{Keyword: "socks5 list", Reason: "proxy list"},
		{Keyword: "proxy list", Reason: "proxy list"},
		{Keyword: "fresh proxy", Reason: "proxy list"},
		{Keyword: "vpn account", Reason: "VPN account selling"},
	}
}

func defaultNamedTargets() []string {
	return []string{
		// Ransomware groups
		"lockbit", "alphav", "cl0p", "clop", "blackcat", "ransomhub", "akira",
		"play ransomware", "black basta", "medusa", "8base", "rhysida", "hunters",
		"noescaperansomware", "meow", "killsec", "monti", "incransom",
		"dispossessor", "darkvault", "cicada3301", "fog ransomware",

		// Financial / Banking
		"bank", "banking", "fintech", "credit union", "insurance", "payment",
		"broker", "mortgage", "investment", "financial", "trading platform",
		"visa", "mastercard", "american express", "paypal", "stripe",

		// Healthcare
		"hospital", "clinic", "medical", "health", "pharmacy", "patient data",
		"healthcare", "nhs", "medicare", "medicaid", "dental", "laboratory",

		// Government / National scale
		"government", "ministry of", "department of", "federal",
		"national id", "national database", "citizen", "municipality", "police",
		"military", "intelligence", "agency", "senate", "parliament",
		"electoral", "tax authority", "immigration", "border",

		// Critical infrastructure
		"power grid", "electricity", "utility", "water treatment", "pipeline",
		"airport", "railway", "metro", "telecom", "isp", "carrier",

		// Education / Research
		"university", "college", "school district", "research institute",

		// Specific breach context signals
		"employee data", "customer database", "user database", "internal data",
		"source code", "proprietary", "confidential",
	}
}

func defaultRulesConfig() RulesConfig {
	return RulesConfig{
		VendorPatterns: []string{
			"whatsapp:", "whatsapp +", "telegram @", "telegram:", "@telegram",
			"buy fake", "buy real and fake", "buy diplomatic", "buy passport",
			"buy driver", "buy id card", "sell clone card", "sell cc ", "sell cvv",
			"combo mixed email", "combo mixed mail", "k++ combo", "k++ ] combo",
			"combo { ", "[ combo", "fresh combo", "free combo", "hq combo",
			"private combo", "combo pack", "mixed combo",
		},
		Tier1Sites: []string{
			"breachforum", "leakbase", "thejavasea", "exploit-in", "xss-is",
		},
		Tier2Sites: []string{
			"probiv", "darkforum", "altenens", "nulled", "leakforum",
			"mipped", "hard-tm", "dublikat", "leetforum", "ipbmafia",
		},
		RansomSites: []string{
			"ransomware", "ransom", "lockbit", "alphv", "blackcat",
		},
		FreshPositive: []string{
			"fresh", "new", "latest", "2024", "2025", "2026",
			"today", "just", "hot", "0day",
		},
		FreshNegative: []string{
			"old", "2020", "2019", "2018", "2017", "2016", "2015",
			"leaked in 20", "from 20",
		},
		DataTypes: map[string][]string{
			"email":       {"email", "e-mail", "@gmail", "@yahoo", "@hotmail", "@outlook"},
			"password":    {"password", "passwd", "pwd", "hash", "md5", "sha1", "bcrypt", "ntlm", "plaintext", "cracked"},
			"phone":       {"phone number", "mobile number", "telephone", "gsm", "sms code"},
			"address":     {"home address", "physical address", "location data", "zip code", "postal code"},
			"credit_card": {"credit card", "cc dump", "cvv", "cvc", "fullz", "card number", "debit card"},
			"ssn":         {"ssn", "social security", "national id number", "tax id"},
			"dob":         {"date of birth", "birthday", "birthdate", "dob:"},
			"ip":          {"ip address", "ip log", "ipv4", "ipv6"},
			"cookie":      {"cookie", "session token", "auth token", "session cookie"},
			"source_code": {"source code", "gitlab", "repository leak", "git dump"},
			"financial":   {"bank account", "iban", "swift code", "routing number", "account balance", "wire transfer"},
			"medical":     {"medical record", "health record", "patient data", "diagnosis", "prescription", "hipaa"},
			"government":  {"government database", ".gov", "military data", "dod ", "nsa ", "fbi ", "cia "},
			"combo":       {"combolist", "combo list", "mail:pass", "email:pass", "user:pass", "login:pass"},
		},
	}
}
