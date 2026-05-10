// Package sources handles dynamic RSS source loading from remote URLs,
// local YAML files, and automatic health-checking to prune dead feeds.
//
// Loading priority (highest → lowest):
//  1. Remote URL (e.g. a GitHub-hosted sources.yaml you control)
//  2. Local config.yaml data_sources block
//  3. Built-in fallback seed list (never empty)
package sources

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"darkweb-tracker/internal/config"
)

// ---------------------------------------------------------------------------
// RemoteSource — one entry in a remote sources file
// ---------------------------------------------------------------------------

// RemoteEntry describes a single RSS feed in a remote sources YAML.
// The format is intentionally simple so community files are easy to edit.
type RemoteEntry struct {
	Name    string `yaml:"name"`
	RSSURL  string `yaml:"rss_url"`
	Enabled bool   `yaml:"enabled"`
	// Tags are optional metadata for filtering (e.g. "credentials", "malware").
	Tags []string `yaml:"tags,omitempty"`
	// Note is a human-readable comment (e.g. "moved from .st to .hn in Jan 2026").
	Note string `yaml:"note,omitempty"`
}

// RemoteSourceFile is the top-level structure of a remote sources YAML.
type RemoteSourceFile struct {
	// Version allows future schema evolution.
	Version string        `yaml:"version"`
	Updated string        `yaml:"updated"` // ISO date for display
	Sources []RemoteEntry `yaml:"sources"`
}

// ---------------------------------------------------------------------------
// Loader
// ---------------------------------------------------------------------------

// Loader merges remote + local sources and validates them.
type Loader struct {
	cfg        config.SourcesConfig
	httpClient *http.Client
	log        *slog.Logger
}

// New creates a Loader.
func New(cfg config.SourcesConfig, httpClient *http.Client, log *slog.Logger) *Loader {
	return &Loader{cfg: cfg, httpClient: httpClient, log: log}
}

// Load returns the final merged, deduplicated, enabled source list.
//
// Strategy:
//  1. If RemoteURL is configured, fetch and parse it.
//  2. Merge with local config.DataSources (local wins on conflicts if cfg.LocalOverridesRemote).
//  3. Append built-in seeds for any names not already present.
//  4. If HealthCheck is enabled, probe each URL and remove dead ones.
func (l *Loader) Load(ctx context.Context, localSources map[string]config.DataSource) ([]config.DataSource, error) {
	merged := make(map[string]config.DataSource)

	// Step 1: Remote sources.
	if l.cfg.RemoteURL != "" {
		remote, err := l.fetchRemote(ctx, l.cfg.RemoteURL)
		if err != nil {
			l.log.Warn("remote sources fetch failed — using local only",
				"url", l.cfg.RemoteURL, "err", err)
		} else {
			l.log.Info("remote sources loaded",
				"url", l.cfg.RemoteURL,
				"count", len(remote.Sources),
				"updated", remote.Updated,
			)
			for _, e := range remote.Sources {
				if e.RSSURL == "" || e.Name == "" {
					continue
				}
				key := normalizeKey(e.Name)
				merged[key] = config.DataSource{
					Name:   e.Name,
					RSSURL: e.RSSURL,
					Enabled: e.Enabled,
				}
			}
		}
	}

	// Step 2: Local config (merge / override).
	for key, ds := range localSources {
		if ds.RSSURL == "" {
			continue
		}
		if existing, ok := merged[normalizeKey(ds.Name)]; ok {
			if l.cfg.LocalOverridesRemote {
				// Local wins: replace remote entry.
				merged[normalizeKey(ds.Name)] = ds
			} else {
				// Remote wins, but respect local enabled flag.
				existing.Enabled = ds.Enabled
				merged[normalizeKey(ds.Name)] = existing
			}
		} else {
			merged[key] = ds
		}
	}

	// Step 3: Built-in seeds (only add if not already present).
	if !l.cfg.DisableBuiltinSeeds {
		for _, seed := range builtinSeeds {
			key := normalizeKey(seed.Name)
			if _, exists := merged[key]; !exists {
				merged[key] = seed
			}
		}
	}

	// Collect enabled sources.
	var active []config.DataSource
	for _, ds := range merged {
		if ds.Enabled {
			active = append(active, ds)
		}
	}

	l.log.Info("sources after merge", "total", len(merged), "enabled", len(active))

	// Step 4: Health check (parallel HEAD requests, remove dead feeds).
	if l.cfg.HealthCheck && len(active) > 0 {
		active = l.healthFilter(ctx, active)
	}

	if len(active) == 0 {
		return nil, fmt.Errorf("no active RSS sources after loading (all disabled or failed health check)")
	}

	return active, nil
}

// ---------------------------------------------------------------------------
// Remote fetch
// ---------------------------------------------------------------------------

func (l *Loader) fetchRemote(ctx context.Context, rawURL string) (*RemoteSourceFile, error) {
	// Support GitHub shorthand: "owner/repo/path/to/file.yaml"
	url := expandGitHubURL(rawURL)

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "darkweb-tracker/1.0 (https://github.com)")
	req.Header.Set("Accept", "application/yaml, text/plain, */*")

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024)) // 512 KB max
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var file RemoteSourceFile
	if err := yaml.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}

	return &file, nil
}

// expandGitHubURL converts "owner/repo/path/file.yaml" to a raw.githubusercontent.com URL.
// Full https:// URLs are passed through unchanged.
func expandGitHubURL(s string) string {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	// Shorthand: "owner/repo/branch/path" or "owner/repo/path" (defaults to main).
	parts := strings.SplitN(s, "/", 4)
	switch len(parts) {
	case 3:
		// owner/repo/file.yaml → main branch
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/%s",
			parts[0], parts[1], parts[2])
	case 4:
		// owner/repo/branch/path
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
			parts[0], parts[1], parts[2], parts[3])
	default:
		return s
	}
}

// ---------------------------------------------------------------------------
// Health check — parallel HEAD probes
// ---------------------------------------------------------------------------

func (l *Loader) healthFilter(ctx context.Context, sources []config.DataSource) []config.DataSource {
	type result struct {
		ds    config.DataSource
		alive bool
	}

	results := make([]result, len(sources))
	var wg sync.WaitGroup

	// Limit concurrency to avoid hammering every source at once.
	sem := make(chan struct{}, 8)

	for i, ds := range sources {
		wg.Add(1)
		go func(idx int, src config.DataSource) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			alive := l.probe(ctx, src.RSSURL)
			results[idx] = result{ds: src, alive: alive}
			if !alive {
				l.log.Warn("source health check failed — skipping",
					"name", src.Name, "url", src.RSSURL)
			} else {
				l.log.Debug("source healthy", "name", src.Name)
			}
		}(i, ds)
	}

	wg.Wait()

	var alive []config.DataSource
	for _, r := range results {
		if r.alive {
			alive = append(alive, r.ds)
		}
	}

	l.log.Info("health check complete",
		"checked", len(sources),
		"alive", len(alive),
		"dead", len(sources)-len(alive),
	)
	return alive
}

func (l *Loader) probe(ctx context.Context, url string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	// 200–399 = alive; 403 may still serve content to real browsers, mark alive.
	return resp.StatusCode < 500
}

// ---------------------------------------------------------------------------
// Built-in seed list  (fallback, last resort)
// ---------------------------------------------------------------------------

func normalizeKey(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "_"))
}

// builtinSeeds is the minimum set of sources baked into the binary.
// These are only used when neither remote nor local config provides a source
// with the same name. Think of them as "factory defaults".
var builtinSeeds = []config.DataSource{
	{Name: "gerki", RSSURL: "https://forum.gerki.ws/forums/-/index.rss", Enabled: true},
	{Name: "blackbones", RSSURL: "https://blackbones.net/forums/-/index.rss", Enabled: true},
	{Name: "hard-tm", RSSURL: "https://hard-tm.su/forums/-/index.rss", Enabled: true},
	{Name: "ascarding", RSSURL: "https://ascarding.net/forums/-/index.rss", Enabled: true},
	{Name: "mipped", RSSURL: "https://mipped.com/f/forums/-/index.rss", Enabled: true},
	{Name: "leakbase", RSSURL: "https://leakbase.la/forums/-/index.rss", Enabled: true},
	{Name: "dublikat", RSSURL: "https://at.dublikat.club/forums/-/index.rss", Enabled: true},
	{Name: "cardforum", RSSURL: "https://cardforum.cc/syndication.php?limit=50", Enabled: true},
	{Name: "ipbmafia", RSSURL: "https://ipbmafia.ru/rss/1-temy.xml/", Enabled: true},
	// Currently unreliable — disabled by default in seeds, can be re-enabled via remote.
	{Name: "xforums.st", RSSURL: "https://xforums.st/forums/-/index.rss", Enabled: false},
	{Name: "htdark", RSSURL: "https://htdark.com/forums/-/index.rss", Enabled: false},
	{Name: "niflheim", RSSURL: "https://niflheim.world/forums/-/index.rss", Enabled: false},
	{Name: "sinister", RSSURL: "https://sinister.ly/syndication.php?fid=9&limit=50", Enabled: false},
}
