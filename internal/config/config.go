// Package config handles loading and merging configuration from YAML files
// and environment variables. Environment variables always take precedence.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Top-level config structure
// ---------------------------------------------------------------------------

type Config struct {
	Push         PushConfig            `yaml:"push"`
	Proxy        ProxyConfig           `yaml:"proxy"`
	NightSleep   NightSleepConfig      `yaml:"night_sleep"`
	DailyReport  ReportConfig          `yaml:"daily_report"`
	WeeklyReport WeeklyReportConfig    `yaml:"weekly_report"`
	DataSources  map[string]DataSource `yaml:"data_sources"`
	Interval     time.Duration         `yaml:"interval"`
	LLM          LLMConfig             `yaml:"llm"`
	Sources      SourcesConfig         `yaml:"sources"`
	Web          WebConfig             `yaml:"web"`
	Fetch        FetchConfig           `yaml:"fetch"`

	// MaxItemAgeDays is the maximum age of a post's pub_date (in days) for it
	// to be eligible for AI analysis and push notifications.
	// Posts older than this are inserted to DB (for deduplication) but are
	// silently skipped — no analysis, no push.
	// Default: 7. Set to 0 to disable the filter (push all new items).
	MaxItemAgeDays int `yaml:"max_item_age_days"` // MAX_ITEM_AGE_DAYS

	// RetentionDays is how many days to keep items in the database.
	// 0 = keep forever. Default 90.
	RetentionDays int `yaml:"retention_days"` // RETENTION_DAYS
}

// FetchConfig controls optional full-page content fetching.
type FetchConfig struct {
	// FullContent enables fetching the linked page and extracting the main article body.
	// Extracted content is stored as full_content and passed to the LLM instead of
	// the RSS teaser, significantly improving analysis quality.
	FullContent bool `yaml:"full_content"` // FETCH_FULL_CONTENT

	// ContentTimeout is the per-page HTTP timeout when fetching full content.
	// Default 15s.
	ContentTimeout time.Duration `yaml:"content_timeout"` // FETCH_CONTENT_TIMEOUT

	// ContentMaxBytes caps the response body read to avoid huge pages.
	// Default 512KB. 0 = no limit.
	ContentMaxBytes int `yaml:"content_max_bytes"` // FETCH_CONTENT_MAX_BYTES
}

// WebConfig controls the built-in HTTP web interface.
// It uses the Telegram Login Widget for authentication and verifies
// that the user is a member of the configured Telegram channel.
type WebConfig struct {
	// Enabled turns the web server on/off. Default false.
	Enabled bool `yaml:"enabled"` // WEB_ENABLED

	// Port is the TCP port to listen on. Default 8080.
	Port int `yaml:"port"` // WEB_PORT

	// BotToken is the Telegram Bot token used to validate login and check
	// channel membership. Can be the same token as Push.Telegram.Token.
	BotToken string `yaml:"bot_token"` // WEB_BOT_TOKEN

	// BotUsername is the bot's @username WITHOUT the leading '@'.
	// Required for the Telegram Login Widget.
	BotUsername string `yaml:"bot_username"` // WEB_BOT_USERNAME

	// ChannelID is the channel @username (e.g. "@mychannel") or numeric ID
	// (e.g. "-1001234567890") whose membership grants access.
	ChannelID string `yaml:"channel_id"` // WEB_CHANNEL_ID

	// ChannelTitle is an optional human-readable display name shown on the login page.
	// If empty, the server will fetch it automatically via the Telegram getChat API.
	ChannelTitle string `yaml:"channel_title"` // WEB_CHANNEL_TITLE

	// SessionSecret is used to sign session cookies. Set to a random string.
	// If empty, a random secret is generated on startup (sessions lost on restart).
	SessionSecret string `yaml:"session_secret"` // WEB_SESSION_SECRET
}

// SourcesConfig controls how RSS feed source lists are loaded.
type SourcesConfig struct {
	// RemoteURL 指向手动维护的补充源列表（优先级最高）
	RemoteURL string `yaml:"remote_url"` // SOURCES_REMOTE_URL

	// LocalOverridesRemote 本地 data_sources 是否覆盖远程同名源
	LocalOverridesRemote bool `yaml:"local_overrides_remote"` // SOURCES_LOCAL_OVERRIDES

	// HealthCheck 启动时对所有源 HEAD 探活，自动剔除死链
	HealthCheck bool `yaml:"health_check"` // SOURCES_HEALTH_CHECK

	// DisableBuiltinSeeds 禁用内置兜底 seed
	DisableBuiltinSeeds bool `yaml:"disable_builtin_seeds"` // SOURCES_NO_SEEDS

	// DisableAutoDiscover 禁用从 deepdarkCTI 自动发现新源
	// 默认 false（启用自动发现），设为 true 则只用手动配置的源
	DisableAutoDiscover bool `yaml:"disable_auto_discover"` // SOURCES_NO_AUTODISCOVER
}

// LLMConfig holds all settings for the AI analysis layer.
// Supports any OpenAI-wire-compatible provider: OpenAI, DeepSeek, Ollama, etc.
type LLMConfig struct {
	// Enabled controls whether AI analysis runs at all.
	Enabled bool `yaml:"enabled"` // LLM_ENABLED

	// Provider is a label for logging: "openai", "deepseek", "ollama", "custom".
	Provider string `yaml:"provider"` // LLM_PROVIDER

	// BaseURL overrides the API endpoint. Leave empty for official OpenAI.
	// DeepSeek: https://api.deepseek.com/v1
	// Ollama:   http://localhost:11434/v1
	// Groq:     https://api.groq.com/openai/v1
	// Gemini:   https://generativelanguage.googleapis.com/v1beta/openai
	BaseURL string `yaml:"base_url"` // LLM_BASE_URL

	// APIKey is the authentication token.
	APIKey string `yaml:"api_key"` // LLM_API_KEY

	// Model is the model identifier, e.g. "gpt-4o-mini", "deepseek-chat", "llama3".
	Model string `yaml:"model"` // LLM_MODEL

	// RPM is the requests-per-minute rate limit. Default 60.
	RPM int `yaml:"rpm"` // LLM_RPM

	// RPMBurst is the burst size for the rate limiter. Default 5.
	RPMBurst int `yaml:"rpm_burst"` // LLM_RPM_BURST

	// Workers is the number of concurrent analysis goroutines. Default 3.
	Workers int `yaml:"workers"` // LLM_WORKERS

	// UrgentScoreThreshold is the minimum score to trigger an immediate push. Default 8.
	UrgentScoreThreshold int `yaml:"urgent_score_threshold"` // LLM_URGENT_THRESHOLD

	// NotifyMinScore is the minimum score for any analyzed item to be pushed
	// immediately. Items below this score are only included in the daily report.
	// Default 5. Set to 0 to push everything, 10 to push only critical.
	NotifyMinScore int `yaml:"notify_min_score"` // LLM_NOTIFY_MIN_SCORE

	// MaxInputChars limits the number of post-body characters sent to the LLM.
	// Default 0 = no limit; full content is sent and the provider handles context.
	// Set e.g. 30000 only if a specific provider rejects oversized requests.
	MaxInputChars int `yaml:"max_input_chars"` // LLM_MAX_INPUT_CHARS

	// DailyTopN is how many top-scored items to include in daily reports. Default 20.
	DailyTopN int `yaml:"daily_top_n"` // LLM_DAILY_TOP_N

	// SkipAnalyzedItems skips re-analysis of items that already have a result in DB.
	SkipAnalyzedItems bool `yaml:"skip_analyzed_items"` // LLM_SKIP_ANALYZED

	// FallbackProviders are secondary providers tried in order when primary fails.
	// Each entry uses the same fields as the primary (base_url, api_key, model).
	// The rules engine is always appended automatically as the final fallback.
	FallbackProviders []LLMProviderConfig `yaml:"fallback_providers"` // LLM_FALLBACK_*
}

// LLMProviderConfig is a secondary provider entry inside LLMConfig.FallbackProviders.
type LLMProviderConfig struct {
	Name    string `yaml:"name"`     // display name, e.g. "groq", "gemini"
	BaseURL string `yaml:"base_url"` // OpenAI-compatible endpoint
	APIKey  string `yaml:"api_key"`  // auth token
	Model   string `yaml:"model"`    // model id
	RPM     int    `yaml:"rpm"`      // rate limit (requests/min)
}

type PushConfig struct {
	DingTalk PushChannel    `yaml:"dingtalk"`
	Feishu   PushChannel    `yaml:"feishu"`
	Telegram TelegramConfig `yaml:"telegram"`
	Discord  DiscordConfig  `yaml:"discord"`
}

type PushChannel struct {
	Webhook string `yaml:"webhook"`
	Enabled bool   `yaml:"enabled"`
}

type TelegramConfig struct {
	Token   string `yaml:"token"`
	ChatID  string `yaml:"chat_id"`
	Enabled bool   `yaml:"enabled"`
}

// DingTalk requires HMAC-SHA256 signing.
type DingTalkConfig struct {
	Webhook   string `yaml:"webhook"`
	SecretKey string `yaml:"secret_key"`
	Enabled   bool   `yaml:"enabled"`
}

type DiscordConfig struct {
	Webhook           string `yaml:"webhook"`
	Enabled           bool   `yaml:"enabled"`
	SendDailyReport   bool   `yaml:"send_daily_report"`
	SendNormalMsg     bool   `yaml:"send_normal_msg"`
	SendWeeklyReport  bool   `yaml:"send_weekly_report"`
}

type ProxyConfig struct {
	Enabled  bool   `yaml:"enabled"`
	HTTP     string `yaml:"http"`
	HTTPS    string `yaml:"https"`
	NoProxy  string `yaml:"no_proxy"`
	// Tor SOCKS5 support
	// If TorSOCKS is set (e.g. "socks5://127.0.0.1:9050"), all RSS fetches
	// go through Tor. Takes precedence over HTTP/HTTPS proxy when set.
	TorSOCKS string `yaml:"tor_socks"` // TOR_SOCKS env var
}

type NightSleepConfig struct {
	Enabled   bool `yaml:"enabled"`
	StartHour int  `yaml:"start_hour"` // default 0
	EndHour   int  `yaml:"end_hour"`   // default 7
}

type ReportConfig struct {
	Enabled bool `yaml:"enabled"`
}

type WeeklyReportConfig struct {
	Enabled   bool      `yaml:"enabled"`
	PushEnabled bool    `yaml:"push_enabled"`
	PushTime  string    `yaml:"push_time"`  // "15:00"
	PushDay   time.Weekday `yaml:"push_day"` // 5 = Friday
}

type DataSource struct {
	Name    string `yaml:"name"`
	RSSURL  string `yaml:"rss_url"`
	Enabled bool   `yaml:"enabled"`
}

// ---------------------------------------------------------------------------
// DingTalk lives separately (has secret_key field)
// ---------------------------------------------------------------------------

// dingtalkYAML is used only during YAML unmarshalling.
type dingtalkYAML struct {
	Webhook   string `yaml:"webhook"`
	SecretKey string `yaml:"secret_key"`
	Enabled   bool   `yaml:"enabled"`
}

// rawPushConfig mirrors PushConfig for YAML loading (DingTalk needs special type).
type rawPushConfig struct {
	DingTalk dingtalkYAML   `yaml:"dingtalk"`
	Feishu   PushChannel    `yaml:"feishu"`
	Telegram TelegramConfig `yaml:"telegram"`
	Discord  DiscordConfig  `yaml:"discord"`
}

type rawConfig struct {
	Push         rawPushConfig         `yaml:"push"`
	Proxy        ProxyConfig           `yaml:"proxy"`
	NightSleep   NightSleepConfig      `yaml:"night_sleep"`
	DailyReport  ReportConfig          `yaml:"daily_report"`
	WeeklyReport WeeklyReportConfig    `yaml:"weekly_report"`
	DataSources  map[string]DataSource `yaml:"data_sources"`
	Interval     string                `yaml:"interval"`
	LLM          LLMConfig             `yaml:"llm"`
	Sources      SourcesConfig         `yaml:"sources"`
	Web          WebConfig             `yaml:"web"`
	Fetch        FetchConfig           `yaml:"fetch"`
	MaxItemAgeDays int                 `yaml:"max_item_age_days"`
	RetentionDays  int                 `yaml:"retention_days"`
}

// DingTalk returns the DingTalk config (stored separately from PushConfig).
var globalDingTalk DingTalkConfig

func DingTalk() DingTalkConfig { return globalDingTalk }

// ---------------------------------------------------------------------------
// Load loads config.yaml then overlays environment variables.
// ---------------------------------------------------------------------------

func Load(path string) (*Config, error) {
	raw := defaultRawConfig()

	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Overlay environment variables.
	applyEnv(&raw)

	cfg := &Config{
		Push: PushConfig{
			Feishu:   raw.Push.Feishu,
			Telegram: raw.Push.Telegram,
			Discord:  raw.Push.Discord,
		},
		Proxy:          raw.Proxy,
		NightSleep:     raw.NightSleep,
		DailyReport:    raw.DailyReport,
		WeeklyReport:   raw.WeeklyReport,
		DataSources:    raw.DataSources,
		LLM:            raw.LLM,
		Sources:        raw.Sources,
		Web:            raw.Web,
		Fetch:          raw.Fetch,
		MaxItemAgeDays: raw.MaxItemAgeDays,
		RetentionDays:  raw.RetentionDays,
	}

	// Apply LLM defaults.
	if cfg.LLM.Model == "" {
		cfg.LLM.Model = "gpt-4o-mini"
	}
	if cfg.LLM.RPM == 0 {
		cfg.LLM.RPM = 60
	}
	if cfg.LLM.RPMBurst == 0 {
		cfg.LLM.RPMBurst = 5
	}
	if cfg.LLM.Workers == 0 {
		cfg.LLM.Workers = 3
	}
	if cfg.LLM.UrgentScoreThreshold == 0 {
		cfg.LLM.UrgentScoreThreshold = 8
	}
	if cfg.LLM.NotifyMinScore == 0 {
		cfg.LLM.NotifyMinScore = 5
	}
	if cfg.LLM.DailyTopN == 0 {
		cfg.LLM.DailyTopN = 20
	}

	// Store DingTalk globally (it has extra field).
	globalDingTalk = DingTalkConfig{
		Webhook:   raw.Push.DingTalk.Webhook,
		SecretKey: raw.Push.DingTalk.SecretKey,
		Enabled:   raw.Push.DingTalk.Enabled,
	}

	// Parse interval.
	if raw.Interval != "" {
		d, err := time.ParseDuration(raw.Interval)
		if err != nil {
			return nil, fmt.Errorf("invalid interval %q: %w", raw.Interval, err)
		}
		cfg.Interval = d
	}
	if cfg.Interval == 0 {
		cfg.Interval = 2 * time.Hour
	}
	if cfg.MaxItemAgeDays == 0 {
		cfg.MaxItemAgeDays = 7 // default: only analyze/push posts from last 7 days
	}
	if cfg.RetentionDays == 0 {
		cfg.RetentionDays = 90 // default: keep 90 days of data
	}
	// FetchConfig defaults.
	if cfg.Fetch.ContentTimeout == 0 {
		cfg.Fetch.ContentTimeout = 15 * time.Second
	}
	if cfg.Fetch.ContentMaxBytes == 0 {
		cfg.Fetch.ContentMaxBytes = 512 * 1024 // 512 KB
	}

	// Validate data sources have required fields.
	for k, ds := range cfg.DataSources {
		if ds.RSSURL == "" {
			return nil, fmt.Errorf("data_source %q missing rss_url", k)
		}
		if ds.Name == "" {
			ds.Name = k
			cfg.DataSources[k] = ds
		}
	}

	return cfg, nil
}

func defaultRawConfig() rawConfig {
	return rawConfig{
		NightSleep: NightSleepConfig{
			Enabled:   true,
			StartHour: 0,
			EndHour:   7,
		},
		DailyReport:  ReportConfig{Enabled: true},
		WeeklyReport: WeeklyReportConfig{
			Enabled:     true,
			PushEnabled: true,
			PushTime:    "15:00",
			PushDay:     time.Friday,
		},
		Push: rawPushConfig{
			Discord: DiscordConfig{
				SendNormalMsg: true,
			},
		},
		Interval: "2h",
		Web: WebConfig{
			Port: 8080,
		},
	}
}

// ---------------------------------------------------------------------------
// Environment variable overlay
// ---------------------------------------------------------------------------

func applyEnv(r *rawConfig) {
	// DingTalk
	envStr("DINGTALK_WEBHOOK", &r.Push.DingTalk.Webhook)
	envStr("DINGTALK_SECRET", &r.Push.DingTalk.SecretKey)
	envBool("DINGTALK_ENABLED", &r.Push.DingTalk.Enabled)

	// Feishu
	envStr("FEISHU_WEBHOOK", &r.Push.Feishu.Webhook)
	envBool("FEISHU_ENABLED", &r.Push.Feishu.Enabled)

	// Telegram
	envStr("TELEGRAM_TOKEN", &r.Push.Telegram.Token)
	envStr("TELEGRAM_CHAT_ID", &r.Push.Telegram.ChatID)
	envBool("TELEGRAM_ENABLED", &r.Push.Telegram.Enabled)

	// Discord
	envStr("DISCORD_WEBHOOK", &r.Push.Discord.Webhook)
	envBool("DISCORD_ENABLED", &r.Push.Discord.Enabled)
	envBool("DISCORD_SEND_DAILY", &r.Push.Discord.SendDailyReport)
	envBool("DISCORD_SEND_NORMAL", &r.Push.Discord.SendNormalMsg)
	envBool("DISCORD_SEND_WEEKLY", &r.Push.Discord.SendWeeklyReport)

	// Proxy
	envBool("PROXY_ENABLED", &r.Proxy.Enabled)
	envStr("HTTP_PROXY", &r.Proxy.HTTP)
	envStr("HTTPS_PROXY", &r.Proxy.HTTPS)
	envStr("NO_PROXY", &r.Proxy.NoProxy)
	envStr("TOR_SOCKS", &r.Proxy.TorSOCKS)

	// Night sleep
	envBool("NIGHT_SLEEP_ENABLED", &r.NightSleep.Enabled)

	// Reports
	envBool("DAILY_REPORT_ENABLED", &r.DailyReport.Enabled)
	envBool("WEEKLY_REPORT_ENABLED", &r.WeeklyReport.Enabled)
	envBool("WEEKLY_REPORT_PUSH_ENABLED", &r.WeeklyReport.PushEnabled)

	// Per data-source toggles: DATASOURCE_<NAME>=true|false
	for key := range r.DataSources {
		envKey := "DATASOURCE_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
		if v := os.Getenv(envKey); v != "" {
			ds := r.DataSources[key]
			ds.Enabled, _ = strconv.ParseBool(v)
			r.DataSources[key] = ds
		}
	}

	// Interval
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		r.Interval = v
	}

	// LLM
	envBool("LLM_ENABLED", &r.LLM.Enabled)
	envStr("LLM_PROVIDER", &r.LLM.Provider)
	envStr("LLM_BASE_URL", &r.LLM.BaseURL)
	envStr("LLM_API_KEY", &r.LLM.APIKey)
	envStr("LLM_MODEL", &r.LLM.Model)
	envInt("LLM_RPM", &r.LLM.RPM)
	envInt("LLM_RPM_BURST", &r.LLM.RPMBurst)
	envInt("LLM_WORKERS", &r.LLM.Workers)
	envInt("LLM_URGENT_THRESHOLD", &r.LLM.UrgentScoreThreshold)
	envInt("LLM_NOTIFY_MIN_SCORE", &r.LLM.NotifyMinScore)
	envInt("LLM_MAX_INPUT_CHARS", &r.LLM.MaxInputChars)
	envInt("LLM_DAILY_TOP_N", &r.LLM.DailyTopN)
	envBool("LLM_SKIP_ANALYZED", &r.LLM.SkipAnalyzedItems)

	// Fallback providers via env: LLM_FALLBACK_1_BASE_URL, LLM_FALLBACK_1_API_KEY, etc.
	// Supports up to 5 fallback providers (indices 1-5).
	for i := 1; i <= 5; i++ {
		prefix := fmt.Sprintf("LLM_FALLBACK_%d_", i)
		baseURL := os.Getenv(prefix + "BASE_URL")
		apiKey := os.Getenv(prefix + "API_KEY")
		model := os.Getenv(prefix + "MODEL")
		if baseURL == "" && apiKey == "" {
			break // no more fallbacks defined
		}
		name := os.Getenv(prefix + "NAME")
		if name == "" {
			name = fmt.Sprintf("fallback-%d", i)
		}
		rpm := 30 // safe default for free tier
		envInt(prefix+"RPM", &rpm)

		// Extend or overwrite slot i-1 in FallbackProviders.
		p := LLMProviderConfig{
			Name:    name,
			BaseURL: baseURL,
			APIKey:  apiKey,
			Model:   model,
			RPM:     rpm,
		}
		if i-1 < len(r.LLM.FallbackProviders) {
			r.LLM.FallbackProviders[i-1] = p
		} else {
			r.LLM.FallbackProviders = append(r.LLM.FallbackProviders, p)
		}
	}

	// Sources
	envStr("SOURCES_REMOTE_URL", &r.Sources.RemoteURL)
	envBool("SOURCES_LOCAL_OVERRIDES", &r.Sources.LocalOverridesRemote)
	envBool("SOURCES_HEALTH_CHECK", &r.Sources.HealthCheck)
	envBool("SOURCES_NO_SEEDS", &r.Sources.DisableBuiltinSeeds)
	envBool("SOURCES_NO_AUTODISCOVER", &r.Sources.DisableAutoDiscover)

	// Web server
	envBool("WEB_ENABLED", &r.Web.Enabled)
	envInt("WEB_PORT", &r.Web.Port)
	envStr("WEB_BOT_TOKEN", &r.Web.BotToken)
	envStr("WEB_BOT_USERNAME", &r.Web.BotUsername)
	envStr("WEB_CHANNEL_ID", &r.Web.ChannelID)
	envStr("WEB_CHANNEL_TITLE", &r.Web.ChannelTitle)
	envStr("WEB_SESSION_SECRET", &r.Web.SessionSecret)

	// Feed age filter
	envInt("MAX_ITEM_AGE_DAYS", &r.MaxItemAgeDays)
	envInt("RETENTION_DAYS", &r.RetentionDays)

	// Full-content fetch
	envBool("FETCH_FULL_CONTENT", &r.Fetch.FullContent)
	envInt("FETCH_CONTENT_MAX_BYTES", &r.Fetch.ContentMaxBytes)
	if v := os.Getenv("FETCH_CONTENT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			r.Fetch.ContentTimeout = d
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func envStr(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func envBool(key string, dst *bool) {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			*dst = b
		}
	}
}

func envInt(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// EnabledSources returns only the data sources that are enabled.
func (c *Config) EnabledSources() []DataSource {
	out := make([]DataSource, 0, len(c.DataSources))
	for _, ds := range c.DataSources {
		if ds.Enabled {
			out = append(out, ds)
		}
	}
	return out
}

// AnyPushEnabled returns true if at least one notification channel is active.
func (c *Config) AnyPushEnabled() bool {
	return globalDingTalk.Enabled ||
		c.Push.Feishu.Enabled ||
		c.Push.Telegram.Enabled ||
		c.Push.Discord.Enabled
}
