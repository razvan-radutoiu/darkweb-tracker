// Package notify contains all notification channel implementations.
// Each channel satisfies the Notifier interface.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Notifier interface
// ---------------------------------------------------------------------------

// Message is the canonical notification payload passed to every channel.
type Message struct {
	Title    string
	Body     string // plain text body
	Link     string // optional primary link
	SiteName string
	Kind     Kind
}

// Kind classifies the message for channels that treat them differently.
type Kind int

const (
	KindNormal  Kind = iota
	KindStartup      // service start announcement
	KindDaily        // daily report
	KindWeekly       // weekly report
	KindUrgent       // immediate high-priority alert from AI analysis (score>=8)
)

// Notifier is the interface every push channel must implement.
type Notifier interface {
	// Name returns a human-readable channel name for logging.
	Name() string
	// Send delivers msg. Implementations must respect ctx cancellation.
	Send(ctx context.Context, msg Message) error
}

// ---------------------------------------------------------------------------
// Multi-sender: fan out to all enabled channels
// ---------------------------------------------------------------------------

// Multi sends to all registered notifiers, logging failures without aborting.
type Multi struct {
	notifiers []Notifier
	log       *slog.Logger
}

func NewMulti(log *slog.Logger, ns ...Notifier) *Multi {
	return &Multi{notifiers: ns, log: log}
}

func (m *Multi) Send(ctx context.Context, msg Message) {
	for _, n := range m.notifiers {
		if err := n.Send(ctx, msg); err != nil {
			m.log.Error("notification failed",
				"channel", n.Name(),
				"title", msg.Title,
				"err", err,
			)
		}
	}
}

// ---------------------------------------------------------------------------
// Shared HTTP helper with retry + exponential back-off
// ---------------------------------------------------------------------------

func postJSON(ctx context.Context, client *http.Client, endpoint string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	const maxRetries = 5
	base := time.Second

	for attempt := range maxRetries {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		resp, err := client.Do(req)
		if err != nil {
			wait := jitteredDelay(base, attempt)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}

		body := make([]byte, 512)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent:
			return nil

		case resp.StatusCode == http.StatusTooManyRequests:
			wait := jitteredDelay(base, attempt)
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if s, err := strconv.ParseFloat(ra, 64); err == nil {
					if s > 1000 {
						s /= 1000
					}
					wait = time.Duration(s * float64(time.Second))
				}
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}

		case resp.StatusCode >= 500:
			wait := jitteredDelay(base, attempt)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}

		default:
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body[:n])
		}
	}

	return fmt.Errorf("exceeded %d retries", maxRetries)
}

// jitteredDelay returns base*2^attempt + random jitter up to base.
func jitteredDelay(base time.Duration, attempt int) time.Duration {
	d := base * (1 << attempt)
	jitter := time.Duration(rand.Int63n(int64(base)))
	d += jitter
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// ---------------------------------------------------------------------------
// Discord
// ---------------------------------------------------------------------------

type discordEmbed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color"`
	Fields      []embedField `json:"fields,omitempty"`
	Footer      *embedFooter `json:"footer,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
}

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type embedFooter struct {
	Text    string `json:"text"`
	IconURL string `json:"icon_url,omitempty"`
}

type discordPayload struct {
	Embeds []discordEmbed `json:"embeds"`
}

const (
	colorGreen  = 0x57F287
	colorPurple = 0x9C27B0
	colorRed    = 0xED4245 // Discord danger red
)

// Discord sends notifications to a Discord webhook.
type Discord struct {
	webhook          string
	sendNormalMsg    bool
	sendDailyReport  bool
	sendWeeklyReport bool
	client           *http.Client
}

func NewDiscord(webhook string, sendNormal, sendDaily, sendWeekly bool, client *http.Client) *Discord {
	return &Discord{
		webhook:          webhook,
		sendNormalMsg:    sendNormal,
		sendDailyReport:  sendDaily,
		sendWeeklyReport: sendWeekly,
		client:           client,
	}
}

func (d *Discord) Name() string { return "Discord" }

func (d *Discord) Send(ctx context.Context, msg Message) error {
	var embed discordEmbed
	now := time.Now().UTC().Format(time.RFC3339)

	switch msg.Kind {
	case KindStartup:
		embed = discordEmbed{
			Title:       msg.Title,
			Description: msg.Body,
			Color:       colorGreen,
			Footer:      &embedFooter{Text: "DarkWeb Forums Tracker"},
			Timestamp:   now,
		}

	case KindUrgent:
		// Urgent AI alert: bright red, full body as description.
		fields := []embedField{
			{Name: "来源站点", Value: msg.SiteName, Inline: true},
			{Name: "告警时间", Value: time.Now().Format(time.DateTime), Inline: true},
		}
		if msg.Link != "" {
			fields = append(fields, embedField{Name: "原始链接", Value: fmt.Sprintf("[访问原文](%s)", msg.Link)})
		}
		embed = discordEmbed{
			Title:       msg.Title,
			Description: msg.Body,
			Color:       colorRed,
			Fields:      fields,
			Footer:      &embedFooter{Text: "DarkWeb Forums Tracker — AI 紧急告警"},
			Timestamp:   now,
		}

	case KindWeekly:
		if !d.sendWeeklyReport {
			return nil
		}
		embed = discordEmbed{
			Title:     msg.Title,
			Color:     colorPurple,
			Fields:    []embedField{{Name: "报告内容", Value: msg.Body}},
			Footer:    &embedFooter{Text: "DarkWeb Forums Tracker — 周报"},
			Timestamp: now,
		}

	case KindDaily:
		if !d.sendDailyReport {
			return nil
		}
		embed = discordEmbed{
			Title:     msg.Title,
			Color:     colorPurple,
			Fields:    []embedField{{Name: "今日摘要", Value: msg.Body}},
			Footer:    &embedFooter{Text: "DarkWeb Forums Tracker — 日报"},
			Timestamp: now,
		}

	default: // KindNormal
		if !d.sendNormalMsg {
			return nil
		}
		color := rand.Intn(0xFFFFFF + 1)
		fields := []embedField{
			{Name: "来源", Value: msg.SiteName, Inline: true},
			{Name: "推送时间", Value: time.Now().Format(time.DateTime), Inline: true},
		}
		if msg.Link != "" {
			fields = append(fields, embedField{Name: "链接", Value: fmt.Sprintf("[访问](%s)", msg.Link)})
		}
		embed = discordEmbed{
			Title:     msg.Title,
			Color:     color,
			Fields:    fields,
			Footer:    &embedFooter{Text: "DarkWeb Forums Tracker"},
			Timestamp: now,
		}
	}

	return postJSON(ctx, d.client, d.webhook, discordPayload{Embeds: []discordEmbed{embed}})
}

// ---------------------------------------------------------------------------
// Telegram
// ---------------------------------------------------------------------------

type Telegram struct {
	token  string
	chatID string
	client *http.Client
}

func NewTelegram(token, chatID string, client *http.Client) *Telegram {
	return &Telegram{token: token, chatID: chatID, client: client}
}

func (t *Telegram) Name() string { return "Telegram" }

func (t *Telegram) Send(ctx context.Context, msg Message) error {
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)

	text := fmt.Sprintf("*%s*\n%s", escapeMarkdownV2(msg.Title), escapeMarkdownV2(msg.Body))
	if msg.Link != "" {
		text += fmt.Sprintf("\n[链接](%s)", msg.Link)
	}

	payload := map[string]any{
		"chat_id":                  t.chatID,
		"text":                     text,
		"parse_mode":               "MarkdownV2",
		"disable_web_page_preview": true,
	}
	return postJSON(ctx, t.client, endpoint, payload)
}

// escapeMarkdownV2 escapes characters reserved in Telegram MarkdownV2.
func escapeMarkdownV2(s string) string {
	const reserved = `_*[]()~` + "`" + `>#+-=|{}.!`
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(reserved, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// DingTalk (HMAC-SHA256 signed)
// ---------------------------------------------------------------------------

type DingTalk struct {
	webhook   string
	secretKey string
	client    *http.Client
}

func NewDingTalk(webhook, secretKey string, client *http.Client) *DingTalk {
	return &DingTalk{webhook: webhook, secretKey: secretKey, client: client}
}

func (d *DingTalk) Name() string { return "DingTalk" }

func (d *DingTalk) Send(ctx context.Context, msg Message) error {
	endpoint := d.signedURL()
	text := fmt.Sprintf("**%s**\n%s", msg.Title, msg.Body)
	if msg.Link != "" {
		text += "\n链接: " + msg.Link
	}

	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]any{
			"title": msg.Title,
			"text":  text,
		},
		"at": map[string]any{"isAtAll": false},
	}
	return postJSON(ctx, d.client, endpoint, payload)
}

// signedURL appends the timestamp+HMAC-SHA256 signature required by DingTalk.
func (d *DingTalk) signedURL() string {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	raw := ts + "\n" + d.secretKey

	mac := hmac.New(sha256.New, []byte(d.secretKey))
	mac.Write([]byte(raw))
	sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	return fmt.Sprintf("%s&timestamp=%s&sign=%s", d.webhook, ts, sign)
}

// ---------------------------------------------------------------------------
// Feishu (Lark)
// ---------------------------------------------------------------------------

type Feishu struct {
	webhook string
	client  *http.Client
}

func NewFeishu(webhook string, client *http.Client) *Feishu {
	return &Feishu{webhook: webhook, client: client}
}

func (f *Feishu) Name() string { return "Feishu" }

func (f *Feishu) Send(ctx context.Context, msg Message) error {
	body := msg.Title + "\n" + msg.Body
	if msg.Link != "" {
		body += "\n链接: " + msg.Link
	}

	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]any{"text": body},
	}
	return postJSON(ctx, f.client, f.webhook, payload)
}
