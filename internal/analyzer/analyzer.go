// Package analyzer provides LLM-powered analysis of dark web forum posts.
// It uses OpenAI-compatible APIs to classify, score, and extract structured
// threat intelligence from raw forum content.
package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/invopop/jsonschema"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"golang.org/x/time/rate"

	"darkweb-tracker/internal/config"
)

const systemPrompt = `You are a cybersecurity threat intelligence analyst at a Security Operations Center (SOC).
Your task: analyze underground forum posts and extract structured breach intelligence.

━━ SCORING GUIDE (1-10, intelligence value) ━━
1-2  No actionable intel — generic discussion, spam, off-topic, promotional posts
3-4  Low value — unverified claims, samples only (no real data), vague leaks, access denied content
5-6  Medium value — small breach (<10k records), partial/unverified data, plausible but lacks proof
7-8  High value — confirmed breach >10k records, fresh credentials/PII, named target organization
9-10 Critical — >100k records, financial/healthcare/government sector, active exploitation evidence

━━ FIELD RULES ━━
summary          : 1-2 sentences in Simplified Chinese (简体中文). Describe what was leaked, scale, and impact.
affected_targets : Named organizations, countries, or sectors. Empty list if none mentioned.
estimated_records: Conservative estimate as integer. Use -1 if genuinely unknown.
data_types       : Only types explicitly evidenced in the post (email, password, phone, ssn, credit_card, etc.)
is_urgent        : true ONLY when score>=8 AND content implies data is fresh (posted or collected within 30 days).
confidence_level : high = clear evidence; medium = plausible but unverified; low = vague or unreadable content.

━━ CONTENT EDGE CASES ━━
- "Register to view" / login-required placeholder → score 1-2, category="general_discussion", confidence_level="low"
- Very short or empty content (<50 chars) → score 1-3, confidence_level="low"
- Non-English post (Russian, Chinese, etc.) → analyze as normal; summary must still be in Chinese.
- Post discusses techniques/tools without actual data → category="vulnerability_info" or "malware_distribution", score 1-5

━━ SCORING CALIBRATION ━━
Be conservative. When evidence is ambiguous, prefer the lower end of the range.
Do NOT inflate scores for dramatic titles without supporting content.

HARD RULES (no exceptions):
- Score MUST be 1-3 if: no specific named organization AND no data sample/download AND content < 100 chars
- Score MUST be 1-2 if: post is a vendor selling service (fullz, docs, accounts) — these are supply-side ads, not breaches
- Score MUST be 1-3 if: post is a news article analyzing malware/techniques without a specific named victim organization
- Score MUST NOT exceed 4 if confidence_level = "low"
- Score 5+ requires at minimum: a named organization OR a specific breach claim with some evidence

OUTPUT: Return ONLY a valid JSON object matching the provided schema. No markdown fences, no preamble, no trailing text.`

// maxRetries is the number of retry attempts for transient LLM API failures.
const maxRetries = 3

// perStreamTimeout is the wall-clock deadline for a single streaming LLM call.
// Streaming keeps the TCP connection alive by delivering tokens continuously,
// so this timeout only fires if the provider stops sending data entirely.
// 120s accommodates large context windows (64K+) on slower providers.
const perStreamTimeout = 120 * time.Second

// reRetryAfter matches "retry after 42s", "retry after 42", "retry-after: 42"
var reRetryAfter = regexp.MustCompile(`(?i)retry.?after[:\s]+(\d+)`)

// isRetryable reports whether err warrants a backoff-retry and how long to wait.
// It parses the retry-after value from the error message when present.
// Returns (false, 0) for non-retryable errors (e.g. 400 content filter).
//
// ErrRPDExhausted is returned as a sentinel when daily quota is fully consumed —
// the caller should stop retrying and fall back to another provider.
var ErrRPDExhausted = fmt.Errorf("daily request quota exhausted (RPD)")

func isRetryable(err error) (bool, time.Duration) {
	if err == nil {
		return false, 0
	}
	msg := err.Error()

	// 403 — Forbidden; staged back-off, caller multiplies base by attempt number:
	// attempt 1 → 20 s, attempt 2 → 40 s, attempt 3 → 60 s.
	if strings.Contains(msg, "403") {
		return true, 20 * time.Second
	}

	// 429 — rate limited. Try to read exact retry-after from error message.
	if strings.Contains(msg, "429") {
		wait := 15 * time.Second // safe default; most providers reset within 10-60s
		if m := reRetryAfter.FindStringSubmatch(msg); len(m) > 1 {
			if secs, e := strconv.Atoi(m[1]); e == nil && secs > 0 {
				wait = time.Duration(secs)*time.Second + 500*time.Millisecond
			}
		}
		// Cap at 90s — if the provider wants us to wait longer, we'd rather
		// fall back to another provider than hold up the batch.
		if wait > 90*time.Second {
			wait = 90 * time.Second
		}
		return true, wait
	}

	// 5xx server errors — short backoff, worth retrying once
	if strings.Contains(msg, "500") || strings.Contains(msg, "502") ||
		strings.Contains(msg, "503") || strings.Contains(msg, "504") {
		return true, 5 * time.Second
	}

	// Network / connection errors
	if strings.Contains(msg, "connection") || strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "EOF") || strings.Contains(msg, "context deadline") {
		return true, 3 * time.Second
	}

	return false, 0
}

// AnalysisResult is the structured output from LLM analysis.
// All fields are designed for ML training dataset use.
type AnalysisResult struct {
	// Score is the data leak value score, 1-10.
	// 1-3: low value (general discussion, no real data)
	// 4-6: medium value (partial data, outdated leaks)
	// 7-9: high value (fresh credentials, PII, financial data)
	// 10:  critical (massive fresh dump, nation-state targets)
	Score int `json:"score" jsonschema_description:"Data leak value score 1-10. 10=critical fresh dump, 1=irrelevant noise"`

	// Category is the primary classification label.
	Category string `json:"category" jsonschema:"enum=credential_leak,database_dump,financial_data,personal_info,malware_distribution,access_sale,data_trade,vulnerability_info,general_discussion,other"`

	// Tags are secondary labels for multi-label classification.
	Tags []string `json:"tags" jsonschema_description:"Secondary classification tags, e.g. ['email','password','combo-list','sql-dump','fresh']"`

	// Summary is a concise 1-2 sentence Chinese description of the leak content.
	Summary string `json:"summary" jsonschema_description:"1-2句中文摘要，描述泄露数据的内容、规模和影响"`

	// AffectedTargets are organizations/countries/platforms mentioned.
	AffectedTargets []string `json:"affected_targets" jsonschema_description:"Affected organizations, countries, or platforms mentioned in the post"`

	// EstimatedRecords is the estimated number of records if discernible, -1 if unknown.
	EstimatedRecords int `json:"estimated_records" jsonschema_description:"Estimated number of leaked records. -1 if unknown"`

	// DataTypes lists what kind of data is in the leak.
	DataTypes []string `json:"data_types" jsonschema_description:"Types of data: email, password, phone, address, credit_card, ssn, medical, source_code, etc."`

	// IsUrgent means score>=8 AND data appears fresh (< 30 days old implied).
	IsUrgent bool `json:"is_urgent" jsonschema_description:"True if this requires immediate attention: score>=8 and data appears fresh"`

	// ConfidenceLevel is the LLM's self-assessed confidence in this analysis.
	ConfidenceLevel string `json:"confidence_level" jsonschema:"enum=high,medium,low"`

	// Reasoning is a brief explanation of why this score/category was assigned (for training quality).
	Reasoning string `json:"reasoning" jsonschema_description:"Brief explanation of the score and category assignment, for training data quality"`
}

// BatchItem represents a single forum post to be analyzed in a batch operation.
type BatchItem struct {
	ID            int64
	Title         string
	Content       string // RSS teaser fallback
	FullContent   string // full page Markdown (preferred when non-empty)
	SiteName      string
	PubDate       string // RFC3339 or empty — passed to LLM for freshness context
	Author        string // poster/threat actor handle — known TA raises confidence
	DownloadLinks string // newline-separated URLs — presence = strong evidence of real data
}

// BatchResult holds the analysis outcome for a single batch item.
type BatchResult struct {
	ID     int64
	Result *AnalysisResult
	Err    error
}

// Analyzer wraps an OpenAI-compatible client with rate limiting
// and structured output parsing for dark web post analysis.
type Analyzer struct {
	client  *openai.Client
	cfg     config.LLMConfig
	limiter *rate.Limiter
	log     *slog.Logger
	schema  interface{} // pre-computed JSON schema for structured output
}

// New creates an Analyzer with the given LLM configuration and logger.
// It pre-computes the JSON schema and configures rate limiting based on cfg.RPM.
func New(cfg config.LLMConfig, log *slog.Logger) *Analyzer {
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}

	client := openai.NewClient(opts...)

	rpm := cfg.RPM
	if rpm <= 0 {
		rpm = 30
	}
	burst := cfg.RPMBurst
	if burst <= 0 {
		burst = 5
	}

	return &Analyzer{
		client:  &client,
		cfg:     cfg,
		limiter: rate.NewLimiter(rate.Every(time.Minute/time.Duration(rpm)), burst),
		log:     log,
		schema:  generateSchema[AnalysisResult](),
	}
}

// Analyze sends a single forum post to the LLM using the streaming API.
//
// Why streaming:
//   - Streaming delivers tokens incrementally; the connection stays alive the entire
//     time, eliminating HTTP idle-timeout issues even with 64K+ token contexts.
//   - A non-streaming call with a large context blocks until the full response is
//     ready, requiring an artificially short hard-timeout that cuts off large posts.
//   - With streaming we only need a wall-clock deadline (perStreamTimeout) that fires
//     if the provider stops sending data entirely — not for normal processing delay.
//
// fullContent is the full page body as Markdown (preferred over content when non-empty).
// content is the RSS teaser fallback.
// Content size: if MaxInputChars > 0 in config, the effective body is trimmed to that limit.
func (a *Analyzer) Analyze(ctx context.Context, title, content, fullContent, siteName, pubDate, author, downloadLinks string) (*AnalysisResult, error) {
	if err := a.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter wait: %w", err)
	}

	// Prefer full page content; fall back to RSS teaser for length trimming.
	body := fullContent
	if body == "" {
		body = content
	}

	// Trim body only if an explicit limit is configured; otherwise send in full.
	if a.cfg.MaxInputChars > 0 {
		body = truncate(strings.TrimSpace(body), a.cfg.MaxInputChars)
		// Propagate the trimmed body back to the correct field.
		if fullContent != "" {
			fullContent = body
		} else {
			content = body
		}
	}

	userMsg := buildUserMessage(title, content, fullContent, siteName, pubDate, author, downloadLinks)
	model := a.cfg.Model
	if model == "" {
		model = "gpt-4o-mini"
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			retryable, baseWait := isRetryable(lastErr)
			if !retryable {
				break
			}
			var delay time.Duration
			if strings.Contains(lastErr.Error(), "403") {
				// Staged back-off for 403: 20s → 40s → 60s
				delay = baseWait * time.Duration(attempt)
			} else {
				jitter := time.Duration(rand.Int63n(int64(5 * time.Second)))
				delay = baseWait + jitter
			}
			a.log.Warn("retrying LLM streaming call",
				"attempt", attempt,
				"wait", delay.Round(time.Millisecond),
				"err", lastErr,
			)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			case <-time.After(delay):
			}
		}

		result, err := a.analyzeStreaming(ctx, model, title, siteName, userMsg)
		if err != nil {
			lastErr = err
			continue
		}
		return result, nil
	}

	return nil, fmt.Errorf("LLM API failed after %d retries: %w", maxRetries, lastErr)
}

// analyzeStreaming performs one streaming call and accumulates the full response.
func (a *Analyzer) analyzeStreaming(ctx context.Context, model, title, siteName, userMsg string) (*AnalysisResult, error) {
	// Deadline: fires only if the provider goes silent mid-stream.
	streamCtx, cancel := context.WithTimeout(ctx, perStreamTimeout)
	defer cancel()

	stream := a.client.Chat.Completions.NewStreaming(streamCtx, openai.ChatCompletionNewParams{
		Model:       model,
		Temperature: openai.Float(0.1),
		// 2048 output tokens: enough for detailed analysis with full reasoning field.
		// The input context is unbounded from our side — provider's window applies.
		MaxTokens: openai.Int(2048),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userMsg),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &openai.ResponseFormatJSONSchemaParam{
				JSONSchema: openai.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "AnalysisResult",
					Schema: a.schema,
					Strict: openai.Bool(true),
				},
			},
		},
	})

	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		acc.AddChunk(stream.Current())
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("stream error: %w", err)
	}

	if len(acc.Choices) == 0 {
		return nil, fmt.Errorf("empty response from provider")
	}

	raw := acc.Choices[0].Message.Content
	if raw == "" {
		return nil, fmt.Errorf("provider returned empty content (refusal: %s)", acc.Choices[0].Message.Refusal)
	}

	var result AnalysisResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("unmarshal LLM JSON: %w (raw: %.200s)", err, raw)
	}
	if err := validateResult(&result); err != nil {
		return nil, fmt.Errorf("invalid result: %w", err)
	}

	a.log.Debug("analysis complete",
		"title", truncate(title, 80),
		"site", siteName,
		"score", result.Score,
		"category", result.Category,
		"urgent", result.IsUrgent,
		"confidence", result.ConfidenceLevel,
		"tokens_total", acc.Usage.TotalTokens,
		"tokens_prompt", acc.Usage.PromptTokens,
	)

	return &result, nil
}

// AnalyzeBatch processes multiple posts concurrently using a worker pool.
// Results are returned sorted by BatchItem.ID for deterministic ordering.
// If workers <= 0, defaults to 3.
func (a *Analyzer) AnalyzeBatch(ctx context.Context, items []BatchItem, workers int) []BatchResult {
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
				res, err := a.Analyze(ctx, item.Title, item.Content, item.FullContent, item.SiteName, item.PubDate, item.Author, item.DownloadLinks)
				results <- BatchResult{ID: item.ID, Result: res, Err: err}
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
	for r := range results {
		out = append(out, r)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})

	return out
}

// generateSchema pre-computes a JSON schema for type T using the invopop/jsonschema
// reflector. The schema disallows additional properties and avoids $ref indirection,
// which is required for OpenAI strict JSON schema mode.
func generateSchema[T any]() interface{} {
	r := &jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
	}
	var v T
	return r.Reflect(&v)
}

// ---------------------------------------------------------------------------
// IOC pre-extraction — fast regex pass before LLM call
// ---------------------------------------------------------------------------

// preExtractSignals scans title + content for structured signals:
// record counts, data volumes, known file types, and domain-style targets.
// Returns a compact summary string appended to the user message, or "".
// This gives the LLM quantitative anchors that reduce hallucination on scores.
func preExtractSignals(title, content string) string {
	combined := title + " " + content

	var parts []string

	// Record counts: "1.5M", "500K", "2 million", "300,000 records"
	reRecords := regexp.MustCompile(`(?i)(\d[\d,\.]*)\s*(million|m|k|thousand|billion)?\s*(records?|rows?|users?|accounts?|lines?|entries|credentials?|combos?)`)
	if m := reRecords.FindString(combined); m != "" {
		parts = append(parts, "Records: "+strings.TrimSpace(m))
	}

	// Data sizes: "500GB", "2.3 TB", "50 MB"
	reSize := regexp.MustCompile(`(?i)(\d[\d\.]*)\s*(gb|tb|mb|gigabyte|terabyte)`)
	if m := reSize.FindString(combined); m != "" {
		parts = append(parts, "DataSize: "+strings.TrimSpace(m))
	}

	// CVE identifiers
	reCVE := regexp.MustCompile(`(?i)CVE-\d{4}-\d{4,}`)
	if cves := reCVE.FindAllString(combined, 5); len(cves) > 0 {
		parts = append(parts, "CVEs: "+strings.Join(cves, ", "))
	}

	// Common data field signals
	fieldSignals := []string{"ssn", "social security", "credit card", "passport", "medical", "health", "dob", "date of birth", "salary", "bank account", "iban", "swift"}
	var foundFields []string
	low := strings.ToLower(combined)
	for _, sig := range fieldSignals {
		if strings.Contains(low, sig) {
			foundFields = append(foundFields, sig)
		}
	}
	if len(foundFields) > 0 {
		parts = append(parts, "SensitiveFields: "+strings.Join(foundFields, ", "))
	}

	if len(parts) == 0 {
		return ""
	}
	return "PreExtracted:\n  " + strings.Join(parts, "\n  ")
}

// buildUserMessage constructs the user prompt from post metadata.
// When FullContent is non-empty it is used instead of Content, giving the LLM
// the full article body rather than the RSS teaser snippet.
// pubDate, author, downloadLinks are optional — pass empty string when unavailable.
func buildUserMessage(title, content, fullContent, siteName, pubDate, author, downloadLinks string) string {
	title = strings.TrimSpace(title)

	// Prefer full page content; fall back to RSS teaser.
	body := strings.TrimSpace(fullContent)
	if body == "" {
		body = strings.TrimSpace(content)
	}

	// Trim body only if an explicit limit is configured.
	// (Caller handles MaxInputChars truncation before calling this function.)

	var b strings.Builder
	b.Grow(len(title) + len(body) + len(siteName) + 512)

	b.WriteString("━━━━━━━━━ THREAT INTELLIGENCE POST ━━━━━━━━━\n")
	fmt.Fprintf(&b, "Forum    : %s\n", siteName)
	fmt.Fprintf(&b, "Title    : %s\n", title)
	if pubDate != "" {
		fmt.Fprintf(&b, "PostDate : %s\n", pubDate)
	}
	if author != "" {
		// Known threat actor handles (e.g. ShinyHunters, KillSec) are high-confidence signals.
		fmt.Fprintf(&b, "Author   : %s\n", author)
	}
	if downloadLinks != "" {
		// Presence of download links (mega.nz, gofile, etc.) is strong evidence of real data.
		b.WriteString("Downloads:\n")
		for _, dl := range strings.Split(downloadLinks, "\n") {
			if dl = strings.TrimSpace(dl); dl != "" {
				fmt.Fprintf(&b, "  - %s\n", dl)
			}
		}
	}
	if fullContent != "" {
		b.WriteString("Content  : [FULL PAGE — extracted via Readability]\n")
	} else {
		b.WriteString("Content  : [RSS TEASER]\n")
	}
	if body == "" {
		b.WriteString("(no content available)\n")
	} else {
		b.WriteString(body)
		b.WriteString("\n")
	}

	// Pre-extracted quantitative signals — helps LLM score accurately even when
	// content is sparse (RSS teaser only).
	if signals := preExtractSignals(title, body); signals != "" {
		b.WriteString(signals)
		b.WriteString("\n")
	}

	b.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	b.WriteString("Analyze the post above. Return a JSON object matching the schema.\n")
	b.WriteString("IMPORTANT: summary field MUST be written in Simplified Chinese (简体中文).\n")
	b.WriteString("NOTE: If 'Downloads' section is present, data likely EXISTS — score accordingly.\n")
	b.WriteString("Score conservatively — lower score when evidence is insufficient.")

	return b.String()
}

// truncate cuts s to at most maxLen characters, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// validateResult checks that required fields have sane values.
func validateResult(r *AnalysisResult) error {
	if r.Score < 1 || r.Score > 10 {
		return fmt.Errorf("score %d out of range [1, 10]", r.Score)
	}
	if r.Category == "" {
		return fmt.Errorf("category is empty")
	}
	return nil
}
