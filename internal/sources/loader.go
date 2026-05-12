// Package sources handles dynamic RSS source loading from multiple GitHub-maintained lists.
//
// Loading priority (highest → lowest):
//  1. Remote sources.yaml  (manual overrides)
//  2. Local config.yaml data_sources
//  3. Auto-discovery from multiple GitHub-maintained lists (runs in parallel)
//  4. Built-in seed list  (never empty fallback)
//
// Auto-discovery sources (all parsed on every startup):
//   - fastfire/deepdarkCTI forum.md          — dark web forums with ONLINE status
//   - fastfire/deepdarkCTI ransomware_gang.md — ransomware gang RSS feeds
//   - adminlove520/DarkWeb-Forums-Tracker     — pre-validated RSS URL list
//   - joshhighet/ransomwatch posts.json       — live ransomware victim posts
//   - ransomware.live API                     — real-time ransomware victims
//   - zer0yu/CyberSecurityRSS OPML            — 1000+ security RSS feeds (breach/leak subset)
//   - Direct security RSS feeds               — HIBP, BleepingComputer, DDoSecrets, etc.
package sources

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"darkweb-tracker/internal/config"
)

// ---------------------------------------------------------------------------
// Remote YAML types
// ---------------------------------------------------------------------------

type RemoteEntry struct {
	Name    string   `yaml:"name"`
	RSSURL  string   `yaml:"rss_url"`
	Enabled bool     `yaml:"enabled"`
	Tags    []string `yaml:"tags,omitempty"`
	Note    string   `yaml:"note,omitempty"`
}

type RemoteSourceFile struct {
	Version string        `yaml:"version"`
	Updated string        `yaml:"updated"`
	Sources []RemoteEntry `yaml:"sources"`
}

// ---------------------------------------------------------------------------
// Loader
// ---------------------------------------------------------------------------

type Loader struct {
	cfg        config.SourcesConfig
	httpClient *http.Client
	log        *slog.Logger
}

func New(cfg config.SourcesConfig, httpClient *http.Client, log *slog.Logger) *Loader {
	return &Loader{cfg: cfg, httpClient: httpClient, log: log}
}

// Load returns the final merged, deduplicated, alive source list.
func (l *Loader) Load(ctx context.Context, localSources map[string]config.DataSource) ([]config.DataSource, error) {
	merged := make(map[string]config.DataSource)

	// Step 1: Built-in seeds (lowest priority)
	if !l.cfg.DisableBuiltinSeeds {
		for _, s := range builtinSeeds {
			merged[normalizeKey(s.Name)] = s
		}
	}

	// Step 2: Auto-discovery from all GitHub sources (parallel)
	// A fixed deadline prevents slow probres (e.g. dead forum domains) from
	// blocking startup indefinitely. 60s is generous for direct connections.
	if !l.cfg.DisableAutoDiscover {
		discoverCtx, discoverCancel := context.WithTimeout(ctx, 60*time.Second)
		discovered := l.autoDiscoverAll(discoverCtx)
		discoverCancel()
		l.log.Info("auto-discovery complete", "found", len(discovered))
		for _, ds := range discovered {
			key := normalizeKey(ds.Name)
			if _, exists := merged[key]; !exists {
				merged[key] = ds
			}
		}
	}

	// Step 3: Local config.yaml data_sources
	for _, ds := range localSources {
		if ds.RSSURL != "" {
			merged[normalizeKey(ds.Name)] = ds
		}
	}

	// Step 4: Remote sources.yaml (highest priority)
	if l.cfg.RemoteURL != "" {
		remote, err := l.fetchRemote(ctx, l.cfg.RemoteURL)
		if err != nil {
			l.log.Warn("remote sources fetch failed", "url", l.cfg.RemoteURL, "err", err)
		} else {
			l.log.Info("remote sources loaded", "count", len(remote.Sources), "updated", remote.Updated)
			for _, e := range remote.Sources {
				if e.Name != "" && e.RSSURL != "" {
					merged[normalizeKey(e.Name)] = config.DataSource{
						Name: e.Name, RSSURL: e.RSSURL, Enabled: e.Enabled,
					}
				}
			}
		}
	}

	// Collect enabled
	var active []config.DataSource
	for _, ds := range merged {
		if ds.Enabled {
			active = append(active, ds)
		}
	}
	l.log.Info("sources merged", "total", len(merged), "enabled", len(active))

	// Step 5: Health check
	if l.cfg.HealthCheck && len(active) > 0 {
		active = l.healthFilter(ctx, active)
	}

	if len(active) == 0 {
		return nil, fmt.Errorf("no active RSS sources after loading")
	}
	return active, nil
}

// ---------------------------------------------------------------------------
// Auto-discovery orchestrator — all sources run in parallel
// ---------------------------------------------------------------------------

func (l *Loader) autoDiscoverAll(ctx context.Context) []config.DataSource {
	type discoveryFunc func(context.Context) []config.DataSource

	discoverers := []struct {
		name string
		fn   discoveryFunc
	}{
		{"deepdarkCTI/forum.md", l.discoverDeepDarkCTIForums},
		{"deepdarkCTI/ransomware_gang.md", l.discoverDeepDarkCTIRansomware},
		{"deepdarkCTI/markets.md", l.discoverDeepDarkCTIMarkets},
		{"adminlove520/rss_dataleak.yaml", l.discoverAdminloveYAML},
		{"ransomware.live API", l.discoverRansomwareLive},
		{"ransomwatch/posts.json", l.discoverRansomwatch},
		{"ransomlook.io API", l.discoverRansomLook},
		{"zer0yu/CyberSecurityRSS OPML", l.discoverCyberSecOPML},
		{"direct security RSS feeds", l.discoverDirectFeeds},
	}

	type result struct {
		name    string
		sources []config.DataSource
	}
	results := make(chan result, len(discoverers))
	var wg sync.WaitGroup

	for _, d := range discoverers {
		wg.Add(1)
		go func(name string, fn discoveryFunc) {
			defer wg.Done()
			sources := fn(ctx)
			results <- result{name: name, sources: sources}
		}(d.name, d.fn)
	}

	go func() { wg.Wait(); close(results) }()

	// Merge all discovered, dedup by name
	seen := make(map[string]bool)
	var all []config.DataSource
	for r := range results {
		l.log.Info("discovered", "source", r.name, "count", len(r.sources))
		for _, ds := range r.sources {
			key := normalizeKey(ds.Name)
			if !seen[key] {
				seen[key] = true
				all = append(all, ds)
			}
		}
	}
	return all
}

// ---------------------------------------------------------------------------
// 1. fastfire/deepdarkCTI — forum.md (ONLINE clearnet forums → probe RSS)
// ---------------------------------------------------------------------------

const deepdarkCTIForumURL = "https://raw.githubusercontent.com/fastfire/deepdarkCTI/main/forum.md"

var (
	reMDOnline  = regexp.MustCompile(`\[([^\]]+)\]\((https://[^)]+)\)[^|]*ONLINE`)
	reOnion     = regexp.MustCompile(`\.onion`)
	rssPathList = []string{
		"/forums/-/index.rss",
		"/syndication.php?limit=50",
		"/syndication.php?fid=127&limit=50", // BreachForums: Leaks & Databases subforum
		"/syndication.php?fid=4&limit=50",   // common "Leaks" subforum fid
		"/external.php?type=RSS2",
		"/rss/1-temy.xml/",
		"/feed/",
		"/rss.xml",
		"/index.rss",
	}
)

func (l *Loader) discoverDeepDarkCTIForums(ctx context.Context) []config.DataSource {
	body := l.fetchText(ctx, deepdarkCTIForumURL, 2*1024*1024)
	if body == "" {
		return nil
	}

	var candidates []forumCandidate
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		for _, m := range reMDOnline.FindAllStringSubmatch(scanner.Text(), -1) {
			base := strings.TrimRight(m[2], "/")
			if reOnion.MatchString(base) || seen[base] {
				continue
			}
			seen[base] = true
			candidates = append(candidates, forumCandidate{sanitizeName(m[1]), base})
		}
	}

	return l.probeRSSBatch(ctx, candidates)
}

// ---------------------------------------------------------------------------
// 2. fastfire/deepdarkCTI — ransomware_gang.md (extract RSS Feed column)
// ---------------------------------------------------------------------------

const deepdarkCTIRansomURL = "https://raw.githubusercontent.com/fastfire/deepdarkCTI/main/ransomware_gang.md"

var reRSSInCell = regexp.MustCompile(`https?://[^\s|<>"]+\.(?:rss|xml|atom)|https?://[^\s|<>"]+/(?:rss|feed|atom)/?(?:[^|\s<>"]*)?`)

func (l *Loader) discoverDeepDarkCTIRansomware(ctx context.Context) []config.DataSource {
	body := l.fetchText(ctx, deepdarkCTIRansomURL, 2*1024*1024)
	if body == "" {
		return nil
	}

	var sources []config.DataSource
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(body))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 { // skip header rows
			continue
		}
		line := scanner.Text()
		if !strings.Contains(line, "ONLINE") {
			continue
		}
		// Extract RSS URL from the RSS Feed column (5th column)
		cols := strings.Split(line, "|")
		if len(cols) < 5 {
			continue
		}
		// Name from first column
		nameMatch := regexp.MustCompile(`\[([^\]]+)\]`).FindStringSubmatch(cols[1])
		name := ""
		if len(nameMatch) > 1 {
			name = sanitizeName(nameMatch[1])
		}
		// Search RSS in all columns
		for _, col := range cols {
			rssURL := reRSSInCell.FindString(col)
			if rssURL != "" && !seen[rssURL] && !reOnion.MatchString(rssURL) {
				seen[rssURL] = true
				if name == "" {
					name = rssURL
				}
				sources = append(sources, config.DataSource{
					Name: name + " (ransom)", RSSURL: rssURL, Enabled: true,
				})
			}
		}
	}
	return sources
}

// ---------------------------------------------------------------------------
// 3. fastfire/deepdarkCTI — markets.md (clearnet dark markets → probe RSS)
// ---------------------------------------------------------------------------

const deepdarkCTIMarketsURL = "https://raw.githubusercontent.com/fastfire/deepdarkCTI/main/markets.md"

// reMarketOnline matches: [Name](https://clearnet-url)| ONLINE
var reMarketOnline = regexp.MustCompile(`\[([^\]]+)\]\((https://[^)]+)\)[^|]*\|\s*ONLINE`)

func (l *Loader) discoverDeepDarkCTIMarkets(ctx context.Context) []config.DataSource {
	body := l.fetchText(ctx, deepdarkCTIMarketsURL, 2*1024*1024)
	if body == "" {
		return nil
	}

	var candidates []forumCandidate
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		for _, m := range reMarketOnline.FindAllStringSubmatch(scanner.Text(), -1) {
			base := strings.TrimRight(m[2], "/")
			if reOnion.MatchString(base) || seen[base] {
				continue
			}
			seen[base] = true
			candidates = append(candidates, forumCandidate{sanitizeName(m[1]), base})
		}
	}

	return l.probeRSSBatch(ctx, candidates)
}

// ---------------------------------------------------------------------------
// 4. adminlove520/DarkWeb-Forums-Tracker — rss_dataleak.yaml (pre-validated!)
// ---------------------------------------------------------------------------

const adminloveYAMLURL = "https://raw.githubusercontent.com/adminlove520/DarkWeb-Forums-Tracker/main/rss_dataleak.yaml"

func (l *Loader) discoverAdminloveYAML(ctx context.Context) []config.DataSource {
	body := l.fetchText(ctx, adminloveYAMLURL, 512*1024)
	if body == "" {
		return nil
	}

	// Format: "name":\n  rss_url: "URL"\n  website_name: "Name"
	var raw map[string]struct {
		RSSURL      string `yaml:"rss_url"`
		WebsiteName string `yaml:"website_name"`
	}
	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		l.log.Warn("adminlove YAML parse failed", "err", err)
		return nil
	}

	var sources []config.DataSource
	for _, v := range raw {
		if v.RSSURL == "" {
			continue
		}
		name := v.WebsiteName
		if name == "" {
			name = v.RSSURL
		}
		sources = append(sources, config.DataSource{
			Name: name, RSSURL: v.RSSURL, Enabled: true,
		})
	}
	return sources
}

// ---------------------------------------------------------------------------
// 4. ransomware.live API — real-time victim posts (convert to internal items)
// ---------------------------------------------------------------------------

const ransomwareLiveURL = "https://api.ransomware.live/v2/recentvictims"

type ransomwareLiveVictim struct {
	Victim      string `json:"victim"`
	Group       string `json:"group"`
	AttackDate  string `json:"attackdate"`
	ClaimURL    string `json:"claim_url"`
	Description string `json:"description"`
	Country     string `json:"country"`
	Activity    string `json:"activity"`
}

// RansomwareLiveItems is stored for the scheduler to process directly.
// The loader wraps it as a virtual "RSS" source using a sentinel URL.
const ransomwareLiveSentinel = "internal://ransomware.live/victims"

func (l *Loader) discoverRansomwareLive(ctx context.Context) []config.DataSource {
	// We add a sentinel source; the fetcher handles it specially.
	return []config.DataSource{{
		Name:    "ransomware.live",
		RSSURL:  ransomwareLiveSentinel,
		Enabled: true,
	}}
}

// FetchRansomwareLive retrieves recent victims from ransomware.live API.
// Called by the feed fetcher when it encounters the sentinel URL.
func FetchRansomwareLive(ctx context.Context, client *http.Client) ([]ransomwareLiveVictim, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ransomwareLiveURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "darkweb-tracker/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ransomware.live: HTTP %d", resp.StatusCode)
	}
	var victims []ransomwareLiveVictim
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10*1024*1024)).Decode(&victims); err != nil {
		return nil, err
	}
	return victims, nil
}

// ---------------------------------------------------------------------------
// 5. joshhighet/ransomwatch — posts.json (recent ransomware posts)
// ---------------------------------------------------------------------------

const ransomwatchPostsURL = "https://raw.githubusercontent.com/joshhighet/ransomwatch/main/posts.json"

type ransomwatchPost struct {
	PostTitle string `json:"post_title"`
	GroupName string `json:"group_name"`
	Discovered string `json:"discovered"`
}

const ransomwatchSentinel = "internal://ransomwatch/posts"

func (l *Loader) discoverRansomwatch(ctx context.Context) []config.DataSource {
	return []config.DataSource{{
		Name:    "ransomwatch",
		RSSURL:  ransomwatchSentinel,
		Enabled: true,
	}}
}

// FetchRansomwatchPosts retrieves recent posts from ransomwatch.
func FetchRansomwatchPosts(ctx context.Context, client *http.Client, limit int) ([]ransomwatchPost, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ransomwatchPostsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "darkweb-tracker/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var posts []ransomwatchPost
	if err := json.NewDecoder(io.LimitReader(resp.Body, 20*1024*1024)).Decode(&posts); err != nil {
		return nil, err
	}
	// Return only the most recent posts (already sorted newest first in the file)
	if limit > 0 && len(posts) > limit {
		posts = posts[:limit]
	}
	return posts, nil
}

// ---------------------------------------------------------------------------
// 6. zer0yu/CyberSecurityRSS — OPML (extract breach/leak related feeds)
// ---------------------------------------------------------------------------

const cyberSecOPMLURL = "https://raw.githubusercontent.com/zer0yu/CyberSecurityRSS/master/CyberSecurityRSS.opml"

// Keywords to filter relevant feeds from the 1000+ OPML list.
// Deliberately anchored to breach/leak signal — broad terms like "malware"
// or "infosec" pull in security news sites that publish analysis articles
// rather than actual breach data.
var leakKeywords = []string{
	// Core breach/leak terms
	"breach", "data leak", "data breach", "leaked", "data dump",
	// Dark web forum terms
	"darkweb", "dark web", "underground forum", "cybercrime",
	// Threat actor output
	"ransomware", "infostealer", "stealer logs", "combo list",
	// Data types
	"credential", "combolist", "combo", "fullz", "doxxing",
	// Trackers
	"haveibeenpwned", "paste", "pastebin",
}

type opmlOutline struct {
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr"`
	XMLUrl   string        `xml:"xmlUrl,attr"`
	HTMLUrl  string        `xml:"htmlUrl,attr"`
	Outlines []opmlOutline `xml:"outline"`
}

type opmlBody struct {
	Outlines []opmlOutline `xml:"outline"`
}

type opmlDoc struct {
	Body opmlBody `xml:"body"`
}

func (l *Loader) discoverCyberSecOPML(ctx context.Context) []config.DataSource {
	body := l.fetchText(ctx, cyberSecOPMLURL, 5*1024*1024)
	if body == "" {
		return nil
	}

	var doc opmlDoc
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		l.log.Warn("OPML parse failed", "err", err)
		return nil
	}

	var sources []config.DataSource
	var walkOutlines func(outlines []opmlOutline)
	walkOutlines = func(outlines []opmlOutline) {
		for _, o := range outlines {
			if o.XMLUrl != "" {
				label := strings.ToLower(o.Text + " " + o.Title)
				for _, kw := range leakKeywords {
					if strings.Contains(label, kw) {
						sources = append(sources, config.DataSource{
							Name:    o.Text,
							RSSURL:  o.XMLUrl,
							Enabled: true,
						})
						break
					}
				}
			}
			walkOutlines(o.Outlines)
		}
	}
	walkOutlines(doc.Body.Outlines)
	return sources
}

// ---------------------------------------------------------------------------
// 7. Direct security RSS feeds (curated high-signal list)
// ---------------------------------------------------------------------------

func (l *Loader) discoverDirectFeeds(_ context.Context) []config.DataSource {
	return []config.DataSource{
		// ── Real breach/leak trackers (data-bearing, not news) ──────────────
		// HaveIBeenPwned: actual breach notifications (high signal)
		{Name: "HaveIBeenPwned", RSSURL: "https://feeds.feedburner.com/HaveIBeenPwnedLatestBreaches", Enabled: true},
		// DDoSecrets: document/database leak collective
		{Name: "DDoSecrets", RSSURL: "https://ddosecrets.substack.com/feed", Enabled: true},
		// ransomfeed.it: ransomware victim tracker with actual data claims
		{Name: "RansomFeed", RSSURL: "https://ransomfeed.it/rss.php", Enabled: true},
		// TweetFeed: real-time IOC / breach links aggregated from security researchers
		{Name: "TweetFeed-ransomware", RSSURL: "https://tweetfeed.live/rss/tag/ransomware.xml", Enabled: true},
		{Name: "TweetFeed-breach", RSSURL: "https://tweetfeed.live/rss/tag/databreach.xml", Enabled: true},
	}
}

// ---------------------------------------------------------------------------
// RSS probe helpers (for deepdarkCTI forum discovery)
// ---------------------------------------------------------------------------

type forumCandidate struct{ name, base string }

func (l *Loader) probeRSSBatch(ctx context.Context, candidates []forumCandidate) []config.DataSource {
	type probeResult struct {
		ds  config.DataSource
		ok  bool
	}
	results := make([]probeResult, len(candidates))
	sem := make(chan struct{}, 15)
	var wg sync.WaitGroup

	for i, c := range candidates {
		wg.Add(1)
		go func(idx int, name, base string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rssURL := l.probeRSS(ctx, base)
			if rssURL != "" {
				results[idx] = probeResult{ok: true, ds: config.DataSource{
					Name: sanitizeName(name), RSSURL: rssURL, Enabled: true,
				}}
			}
		}(i, c.name, c.base)
	}
	wg.Wait()

	var sources []config.DataSource
	for _, r := range results {
		if r.ok {
			sources = append(sources, r.ds)
		}
	}
	return sources
}

func (l *Loader) probeRSS(ctx context.Context, baseURL string) string {
	for _, path := range rssPathList {
		if l.isValidRSS(ctx, baseURL+path) {
			return baseURL + path
		}
	}
	return ""
}

func (l *Loader) isValidRSS(ctx context.Context, url string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; DarkWebTracker/1.0)")
	resp, err := l.httpClient.Do(req)
	if err != nil || resp.StatusCode >= 400 {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	s := string(buf[:n])
	return strings.Contains(s, "<rss") || strings.Contains(s, "<feed") ||
		strings.Contains(s, "<channel>") || strings.Contains(s, "<?xml")
}

// ---------------------------------------------------------------------------
// Health check
// ---------------------------------------------------------------------------

func (l *Loader) healthFilter(ctx context.Context, sources []config.DataSource) []config.DataSource {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 10)
		results []config.DataSource
	)
	for _, ds := range sources {
		// Sentinel URLs bypass health check
		if strings.HasPrefix(ds.RSSURL, "internal://") {
			mu.Lock()
			results = append(results, ds)
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(src config.DataSource) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if l.isValidRSS(ctx, src.RSSURL) {
				mu.Lock()
				results = append(results, src)
				mu.Unlock()
			} else {
				l.log.Warn("health check failed", "name", src.Name, "url", src.RSSURL)
			}
		}(ds)
	}
	wg.Wait()
	l.log.Info("health check done", "alive", len(results), "dead", len(sources)-len(results))
	return results
}

// ---------------------------------------------------------------------------
// Remote YAML fetch
// ---------------------------------------------------------------------------

func (l *Loader) fetchRemote(ctx context.Context, rawURL string) (*RemoteSourceFile, error) {
	url := expandGitHubURL(rawURL)
	body := l.fetchText(ctx, url, 512*1024)
	if body == "" {
		return nil, fmt.Errorf("empty response from %s", url)
	}
	var file RemoteSourceFile
	if err := yaml.Unmarshal([]byte(body), &file); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	return &file, nil
}

// ---------------------------------------------------------------------------
// Generic HTTP text fetch
// ---------------------------------------------------------------------------

func (l *Loader) fetchText(ctx context.Context, url string, maxBytes int64) string {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "darkweb-tracker/1.0 (github.com)")
	resp, err := l.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	return string(body)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func expandGitHubURL(s string) string {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	parts := strings.SplitN(s, "/", 4)
	if len(parts) == 3 {
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/%s", parts[0], parts[1], parts[2])
	}
	if len(parts) == 4 {
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", parts[0], parts[1], parts[2], parts[3])
	}
	return s
}

func normalizeKey(name string) string {
	return strings.ToLower(strings.NewReplacer(
		" ", "_", ".", "_", "-", "_", "(", "", ")", "",
	).Replace(strings.TrimSpace(name)))
}

func sanitizeName(name string) string {
	name = regexp.MustCompile(`(?i)\s*\((?:deep|dark|mirror|backup|onion)[^)]*\)`).ReplaceAllString(name, "")
	return strings.TrimSpace(name)
}

// ---------------------------------------------------------------------------
// 8. ransomlook.io API — 570+ ransomware groups, real-time victims
// ---------------------------------------------------------------------------

const ransomLookRecentURL = "https://www.ransomlook.io/api/recent"
const ransomLookSentinel = "internal://ransomlook.io/recent"

type ransomLookPost struct {
	PostTitle   string `json:"post_title"`
	Discovered  string `json:"discovered"`
	Description string `json:"description"`
	Link        string `json:"link"`
	GroupName   string `json:"group_name"`
	Country     string `json:"country"`
}

func (l *Loader) discoverRansomLook(_ context.Context) []config.DataSource {
	return []config.DataSource{{
		Name:    "ransomlook",
		RSSURL:  ransomLookSentinel,
		Enabled: true,
	}}
}

// FetchRansomLook retrieves recent victim posts from ransomlook.io API.
func FetchRansomLook(ctx context.Context, client *http.Client) ([]ransomLookPost, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ransomLookRecentURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "darkweb-tracker/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ransomlook: HTTP %d", resp.StatusCode)
	}
	var posts []ransomLookPost
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10*1024*1024)).Decode(&posts); err != nil {
		return nil, err
	}
	return posts, nil
}

// ---------------------------------------------------------------------------
// Sentinel URL constants (used by feed fetcher to detect special sources)
// ---------------------------------------------------------------------------

const (
	SentinelRansomwareLive = ransomwareLiveSentinel
	SentinelRansomwatch    = ransomwatchSentinel
	SentinelRansomLook     = ransomLookSentinel
)

// ---------------------------------------------------------------------------
// Built-in seeds (last resort)
// ---------------------------------------------------------------------------

var builtinSeeds = []config.DataSource{
	// BreachForums — try all known active domains (frequently rotated)
	{Name: "breachforums", RSSURL: "https://breachforums.st/syndication.php?limit=50", Enabled: true},
	{Name: "breachforums-rs", RSSURL: "https://breachforums.rs/syndication.php?limit=50", Enabled: true},
	{Name: "breachforums-cx", RSSURL: "https://breachforums.cx/syndication.php?limit=50", Enabled: true},
	// Other high-value leak forums
	{Name: "leakbase", RSSURL: "https://leakbase.la/forums/-/index.rss", Enabled: true},
	{Name: "hard-tm", RSSURL: "https://hard-tm.su/forums/-/index.rss", Enabled: true},
	{Name: "mipped", RSSURL: "https://mipped.com/f/forums/-/index.rss", Enabled: true},
	{Name: "cardforum", RSSURL: "https://cardforum.cc/syndication.php?limit=50", Enabled: true},
	{Name: "ipbmafia", RSSURL: "https://ipbmafia.ru/rss/1-temy.xml/", Enabled: true},
}
