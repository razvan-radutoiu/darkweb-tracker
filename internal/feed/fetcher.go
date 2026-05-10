// Package feed handles RSS/Atom feed fetching, parsing, and content cleaning.
package feed

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"darkweb-tracker/internal/config"
	"darkweb-tracker/internal/storage"
)

// Result is a newly discovered feed item (not yet in DB).
type Result struct {
	Item     storage.Item
	SiteName string
}

// Fetcher fetches and parses RSS/Atom feeds.
type Fetcher struct {
	parser     *gofeed.Parser
	httpClient *http.Client
	log        *slog.Logger
}

// New creates a Fetcher. If proxyCfg.Enabled, all requests go through the proxy.
func New(proxyCfg config.ProxyConfig, log *slog.Logger) *Fetcher {
	transport := &http.Transport{}

	if proxyCfg.Enabled && proxyCfg.HTTP != "" {
		// Set proxy via environment — gofeed uses http.DefaultTransport under the hood,
		// so we inject our own transport.
		transport = proxyTransport(proxyCfg)
	}

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	fp := gofeed.NewParser()
	fp.Client = client

	return &Fetcher{
		parser:     fp,
		httpClient: client,
		log:        log,
	}
}

// Fetch retrieves and parses a single RSS source.
// It returns all parsed items (caller decides which are new).
func (f *Fetcher) Fetch(ctx context.Context, ds config.DataSource) ([]storage.Item, error) {
	feed, err := f.parser.ParseURLWithContext(ds.RSSURL, ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch %s (%s): %w", ds.Name, ds.RSSURL, err)
	}

	items := make([]storage.Item, 0, len(feed.Items))
	for _, entry := range feed.Items {
		link := strings.TrimSpace(entry.Link)
		title := strings.TrimSpace(entry.Title)
		if link == "" || title == "" {
			continue
		}

		pubDate := ""
		if entry.PublishedParsed != nil {
			pubDate = entry.PublishedParsed.Format(time.RFC3339)
		} else if entry.UpdatedParsed != nil {
			pubDate = entry.UpdatedParsed.Format(time.RFC3339)
		}

		author := ""
		if entry.Author != nil {
			author = entry.Author.Name
		}

		category := extractCategory(entry)
		content := extractContent(entry)
		dlLinks := extractDownloadLinks(content)

		items = append(items, storage.Item{
			Title:         title,
			Link:          link,
			PubDate:       pubDate,
			Author:        author,
			Category:      category,
			Content:       content,
			DownloadLinks: dlLinks,
			SiteName:      ds.Name,
		})
	}

	f.log.Debug("fetched feed", "source", ds.Name, "items", len(items))
	return items, nil
}

// ---------------------------------------------------------------------------
// Content extraction helpers
// ---------------------------------------------------------------------------

func extractCategory(e *gofeed.Item) string {
	cats := make([]string, 0, len(e.Categories))
	for _, c := range e.Categories {
		if c != "" {
			cats = append(cats, c)
		}
	}
	return strings.Join(cats, ", ")
}

func extractContent(e *gofeed.Item) string {
	raw := ""
	switch {
	case e.Content != "":
		raw = e.Content
	case e.Description != "":
		raw = e.Description
	}
	return cleanContent(raw)
}

// Compiled regexes for content cleaning.
var (
	reHideBlock  = regexp.MustCompile(`(?si)<div[^>]+class="[^"]*(?:messageHide|block-mhhide)[^"]*"[^>]*>.*?</div>`)
	reInputTag   = regexp.MustCompile(`(?i)<input[^>]*>`)
	reReadMore   = regexp.MustCompile(`(?i)<a[^>]+>Read more</a>`)
	reHTMLTag    = regexp.MustCompile(`<[^>]+>`)
	reMultiSpace = regexp.MustCompile(`\s+`)
	reLoginMsg   = regexp.MustCompile(`(?i)You must be registered for see (links|images attach)`)
	reCyrLogin   = regexp.MustCompile(`(?si)Для просмотра скрытого содержимого вы должны.*?</div>`)
)

func cleanContent(html string) string {
	html = reHideBlock.ReplaceAllString(html, "")
	html = reInputTag.ReplaceAllString(html, "")
	html = reReadMore.ReplaceAllString(html, "")
	html = reCyrLogin.ReplaceAllString(html, "")
	text := reHTMLTag.ReplaceAllString(html, " ")
	text = reLoginMsg.ReplaceAllString(text, "")
	text = reMultiSpace.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// ---------------------------------------------------------------------------
// Download link extraction
// ---------------------------------------------------------------------------

var dlPatterns = []*regexp.Regexp{
	// Direct file URLs
	regexp.MustCompile(`(?i)https?://[^\s"<>]+\.(?:zip|rar|7z|txt|csv|xlsx|pdf|exe|iso|torrent|json|xml)`),
	// href attributes
	regexp.MustCompile(`(?i)href=["'](https?://[^\s"<>']+)["']`),
	// download: prefix
	regexp.MustCompile(`(?i)[Dd]ownload\s*[:：]\s*(https?://[^\s"<>]+)`),
	// /download/, /file/, /files/ paths
	regexp.MustCompile(`(?i)(https?://[^\s"<>]+/(?:download|files?)/[^\s"<>]+)`),
	// Known file hosters
	regexp.MustCompile(`(?i)(https?://(?:mega\.nz|mediafire\.com|sendspace\.com|4shared\.com)/[^\s"<>]+)`),
}

// Exclude patterns (images, login pages).
var dlExcludes = []string{".jpg", ".jpeg", ".png", ".gif", ".svg", ".bmp", "/login/", "/register/"}

func extractDownloadLinks(content string) string {
	seen := make(map[string]struct{})
	var links []string

	for _, re := range dlPatterns {
		for _, m := range re.FindAllStringSubmatch(content, -1) {
			// m[0] is full match; m[1] is first group if present.
			link := m[0]
			if len(m) > 1 && m[1] != "" {
				link = m[1]
			}
			link = strings.TrimRight(link, ".,;)")
			if !strings.HasPrefix(link, "http") {
				continue
			}
			lower := strings.ToLower(link)
			excluded := false
			for _, ex := range dlExcludes {
				if strings.Contains(lower, ex) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
			if _, dup := seen[link]; !dup {
				seen[link] = struct{}{}
				links = append(links, link)
			}
		}
	}

	if len(links) == 0 {
		return ""
	}
	return strings.Join(links, "\n")
}

// ---------------------------------------------------------------------------
// Proxy transport helper
// ---------------------------------------------------------------------------

func proxyTransport(cfg config.ProxyConfig) *http.Transport {
	// Use http.ProxyFromEnvironment after setting env vars, or build manually.
	// We build manually to avoid mutating global env.
	return &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			if req.URL.Scheme == "https" && cfg.HTTPS != "" {
				return url.Parse(cfg.HTTPS)
			}
			if cfg.HTTP != "" {
				return url.Parse(cfg.HTTP)
			}
			return nil, nil
		},
	}
}
