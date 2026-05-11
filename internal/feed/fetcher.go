// Package feed handles RSS/Atom feed fetching, parsing, and content cleaning.
// Also handles special sentinel sources (ransomware.live API, ransomwatch JSON).
// Supports HTTP proxy and Tor SOCKS5 proxy for anonymous fetching.
package feed

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/proxy"

	"darkweb-tracker/internal/config"
	"darkweb-tracker/internal/sources"
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

// New creates a Fetcher. Proxy priority: Tor SOCKS5 > HTTP proxy > direct.
func New(proxyCfg config.ProxyConfig, log *slog.Logger) *Fetcher {
	transport := buildTransport(proxyCfg, log)

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

// buildTransport constructs an http.Transport with proxy settings applied.
// Priority: Tor SOCKS5 > HTTP/HTTPS proxy > direct connection.
func buildTransport(proxyCfg config.ProxyConfig, log *slog.Logger) *http.Transport {
	// Tor SOCKS5 takes highest priority
	if proxyCfg.TorSOCKS != "" {
		dialer, err := torDialer(proxyCfg.TorSOCKS)
		if err != nil {
			if log != nil {
				log.Warn("Tor SOCKS5 dialer failed, falling back to direct", "err", err)
			}
		} else {
			if log != nil {
				log.Info("proxy: Tor SOCKS5 active", "addr", proxyCfg.TorSOCKS)
			}
			return &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				},
				// Keep reasonable timeouts even through Tor (Tor adds latency)
				ResponseHeaderTimeout: 45 * time.Second,
				TLSHandshakeTimeout:   20 * time.Second,
			}
		}
	}

	// Fall back to HTTP/HTTPS proxy
	if proxyCfg.Enabled && proxyCfg.HTTP != "" {
		if log != nil {
			log.Info("proxy: HTTP proxy active", "addr", proxyCfg.HTTP)
		}
		return proxyTransport(proxyCfg)
	}

	return &http.Transport{}
}

// torDialer creates a SOCKS5 dialer for the given Tor address.
// addr format: "socks5://127.0.0.1:9050" or "127.0.0.1:9050"
func torDialer(addr string) (proxy.Dialer, error) {
	// Strip scheme if present
	hostPort := addr
	if u, err := url.Parse(addr); err == nil && u.Host != "" {
		hostPort = u.Host
	}
	return proxy.SOCKS5("tcp", hostPort, nil, proxy.Direct)
}

// Fetch retrieves and parses a single RSS source.
// Handles both regular RSS/Atom feeds and special sentinel sources.
func (f *Fetcher) Fetch(ctx context.Context, ds config.DataSource) ([]storage.Item, error) {
	// Handle special sentinel sources
	switch ds.RSSURL {
	case sources.SentinelRansomwareLive:
		return f.fetchRansomwareLive(ctx, ds.Name)
	case sources.SentinelRansomwatch:
		return f.fetchRansomwatch(ctx, ds.Name)
	case sources.SentinelRansomLook:
		return f.fetchRansomLook(ctx, ds.Name)
	}

	// Standard RSS/Atom fetch
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

		// Prefix title with [SiteName] for instant source identification in reports/notifications.
		// Skip if title already starts with "[" (e.g., ransomware sentinel items already prefixed).
		prefixedTitle := title
		if !strings.HasPrefix(title, "[") {
			prefixedTitle = "[" + ds.Name + "] " + title
		}

		items = append(items, storage.Item{
			Title:         prefixedTitle,
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

// ---------------------------------------------------------------------------
// Sentinel source handlers
// ---------------------------------------------------------------------------

// fetchRansomwareLive fetches real-time victim data from ransomware.live API
// and converts each victim entry into a storage.Item.
func (f *Fetcher) fetchRansomwareLive(ctx context.Context, siteName string) ([]storage.Item, error) {
	victims, err := sources.FetchRansomwareLive(ctx, f.httpClient)
	if err != nil {
		return nil, fmt.Errorf("ransomware.live: %w", err)
	}

	items := make([]storage.Item, 0, len(victims))
	for _, v := range victims {
		link := v.ClaimURL
		if link == "" {
			link = "https://www.ransomware.live"
		}
		title := fmt.Sprintf("[%s] %s", strings.ToUpper(v.Group), v.Victim)
		if v.Country != "" {
			title += fmt.Sprintf(" (%s)", v.Country)
		}
		content := v.Description
		if v.Activity != "" {
			content = "[" + v.Activity + "] " + content
		}

		items = append(items, storage.Item{
			Title:    title,
			Link:     link,
			PubDate:  v.AttackDate,
			Category: "ransomware",
			Content:  content,
			SiteName: siteName,
		})
	}

	f.log.Debug("ransomware.live fetched", "victims", len(items))
	return items, nil
}

// fetchRansomLook fetches recent victim posts from ransomlook.io (570+ groups).
func (f *Fetcher) fetchRansomLook(ctx context.Context, siteName string) ([]storage.Item, error) {
	posts, err := sources.FetchRansomLook(ctx, f.httpClient)
	if err != nil {
		return nil, fmt.Errorf("ransomlook: %w", err)
	}

	items := make([]storage.Item, 0, len(posts))
	for _, p := range posts {
		if p.PostTitle == "" || p.GroupName == "" {
			continue
		}
		// Build a stable dedup link from group + title
		link := p.Link
		if link == "" {
			link = fmt.Sprintf("https://www.ransomlook.io/group/%s#%s",
				strings.ToLower(p.GroupName),
				strings.ReplaceAll(strings.ToLower(p.PostTitle), " ", "-"),
			)
		}
		title := fmt.Sprintf("[%s] %s", strings.ToUpper(p.GroupName), p.PostTitle)
		content := p.Description
		if p.Country != "" {
			content = "[" + p.Country + "] " + content
		}

		items = append(items, storage.Item{
			Title:    title,
			Link:     link,
			PubDate:  p.Discovered,
			Category: "ransomware",
			Content:  content,
			SiteName: siteName,
		})
	}

	f.log.Debug("ransomlook fetched", "posts", len(items))
	return items, nil
}
func (f *Fetcher) fetchRansomwatch(ctx context.Context, siteName string) ([]storage.Item, error) {
	// Only fetch posts from the last 30 days to avoid flooding the DB on first run.
	const recentLimit = 200
	posts, err := sources.FetchRansomwatchPosts(ctx, f.httpClient, recentLimit)
	if err != nil {
		return nil, fmt.Errorf("ransomwatch: %w", err)
	}

	items := make([]storage.Item, 0, len(posts))
	for _, p := range posts {
		if p.PostTitle == "" {
			continue
		}
		// Use group+title as dedup link since ransomwatch posts don't have URLs.
		link := fmt.Sprintf("https://ransomwatch.telemetry.ltd/#/%s/%s",
			strings.ToLower(p.GroupName),
			strings.ReplaceAll(strings.ToLower(p.PostTitle), " ", "-"),
		)
		title := fmt.Sprintf("[%s] %s", strings.ToUpper(p.GroupName), p.PostTitle)

		items = append(items, storage.Item{
			Title:    title,
			Link:     link,
			PubDate:  p.Discovered,
			Category: "ransomware",
			Content:  fmt.Sprintf("Ransomware group %s claimed victim: %s", p.GroupName, p.PostTitle),
			SiteName: siteName,
		})
	}

	f.log.Debug("ransomwatch fetched", "posts", len(items))
	return items, nil
}
