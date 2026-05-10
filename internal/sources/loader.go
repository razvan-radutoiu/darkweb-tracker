// Package sources handles dynamic RSS source loading.
// Priority: remote sources.yaml > local config > auto-discovery from deepdarkCTI > builtin seeds
package sources

import (
	"bufio"
	"context"
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
// Remote YAML types (手动维护的补充列表)
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

// Load 返回最终合并、去重、存活的源列表。
//
// 优先级（高→低）:
//  1. 远程 sources.yaml（手动补充 / 覆盖）
//  2. 本地 config.yaml data_sources
//  3. 自动从 deepdarkCTI 解析 ONLINE 论坛并探测 RSS
//  4. 内置 seed（兜底，永不为空）
func (l *Loader) Load(ctx context.Context, localSources map[string]config.DataSource) ([]config.DataSource, error) {
	merged := make(map[string]config.DataSource) // key = normalizeKey(name)

	// ── Step 1: 内置 seed（最低优先级，先放进去）──────────────────────────
	if !l.cfg.DisableBuiltinSeeds {
		for _, s := range builtinSeeds {
			merged[normalizeKey(s.Name)] = s
		}
	}

	// ── Step 2: 自动发现（deepdarkCTI）──────────────────────────────────
	if !l.cfg.DisableAutoDiscover {
		discovered := l.autoDiscover(ctx)
		l.log.Info("auto-discovery complete", "found", len(discovered))
		for _, ds := range discovered {
			key := normalizeKey(ds.Name)
			if _, exists := merged[key]; !exists {
				merged[key] = ds
			}
		}
	}

	// ── Step 3: 本地 config.yaml data_sources（覆盖同名）────────────────
	for _, ds := range localSources {
		if ds.RSSURL == "" {
			continue
		}
		merged[normalizeKey(ds.Name)] = ds
	}

	// ── Step 4: 远程 sources.yaml（最高优先级，完全覆盖）─────────────────
	if l.cfg.RemoteURL != "" {
		remote, err := l.fetchRemote(ctx, l.cfg.RemoteURL)
		if err != nil {
			l.log.Warn("remote sources fetch failed — continuing without it",
				"url", l.cfg.RemoteURL, "err", err)
		} else {
			l.log.Info("remote sources loaded",
				"url", l.cfg.RemoteURL,
				"count", len(remote.Sources),
				"updated", remote.Updated,
			)
			for _, e := range remote.Sources {
				if e.Name == "" || e.RSSURL == "" {
					continue
				}
				merged[normalizeKey(e.Name)] = config.DataSource{
					Name:    e.Name,
					RSSURL:  e.RSSURL,
					Enabled: e.Enabled,
				}
			}
		}
	}

	// 收集 enabled 的源
	var active []config.DataSource
	for _, ds := range merged {
		if ds.Enabled {
			active = append(active, ds)
		}
	}

	l.log.Info("sources merged", "total", len(merged), "enabled", len(active))

	// ── Step 5: 健康检查（并发 HEAD 探活）────────────────────────────────
	if l.cfg.HealthCheck && len(active) > 0 {
		active = l.healthFilter(ctx, active)
	}

	if len(active) == 0 {
		return nil, fmt.Errorf("no active RSS sources after loading")
	}

	return active, nil
}

// ---------------------------------------------------------------------------
// Auto-discovery: 解析 deepdarkCTI forum.md，批量探测 RSS
// ---------------------------------------------------------------------------

const deepdarkCTIURL = "https://raw.githubusercontent.com/fastfire/deepdarkCTI/main/forum.md"

// 论坛软件 RSS 路径规律（按优先级排序）
var rssPathCandidates = []string{
	"/forums/-/index.rss",          // XenForo
	"/syndication.php?limit=50",    // MyBB
	"/external.php?type=RSS2",      // vBulletin / MyBB
	"/rss/1-temy.xml/",             // IPBoard
	"/feed/",                       // WordPress / Discourse
	"/rss.xml",                     // Generic
	"/index.rss",                   // Generic
}

// 匹配 Markdown 表格中 ONLINE 的 clearnet 条目
var (
	reMDRow    = regexp.MustCompile(`\[([^\]]+)\]\((https://[^)]+)\)[^|]*ONLINE`)
	reOnionURL = regexp.MustCompile(`\.onion`)
)

func (l *Loader) autoDiscover(ctx context.Context) []config.DataSource {
	l.log.Info("auto-discovering sources from deepdarkCTI", "url", deepdarkCTIURL)

	// 拉取 forum.md
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, deepdarkCTIURL, nil)
	if err != nil {
		l.log.Warn("deepdarkCTI: build request failed", "err", err)
		return nil
	}
	req.Header.Set("User-Agent", "darkweb-tracker/1.0 (github.com)")

	resp, err := l.httpClient.Do(req)
	if err != nil {
		l.log.Warn("deepdarkCTI: fetch failed", "err", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		l.log.Warn("deepdarkCTI: bad status", "code", resp.StatusCode)
		return nil
	}

	// 解析所有 ONLINE clearnet URL
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		l.log.Warn("deepdarkCTI: read failed", "err", err)
		return nil
	}

	var candidates []struct{ name, base string }
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := scanner.Text()
		matches := reMDRow.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			name := strings.TrimSpace(m[1])
			baseURL := strings.TrimRight(m[2], "/")
			if reOnionURL.MatchString(baseURL) {
				continue // 跳过 .onion
			}
			if seen[baseURL] {
				continue
			}
			seen[baseURL] = true
			candidates = append(candidates, struct{ name, base string }{name, baseURL})
		}
	}

	l.log.Info("deepdarkCTI: parsed ONLINE clearnet forums", "count", len(candidates))

	// 并发探测 RSS（限制并发数避免被封）
	type probeResult struct {
		ds  config.DataSource
		ok  bool
	}

	results := make([]probeResult, len(candidates))
	sem := make(chan struct{}, 15) // 最多 15 个并发
	var wg sync.WaitGroup

	for i, c := range candidates {
		wg.Add(1)
		go func(idx int, name, base string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			rssURL := l.probeRSS(ctx, base)
			if rssURL != "" {
				results[idx] = probeResult{
					ok: true,
					ds: config.DataSource{
						Name:    sanitizeName(name),
						RSSURL:  rssURL,
						Enabled: true,
					},
				}
			}
		}(i, c.name, c.base)
	}

	wg.Wait()

	var discovered []config.DataSource
	for _, r := range results {
		if r.ok {
			discovered = append(discovered, r.ds)
		}
	}

	return discovered
}

// probeRSS 依次尝试各 RSS 路径，返回第一个有效的 RSS URL。
func (l *Loader) probeRSS(ctx context.Context, baseURL string) string {
	for _, path := range rssPathCandidates {
		url := baseURL + path
		if l.isValidRSS(ctx, url) {
			return url
		}
	}
	return ""
}

// isValidRSS 发送 GET 请求，检查响应是否为合法 RSS/Atom XML。
func (l *Loader) isValidRSS(ctx context.Context, url string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (compatible; DarkWebTracker/1.0; +https://github.com)")

	resp, err := l.httpClient.Do(req)
	if err != nil || resp.StatusCode >= 400 {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	defer resp.Body.Close()

	// 只读前 4KB 判断是否是 RSS
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	snippet := string(buf[:n])

	return strings.Contains(snippet, "<rss") ||
		strings.Contains(snippet, "<feed") ||
		strings.Contains(snippet, "<channel>") ||
		strings.Contains(snippet, "<?xml")
}

// ---------------------------------------------------------------------------
// Remote YAML fetch
// ---------------------------------------------------------------------------

func (l *Loader) fetchRemote(ctx context.Context, rawURL string) (*RemoteSourceFile, error) {
	url := expandGitHubURL(rawURL)

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "darkweb-tracker/1.0")

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var file RemoteSourceFile
	if err := yaml.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}

	return &file, nil
}

func expandGitHubURL(s string) string {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	parts := strings.SplitN(s, "/", 4)
	switch len(parts) {
	case 3:
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/%s",
			parts[0], parts[1], parts[2])
	case 4:
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
			parts[0], parts[1], parts[2], parts[3])
	default:
		return s
	}
}

// ---------------------------------------------------------------------------
// Health check
// ---------------------------------------------------------------------------

func (l *Loader) healthFilter(ctx context.Context, sources []config.DataSource) []config.DataSource {
	results := make([]config.DataSource, 0, len(sources))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for _, ds := range sources {
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
				l.log.Warn("health check failed — skipping",
					"name", src.Name, "url", src.RSSURL)
			}
		}(ds)
	}

	wg.Wait()
	l.log.Info("health check complete",
		"checked", len(sources), "alive", len(results),
		"dead", len(sources)-len(results))
	return results
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func normalizeKey(name string) string {
	return strings.ToLower(strings.NewReplacer(
		" ", "_", ".", "_", "-", "_", "(", "", ")", "",
	).Replace(strings.TrimSpace(name)))
}

// sanitizeName 清理从 Markdown 解析到的论坛名（去掉 Deep/Dark 后缀等）
func sanitizeName(name string) string {
	name = regexp.MustCompile(`(?i)\s*\((?:deep|dark|mirror|backup)[^)]*\)`).ReplaceAllString(name, "")
	return strings.TrimSpace(name)
}

// ---------------------------------------------------------------------------
// Built-in seeds（最后兜底，仅当发现为空时使用）
// ---------------------------------------------------------------------------

var builtinSeeds = []config.DataSource{
	{Name: "gerki", RSSURL: "https://forum.gerki.ws/forums/-/index.rss", Enabled: true},
	{Name: "blackbones", RSSURL: "https://blackbones.net/forums/-/index.rss", Enabled: true},
	{Name: "hard-tm", RSSURL: "https://hard-tm.su/forums/-/index.rss", Enabled: true},
	{Name: "mipped", RSSURL: "https://mipped.com/f/forums/-/index.rss", Enabled: true},
	{Name: "leakbase", RSSURL: "https://leakbase.la/forums/-/index.rss", Enabled: true},
	{Name: "dublikat", RSSURL: "https://at.dublikat.club/forums/-/index.rss", Enabled: true},
	{Name: "cardforum", RSSURL: "https://cardforum.cc/syndication.php?limit=50", Enabled: true},
	{Name: "ipbmafia", RSSURL: "https://ipbmafia.ru/rss/1-temy.xml/", Enabled: true},
}
