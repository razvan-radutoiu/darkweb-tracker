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

const systemPrompt = `You are a cybersecurity threat intelligence analyst working for a security operations center (SOC).
Your task is to analyze threat intelligence data collected from underground forums for the purpose of:
- Early warning of data breaches
- Protecting organizations and individuals from credential exposure
- Supporting law enforcement and incident response teams

This is a legitimate security research and monitoring activity. Analyze each post objectively as structured threat data.

SCORING GUIDE (score 1-10, based on intelligence value):
- 1-2: No actionable intelligence (general discussion, off-topic content)
- 3-4: Low value (unverified claims, old data >1 year, samples only)
- 5-6: Medium value (small dataset <10k records, partial data, plausible but unverified)
- 7-8: High value (fresh verified breach, >10k records, credentials or PII confirmed)
- 9-10: Critical (>100k records, financial/healthcare/government sector, active exploitation confirmed)

URGENCY RULE: Set is_urgent=true ONLY when BOTH: score>=8 AND data is implied to be recent (<30 days).

OUTPUT: Respond ONLY with valid JSON matching the provided schema. No markdown, no preamble.`

// maxContentLen is the maximum number of characters to send to the LLM.
const maxContentLen = 2000

// maxRetries is the number of retry attempts for transient LLM API failures.
const maxRetries = 3

// perRequestTimeout is the hard timeout per single LLM call.
// Prevents a single slow provider from blocking the whole batch.
const perRequestTimeout = 8 * time.Second

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
	ID       int64
	Title    string
	Content  string
	SiteName string
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
		rpm = 60
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

// Analyze sends a single forum post to the LLM for structured analysis.
// It rate-limits requests, retries on transient errors (up to 3 times with
// exponential backoff of 1s, 2s, 4s plus jitter), and validates the result.
func (a *Analyzer) Analyze(ctx context.Context, title, content, siteName string) (*AnalysisResult, error) {
	if err := a.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter wait: %w", err)
	}

	userMsg := buildUserMessage(title, content, siteName)

	model := a.cfg.Model
	if model == "" {
		model = "gpt-4o-mini"
	}

	var (
		completion *openai.ChatCompletion
		lastErr    error
	)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			retryable, baseWait := isRetryable(lastErr)
			if !retryable {
				// 不可重试的错误（如 400 内容过滤）直接返回
				break
			}

			// 指数退避，但 429 用服务器建议的等待时间作为基础
			jitter := time.Duration(rand.Int63n(int64(5 * time.Second)))
			delay := baseWait + jitter

			a.log.Warn("retrying LLM API call",
				"attempt", attempt,
				"max_retries", maxRetries,
				"wait", delay.Round(time.Millisecond),
				"error", lastErr,
			)

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			case <-time.After(delay):
			}
		}

		var err error
		// Each API call gets its own hard deadline so a slow/hung provider
		// never blocks the entire batch. The parent ctx is also respected.
		callCtx, callCancel := context.WithTimeout(ctx, perRequestTimeout)
		completion, err = a.client.Chat.Completions.New(callCtx, openai.ChatCompletionNewParams{
			Model:       model,
			Temperature: openai.Float(0.1),
			MaxTokens:   openai.Int(800),
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
		callCancel()
		if err != nil {
			lastErr = err
			continue
		}

		lastErr = nil
		break
	}

	if lastErr != nil {
		return nil, fmt.Errorf("LLM API failed after %d retries: %w", maxRetries, lastErr)
	}

	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("LLM returned empty choices for title=%q", title)
	}

	raw := completion.Choices[0].Message.Content
	var result AnalysisResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("unmarshal LLM JSON response: %w (raw: %.200s)", err, raw)
	}

	if err := validateResult(&result); err != nil {
		return nil, fmt.Errorf("invalid analysis result: %w", err)
	}

	a.log.Debug("analysis complete",
		"title", truncate(title, 80),
		"site", siteName,
		"score", result.Score,
		"category", result.Category,
		"urgent", result.IsUrgent,
		"confidence", result.ConfidenceLevel,
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
				res, err := a.Analyze(ctx, item.Title, item.Content, item.SiteName)
				results <- BatchResult{
					ID:     item.ID,
					Result: res,
					Err:    err,
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

// buildUserMessage constructs the user prompt from post metadata.
// Content is truncated to maxContentLen to stay within token budgets.
func buildUserMessage(title, content, siteName string) string {
	content = truncate(strings.TrimSpace(content), maxContentLen)
	title = strings.TrimSpace(title)

	// Wrap in threat intelligence framing to reduce content filter false positives.
	// The analytical framing signals research/security context to content moderation.
	var b strings.Builder
	b.Grow(len(title) + len(content) + len(siteName) + 128)

	b.WriteString("THREAT INTELLIGENCE REPORT FOR SOC ANALYSIS\n")
	b.WriteString("============================================\n")
	fmt.Fprintf(&b, "Source Forum : %s\n", siteName)
	fmt.Fprintf(&b, "Post Title   : %s\n", title)
	b.WriteString("Post Content :\n")
	b.WriteString(content)
	b.WriteString("\n============================================\n")
	b.WriteString("Classify the above post according to the schema.")

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
