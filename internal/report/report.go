// Package report generates daily/weekly Markdown + HTML reports and RSS feeds.
package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"darkweb-tracker/internal/storage"
)

// Generator produces daily and weekly reports.
type Generator struct {
	db          *storage.DB
	archiveDir  string
	rssDir      string
	repoBaseURL string // used for RSS feed self-links, e.g. GitHub Pages URL
}

// New creates a Generator.
// archiveDir is where HTML/MD reports go; rssDir is where RSS XML goes.
func New(db *storage.DB, archiveDir, rssDir, repoBaseURL string) *Generator {
	return &Generator{
		db:          db,
		archiveDir:  archiveDir,
		rssDir:      rssDir,
		repoBaseURL: strings.TrimRight(repoBaseURL, "/"),
	}
}

// ---------------------------------------------------------------------------
// Daily report
// ---------------------------------------------------------------------------

// DailyResult holds the generated report metadata.
type DailyResult struct {
	Date         string
	MarkdownFile string
	HTMLFile     string
	TotalCount   int
}

func (g *Generator) Daily(ctx context.Context) (*DailyResult, error) {
	today := time.Now()
	dateStr := today.Format(time.DateOnly)

	dayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	dayEnd := dayStart.Add(24 * time.Hour)

	items, err := g.db.List(ctx, storage.QueryOptions{
		Since:     dayStart,
		Until:     dayEnd,
		OrderDesc: true,
	})
	if err != nil {
		return nil, fmt.Errorf("query daily items: %w", err)
	}

	bySource, err := g.db.CountBySource(ctx, dayStart, dayEnd)
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(g.archiveDir, dateStr)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	md := buildDailyMarkdown(dateStr, items, bySource)
	mdFile := filepath.Join(dir, "Daily_"+dateStr+".md")
	if err := os.WriteFile(mdFile, []byte(md), 0o644); err != nil {
		return nil, fmt.Errorf("write md: %w", err)
	}

	htmlFile := filepath.Join(dir, "Daily_"+dateStr+".html")
	if err := g.renderHTML(htmlFile, reportData{
		Title:      "数据泄露监控日报 " + dateStr,
		Date:       dateStr,
		UpdateTime: time.Now().Format(time.DateTime),
		Items:      items,
		BySource:   bySource,
		TotalCount: len(items),
	}); err != nil {
		return nil, fmt.Errorf("render html: %w", err)
	}

	return &DailyResult{
		Date:         dateStr,
		MarkdownFile: mdFile,
		HTMLFile:     htmlFile,
		TotalCount:   len(items),
	}, nil
}

// ---------------------------------------------------------------------------
// AllTime report — 全量历史数据，生成 index.html
// ---------------------------------------------------------------------------

// AllTimeResult holds metadata for the all-time report.
type AllTimeResult struct {
	HTMLFile   string
	TotalCount int
}

// AllTime generates an HTML report covering ALL items in the database.
// This is used as the main index.html so every hourly run reflects the full history.
func (g *Generator) AllTime(ctx context.Context) (*AllTimeResult, error) {
	items, err := g.db.List(ctx, storage.QueryOptions{
		OrderDesc: true, // newest first
	})
	if err != nil {
		return nil, fmt.Errorf("query all items: %w", err)
	}

	bySource, err := g.db.CountBySourceAll(ctx)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(g.archiveDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir archive: %w", err)
	}

	htmlFile := filepath.Join(filepath.Dir(g.archiveDir), "index.html")
	if err := g.renderHTML(htmlFile, reportData{
		Title:      "DarkWeb 数据泄露监控总览",
		Date:       "全量历史",
		UpdateTime: time.Now().Format(time.DateTime),
		Items:      items,
		BySource:   bySource,
		TotalCount: len(items),
	}); err != nil {
		return nil, fmt.Errorf("render all-time html: %w", err)
	}

	return &AllTimeResult{
		HTMLFile:   htmlFile,
		TotalCount: len(items),
	}, nil
}

// ---------------------------------------------------------------------------
// Weekly report
// ---------------------------------------------------------------------------

type WeeklyResult struct {
	StartDate    string
	EndDate      string
	MarkdownFile string
	HTMLFile     string
	TotalCount   int
}

func (g *Generator) Weekly(ctx context.Context) (*WeeklyResult, error) {
	now := time.Now()
	// Week starts on Monday.
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	weekStart := now.AddDate(0, 0, -(weekday - 1))
	weekStart = time.Date(weekStart.Year(), weekStart.Month(), weekStart.Day(), 0, 0, 0, 0, weekStart.Location())
	weekEnd := weekStart.AddDate(0, 0, 7)

	startStr := weekStart.Format(time.DateOnly)
	endStr := weekEnd.AddDate(0, 0, -1).Format(time.DateOnly)

	items, err := g.db.List(ctx, storage.QueryOptions{
		Since:     weekStart,
		Until:     weekEnd,
		OrderDesc: true,
	})
	if err != nil {
		return nil, fmt.Errorf("query weekly items: %w", err)
	}

	bySource, err := g.db.CountBySource(ctx, weekStart, weekEnd)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(g.archiveDir, 0o755); err != nil {
		return nil, err
	}

	slug := fmt.Sprintf("Weekly_%s_%s", startStr, endStr)
	md := buildWeeklyMarkdown(startStr, endStr, items, bySource)
	mdFile := filepath.Join(g.archiveDir, slug+".md")
	if err := os.WriteFile(mdFile, []byte(md), 0o644); err != nil {
		return nil, fmt.Errorf("write weekly md: %w", err)
	}

	htmlFile := filepath.Join(g.archiveDir, slug+".html")
	if err := g.renderHTML(htmlFile, reportData{
		Title:      fmt.Sprintf("数据泄露监控周报 %s – %s", startStr, endStr),
		Date:       fmt.Sprintf("%s – %s", startStr, endStr),
		UpdateTime: time.Now().Format(time.DateTime),
		Items:      items,
		BySource:   bySource,
		TotalCount: len(items),
	}); err != nil {
		return nil, fmt.Errorf("render weekly html: %w", err)
	}

	return &WeeklyResult{
		StartDate:    startStr,
		EndDate:      endStr,
		MarkdownFile: mdFile,
		HTMLFile:     htmlFile,
		TotalCount:   len(items),
	}, nil
}

// ---------------------------------------------------------------------------
// RSS feed generation
// ---------------------------------------------------------------------------

// GenerateRSS writes an RSS 2.0 XML file and updates the "latest" symlink file.
// feedType is "daily" or "weekly".
func (g *Generator) GenerateRSS(ctx context.Context, feedType string) (string, error) {
	if err := os.MkdirAll(g.rssDir, 0o755); err != nil {
		return "", err
	}

	now := time.Now()
	var items []storage.Item
	var filename, latestFile, feedTitle, feedDesc, feedLink string

	switch feedType {
	case "daily":
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		dayEnd := dayStart.Add(24 * time.Hour)
		dateStr := now.Format(time.DateOnly)

		var err error
		items, err = g.db.List(ctx, storage.QueryOptions{Since: dayStart, Until: dayEnd, OrderDesc: true})
		if err != nil {
			return "", err
		}

		filename = fmt.Sprintf("daily_rss_%s.xml", dateStr)
		latestFile = "latest_daily_rss.xml"
		feedTitle = "数据泄露监控日报 RSS " + dateStr
		feedDesc = fmt.Sprintf("每日数据泄露监控RSS feed，包含%s的最新数据泄露信息", dateStr)
		feedLink = fmt.Sprintf("%s/rss/%s", g.repoBaseURL, filename)

	case "weekly":
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		weekStart := now.AddDate(0, 0, -(weekday - 1))
		weekStart = time.Date(weekStart.Year(), weekStart.Month(), weekStart.Day(), 0, 0, 0, 0, weekStart.Location())
		weekEnd := weekStart.AddDate(0, 0, 7)
		startStr := weekStart.Format(time.DateOnly)
		endStr := weekEnd.AddDate(0, 0, -1).Format(time.DateOnly)

		var err error
		items, err = g.db.List(ctx, storage.QueryOptions{Since: weekStart, Until: weekEnd, OrderDesc: true})
		if err != nil {
			return "", err
		}

		filename = fmt.Sprintf("weekly_rss_%s_%s.xml", startStr, endStr)
		latestFile = "latest_weekly_rss.xml"
		feedTitle = fmt.Sprintf("数据泄露监控周报 RSS %s - %s", startStr, endStr)
		feedDesc = fmt.Sprintf("每周数据泄露监控RSS feed，包含%s到%s的最新数据泄露信息", startStr, endStr)
		feedLink = fmt.Sprintf("%s/rss/%s", g.repoBaseURL, filename)

	default:
		return "", fmt.Errorf("unknown feed type: %s", feedType)
	}

	xml := buildRSS(feedTitle, feedDesc, feedLink, g.repoBaseURL, items)
	outPath := filepath.Join(g.rssDir, filename)
	if err := os.WriteFile(outPath, []byte(xml), 0o644); err != nil {
		return "", fmt.Errorf("write rss: %w", err)
	}

	// Also write as "latest_*".
	latestPath := filepath.Join(g.rssDir, latestFile)
	if err := os.WriteFile(latestPath, []byte(xml), 0o644); err != nil {
		return "", fmt.Errorf("write latest rss: %w", err)
	}

	return outPath, nil
}

// ---------------------------------------------------------------------------
// Markdown builders
// ---------------------------------------------------------------------------

func buildDailyMarkdown(date string, items []storage.Item, bySource map[string]int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# 数据泄露监控日报 %s\n\n", date))
	sb.WriteString(fmt.Sprintf("共收集到 **%d** 条数据泄露相关信息\n\n", len(items)))
	sb.WriteString(fmt.Sprintf("最后更新时间：%s\n\n", time.Now().Format(time.DateTime)))

	sb.WriteString("## 今日统计\n\n### 按数据源统计\n\n")
	for src, cnt := range bySource {
		sb.WriteString(fmt.Sprintf("- %s: %d 条\n", src, cnt))
	}
	sb.WriteString("\n## 今日详情\n\n")

	for _, it := range items {
		sb.WriteString(fmt.Sprintf("### [%s](%s)\n\n", it.Title, it.Link))
		sb.WriteString(fmt.Sprintf("- **来源**: %s\n", it.SiteName))
		sb.WriteString(fmt.Sprintf("- **发现时间**: %s\n\n", it.CreatedAt.Format(time.DateTime)))
	}

	sb.WriteString("\n---\n\nPower By DarkWeb Forums Tracker\n")
	return sb.String()
}

func buildWeeklyMarkdown(start, end string, items []storage.Item, bySource map[string]int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# 数据泄露监控周报 %s – %s\n\n", start, end))
	sb.WriteString(fmt.Sprintf("共收集到 **%d** 条数据泄露相关信息\n\n", len(items)))
	sb.WriteString(fmt.Sprintf("最后更新时间：%s\n\n", time.Now().Format(time.DateTime)))

	sb.WriteString("## 本周统计\n\n### 按数据源统计\n\n")
	for src, cnt := range bySource {
		sb.WriteString(fmt.Sprintf("- %s: %d 条\n", src, cnt))
	}
	sb.WriteString("\n## 本周详情\n\n")

	for _, it := range items {
		sb.WriteString(fmt.Sprintf("### [%s](%s)\n\n", it.Title, it.Link))
		sb.WriteString(fmt.Sprintf("- **来源**: %s\n", it.SiteName))
		sb.WriteString(fmt.Sprintf("- **发现时间**: %s\n\n", it.CreatedAt.Format(time.DateTime)))
	}

	sb.WriteString("\n---\n\nPower By DarkWeb Forums Tracker\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// RSS XML builder
// ---------------------------------------------------------------------------

func buildRSS(title, desc, selfLink, siteURL string, items []storage.Item) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version='1.0' encoding='UTF-8'?>` + "\n")
	sb.WriteString(`<rss version='2.0' xmlns:atom='http://www.w3.org/2005/Atom'>` + "\n")
	sb.WriteString("  <channel>\n")
	sb.WriteString(fmt.Sprintf("    <title>%s</title>\n", xmlEscape(title)))
	sb.WriteString(fmt.Sprintf("    <description>%s</description>\n", xmlEscape(desc)))
	sb.WriteString(fmt.Sprintf("    <link>%s</link>\n", siteURL))
	sb.WriteString(fmt.Sprintf("    <atom:link href='%s' rel='self' type='application/rss+xml'/>\n", selfLink))
	sb.WriteString("    <language>zh-CN</language>\n")
	sb.WriteString(fmt.Sprintf("    <lastBuildDate>%s</lastBuildDate>\n", time.Now().UTC().Format(time.RFC1123Z)))
	sb.WriteString("    <ttl>60</ttl>\n\n")

	for _, it := range items {
		pub := it.CreatedAt.UTC().Format(time.RFC1123Z)
		sb.WriteString("    <item>\n")
		sb.WriteString(fmt.Sprintf("      <title>%s</title>\n", xmlEscape(it.Title)))
		sb.WriteString(fmt.Sprintf("      <link>%s</link>\n", it.Link))
		sb.WriteString(fmt.Sprintf("      <description>%s</description>\n", xmlEscape(it.Title)))
		sb.WriteString(fmt.Sprintf("      <pubDate>%s</pubDate>\n", pub))
		sb.WriteString(fmt.Sprintf("      <guid isPermaLink='false'>%s_%s</guid>\n",
			it.Link, it.CreatedAt.Format("20060102150405")))
		sb.WriteString("    </item>\n")
	}

	sb.WriteString("  </channel>\n</rss>")
	return sb.String()
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}

// ---------------------------------------------------------------------------
// HTML template rendering
// ---------------------------------------------------------------------------

type reportData struct {
	Title      string
	Date       string
	UpdateTime string
	Items      []storage.Item
	BySource   map[string]int
	TotalCount int
}

const htmlTmpl = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>{{ .Title }}</title>
<style>
:root{--bg:#0a0e17;--fg:#00ff41;--fg2:#00e0ff;--card:#121721;--border:#2d3748;--hover:rgba(0,255,65,.06);--text-muted:#8b949e;--transition:all 0.3s ease}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:'Courier New',monospace;background:var(--bg);color:var(--fg2);max-width:1400px;margin:0 auto;padding:0 24px 24px;min-height:100vh;line-height:1.6}
/* Navbar */
nav{position:sticky;top:0;background:rgba(10,14,23,.9);backdrop-filter:blur(10px);border-bottom:1px solid var(--border);padding:16px 0;margin-bottom:24px;z-index:100;display:flex;justify-content:space-between;align-items:center}
.nav-brand{color:var(--fg);font-weight:700;font-size:1.1rem;text-shadow:0 0 5px rgba(0,255,65,.5)}
.nav-links{display:flex;gap:20px}
.nav-links a{color:var(--fg2);text-decoration:none;font-size:.9rem;text-transform:uppercase;letter-spacing:.05em;transition:var(--transition)}
.nav-links a:hover{color:var(--fg);text-shadow:0 0 8px rgba(0,255,65,.5)}
/* Header */
header{border:1px solid var(--fg);padding:30px;text-align:center;margin-bottom:32px;box-shadow:0 0 20px rgba(0,255,65,.15),inset 0 0 20px rgba(0,255,65,.05);background:linear-gradient(180deg,rgba(18,23,33,.8) 0%,rgba(10,14,23,.9) 100%);border-radius:8px}
h1{color:var(--fg);text-shadow:0 0 10px rgba(0,255,65,.4);font-size:2.2rem;margin-bottom:10px;letter-spacing:.05em}
.subtitle{color:var(--text-muted);font-size:.95rem}
/* Section Titles */
h2{color:var(--fg);font-size:1.4rem;margin-bottom:16px;padding-bottom:8px;border-bottom:1px dashed var(--border);display:flex;align-items:center;gap:10px}
h2::before{content:'>';color:var(--fg2)}
/* Stats */
.stats-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:16px;margin-bottom:40px}
.stat-card{background:var(--card);border:1px solid var(--border);padding:20px;border-radius:8px;transition:var(--transition);position:relative;overflow:hidden}
.stat-card::before{content:'';position:absolute;top:0;left:0;width:4px;height:100%;background:var(--fg);opacity:.5}
.stat-card:hover{border-color:var(--fg);box-shadow:0 4px 12px rgba(0,255,65,.1);transform:translateY(-2px)}
.stat-label{font-size:.8rem;color:var(--text-muted);margin-bottom:8px;text-transform:uppercase;letter-spacing:.05em}
.stat-value{font-size:1.8rem;color:var(--fg);font-weight:700;text-shadow:0 0 8px rgba(0,255,65,.3)}
/* Chart */
.chart-container{background:var(--card);border:1px solid var(--border);border-radius:8px;padding:24px;margin-bottom:40px}
.bar-row{display:flex;align-items:center;margin-bottom:12px;gap:16px}
.bar-label{width:120px;text-align:right;font-size:.85rem;color:var(--fg2);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.bar-track{flex:1;height:24px;background:rgba(255,255,255,.05);border-radius:4px;overflow:hidden;position:relative}
.bar-fill{height:100%;background:linear-gradient(90deg,rgba(0,224,255,.6) 0%,rgba(0,255,65,.8) 100%);width:0%;transition:width 1s cubic-bezier(.4,0,.2,1);display:flex;align-items:center;justify-content:flex-end;padding-right:8px;box-shadow:inset -2px 0 5px rgba(0,0,0,.2)}
.bar-value{font-size:.75rem;color:#fff;font-weight:700;text-shadow:1px 1px 2px rgba(0,0,0,.8)}
/* Toolbar */
.toolbar{display:flex;gap:16px;margin-bottom:20px;flex-wrap:wrap;align-items:center;background:var(--card);padding:16px;border-radius:8px;border:1px solid var(--border)}
.toolbar input,.toolbar select{background:var(--bg);border:1px solid var(--border);color:var(--fg2);padding:10px 16px;font-family:inherit;font-size:.9rem;border-radius:4px;outline:none;flex:1;min-width:200px;transition:var(--transition)}
.toolbar input:focus,.toolbar select:focus{border-color:var(--fg);box-shadow:0 0 8px rgba(0,255,65,.2)}
.toolbar select option{background:var(--bg)}
.count-badge{color:var(--fg);font-size:.85rem;white-space:nowrap;font-weight:700}
/* Table */
.table-wrap{overflow-x:auto;background:var(--card);border-radius:8px;border:1px solid var(--border);margin-bottom:24px}
table{width:100%;border-collapse:collapse;font-size:.9rem}
th{background:rgba(10,14,23,.8);color:var(--fg);padding:14px 16px;border-bottom:2px solid var(--border);text-align:left;white-space:nowrap;cursor:pointer;user-select:none;transition:var(--transition);position:sticky;top:60px;z-index:1}
th:hover{background:rgba(0,255,65,.1)}
th.sort-asc::after{content:' ▲';font-size:.7rem}
th.sort-desc::after{content:' ▼';font-size:.7rem}
td{padding:14px 16px;border-bottom:1px solid var(--border);vertical-align:top}
td.num{color:var(--text-muted);text-align:right;width:50px}
td.src{white-space:nowrap}
td.time{white-space:nowrap;color:var(--text-muted);font-size:.85rem}
td a{color:var(--fg2);text-decoration:none;font-weight:700;transition:var(--transition);word-break:break-all}
td a:hover{color:var(--fg);text-shadow:0 0 5px rgba(0,255,65,.4);text-decoration:underline}
/* Expandable Rows */
tr.data-row{cursor:pointer;transition:var(--transition)}
tr.data-row:hover td{background:var(--hover)}
tr.hidden{display:none !important}
.content-preview{font-size:.8rem;color:var(--text-muted);margin-top:6px;display:block;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;max-width:600px}
.content-full{max-height:0;overflow:hidden;transition:max-height .4s cubic-bezier(0,1,0,1);background:rgba(0,0,0,.2);border-radius:4px;margin-top:0;padding:0 12px;font-size:.85rem;color:#a0aec0;white-space:pre-wrap;word-break:break-all}
tr.expanded .content-full{max-height:2000px;padding:12px;margin-top:10px;border-left:2px solid var(--fg2);transition:max-height .5s ease-in-out}
tr.expanded .content-preview{display:none}
/* Site tier badges */
.badge{display:inline-block;padding:4px 8px;border-radius:4px;font-size:.75rem;font-weight:700;letter-spacing:.05em;text-transform:uppercase}
.t1{background:rgba(255,68,68,.1);color:#ff6b6b;border:1px solid #ff4444}
.t2{background:rgba(255,140,0,.1);color:#ffa500;border:1px solid #ff8c00}
.t3{background:rgba(68,170,68,.1);color:#66cc66;border:1px solid #44aa44}
.t4{background:rgba(74,110,216,.1);color:#7b9ef5;border:1px solid #4a6ed8}
.t-ransom{background:rgba(153,50,204,.1);color:#da70d6;border:1px solid #9932cc}
.t-intel{background:rgba(32,178,170,.1);color:#40e0d0;border:1px solid #20b2aa}
/* Pagination */
.pagination{display:flex;gap:8px;justify-content:center;align-items:center;margin:24px 0;flex-wrap:wrap}
.pagination button{background:var(--card);border:1px solid var(--border);color:var(--fg2);padding:8px 16px;font-family:inherit;font-size:.9rem;cursor:pointer;border-radius:4px;transition:var(--transition)}
.pagination button:hover:not(:disabled){border-color:var(--fg);color:var(--fg);box-shadow:0 0 8px rgba(0,255,65,.2)}
.pagination button.active{background:var(--fg);color:var(--bg);border-color:var(--fg);font-weight:700}
.pagination button:disabled{opacity:.4;cursor:not-allowed}
.page-info{color:var(--text-muted);font-size:.85rem;text-align:center;margin-top:12px}
/* Footer */
footer{margin-top:60px;text-align:center;color:var(--text-muted);font-size:.85rem;border-top:1px solid var(--border);padding-top:24px;padding-bottom:24px}
</style>
</head>
<body>

<nav>
  <div class="nav-brand">DWT // SYSTEM</div>
  <div class="nav-links">
    <a href="#stats">统计</a>
    <a href="#chart">图表</a>
    <a href="#data">数据列表</a>
  </div>
</nav>

<header>
  <h1>{{ .Title }}</h1>
  <p class="subtitle">最后更新：{{ .UpdateTime }}</p>
</header>

<h2 id="stats">概览统计</h2>
<div class="stats-grid">
  <div class="stat-card">
    <div class="stat-label">新增数量</div>
    <div class="stat-value" id="total-count">{{ .TotalCount }}</div>
  </div>
  <div class="stat-card">
    <div class="stat-label">监控来源数量</div>
    <div class="stat-value" id="source-count">0</div>
  </div>
  <div class="stat-card">
    <div class="stat-label">最活跃来源</div>
    <div class="stat-value" id="top-source" style="font-size:1.4rem;word-break:break-all;">-</div>
  </div>
</div>

<h2 id="chart">来源分布</h2>
<div class="chart-container" id="source-chart">
  {{ range $src, $cnt := .BySource }}
  <div class="bar-row" data-src="{{ $src }}" data-cnt="{{ $cnt }}">
    <div class="bar-label" title="{{ $src }}">{{ $src }}</div>
    <div class="bar-track">
      <div class="bar-fill"><span class="bar-value">{{ $cnt }}</span></div>
    </div>
  </div>
  {{ end }}
</div>

<h2 id="data">数据列表</h2>
<div class="toolbar">
  <input type="text" id="search" placeholder="🔍 搜索标题/内容..." oninput="filterTable()">
  <select id="src-filter" onchange="filterTable()">
    <option value="">全部来源</option>
    {{ range $src, $cnt := .BySource }}
    <option value="{{ $src }}">{{ $src }} ({{ $cnt }})</option>
    {{ end }}
  </select>
  <span class="count-badge" id="filter-count"></span>
</div>

<div class="table-wrap">
<table id="main-table">
<thead>
  <tr>
    <th>#</th>
    <th onclick="sortTable(1, 'string')" title="点击排序">标题 / 内容摘要 ↕</th>
    <th onclick="sortTable(2, 'string')" title="点击排序">来源 ↕</th>
    <th onclick="sortTable(3, 'string')" title="点击排序">发现时间 ↕</th>
  </tr>
</thead>
<tbody>
{{ range $i, $it := .Items }}
<tr class="data-row" data-src="{{ $it.SiteName }}" data-title="{{ $it.Title }}" data-content="{{ $it.Content }}" data-time="{{ $it.CreatedAt.Format "2006-01-02 15:04:05" }}" onclick="this.classList.toggle('expanded')">
  <td class="num">{{ inc $i }}</td>
  <td class="title-cell">
    <a href="{{ $it.Link }}" target="_blank" rel="noopener noreferrer" onclick="event.stopPropagation()">{{ $it.Title }}</a>
    {{ if $it.Content }}
    <span class="content-preview">{{ truncate64 $it.Content }}</span>
    <div class="content-full" onclick="event.stopPropagation()">{{ $it.Content }}</div>
    {{ end }}
  </td>
  <td class="src"><span class="badge {{ siteTier $it.SiteName }}">{{ $it.SiteName }}</span></td>
  <td class="time">{{ $it.CreatedAt.Format "01-02 15:04" }}</td>
</tr>
{{ end }}
</tbody>
</table>
</div>

<div class="pagination" id="pagination"></div>
<p class="page-info" id="page-info"></p>

<footer><p>DarkWeb Forums Tracker · {{ .TotalCount }} 条记录 · {{ .Date }}</p></footer>

<script>
const PAGE_SIZE = 100;
let currentPage = 1;
let visibleRows = [];
let sortCol = -1;
let sortAsc = true;

function initStatsAndChart() {
  const rows = document.querySelectorAll('.bar-row');
  let maxCnt = 0;
  let topSrc = '-';
  let topCnt = -1;
  
  document.getElementById('source-count').textContent = rows.length;
  
  rows.forEach(r => {
    const cnt = parseInt(r.dataset.cnt, 10);
    if (cnt > maxCnt) maxCnt = cnt;
    if (cnt > topCnt) { topCnt = cnt; topSrc = r.dataset.src; }
  });
  
  document.getElementById('top-source').textContent = topSrc;
  
  setTimeout(() => {
    rows.forEach(r => {
      const cnt = parseInt(r.dataset.cnt, 10);
      const pct = maxCnt > 0 ? (cnt / maxCnt * 100) : 0;
      r.querySelector('.bar-fill').style.width = Math.max(pct, 2) + '%';
    });
  }, 100);
}

function filterTable() {
  const q = document.getElementById('search').value.toLowerCase();
  const src = document.getElementById('src-filter').value;
  const rows = document.querySelectorAll('#main-table tbody tr.data-row');
  visibleRows = [];
  rows.forEach(r => {
    const title = r.dataset.title.toLowerCase();
    const content = (r.dataset.content || '').toLowerCase();
    const rsrc  = r.dataset.src;
    const show  = (!q || title.includes(q) || content.includes(q)) && (!src || rsrc === src);
    r.classList.toggle('hidden', !show);
    if (show) visibleRows.push(r);
  });
  document.getElementById('filter-count').textContent =
    visibleRows.length < rows.length ? visibleRows.length + ' 条匹配' : '';
  
  if (sortCol !== -1) {
    doSort();
  } else {
    currentPage = 1;
    paginate();
  }
}

function sortTable(colIdx, type) {
  const ths = document.querySelectorAll('#main-table th');
  if (sortCol === colIdx) {
    sortAsc = !sortAsc;
  } else {
    sortCol = colIdx;
    sortAsc = true;
  }
  
  ths.forEach((th, i) => {
    th.classList.remove('sort-asc', 'sort-desc');
    if (i === colIdx) {
      th.classList.add(sortAsc ? 'sort-asc' : 'sort-desc');
    }
  });
  
  doSort();
}

function doSort() {
  if (sortCol === -1) return;
  
  visibleRows.sort((a, b) => {
    let valA, valB;
    if (sortCol === 1) {
      valA = a.dataset.title.toLowerCase();
      valB = b.dataset.title.toLowerCase();
    } else if (sortCol === 2) {
      valA = a.dataset.src.toLowerCase();
      valB = b.dataset.src.toLowerCase();
    } else if (sortCol === 3) {
      valA = a.dataset.time;
      valB = b.dataset.time;
    }
    
    if (valA < valB) return sortAsc ? -1 : 1;
    if (valA > valB) return sortAsc ? 1 : -1;
    return 0;
  });
  
  const tbody = document.querySelector('#main-table tbody');
  visibleRows.forEach((r, i) => {
    tbody.appendChild(r);
    // 更新序号列，避免排序后序号和行内容错位
    const numCell = r.querySelector('td.num');
    if (numCell) numCell.textContent = i + 1;
  });
  
  currentPage = 1;
  paginate();
}

function paginate() {
  const total = visibleRows.length;
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  currentPage = Math.min(currentPage, pages);
  const start = (currentPage - 1) * PAGE_SIZE;
  const end   = Math.min(start + PAGE_SIZE, total);

  visibleRows.forEach((r, i) => {
    r.style.display = (i >= start && i < end) ? '' : 'none';
    r.classList.remove('expanded');
  });

  const pg = document.getElementById('pagination');
  pg.innerHTML = '';
  const btn = (label, page, disabled, active) => {
    const b = document.createElement('button');
    b.textContent = label;
    if (disabled) b.disabled = true;
    if (active) b.classList.add('active');
    b.onclick = () => { currentPage = page; paginate(); };
    return b;
  };
  pg.appendChild(btn('«', 1, currentPage===1));
  pg.appendChild(btn('‹', currentPage-1, currentPage===1));

  let lo = Math.max(1, currentPage-2), hi = Math.min(pages, lo+4);
  lo = Math.max(1, hi-4);
  for (let p=lo; p<=hi; p++) pg.appendChild(btn(p, p, false, p===currentPage));

  pg.appendChild(btn('›', currentPage+1, currentPage===pages));
  pg.appendChild(btn('»', pages, currentPage===pages));

  document.getElementById('page-info').textContent =
    total > 0 ? '第 ' + currentPage + ' / ' + pages + ' 页，共 ' + total + ' 条' : '无匹配结果';
}

document.querySelectorAll('a[href^="#"]').forEach(anchor => {
  anchor.addEventListener('click', function (e) {
    e.preventDefault();
    const target = document.querySelector(this.getAttribute('href'));
    if (target) {
      const headerOffset = 80;
      const elementPosition = target.getBoundingClientRect().top;
      const offsetPosition = elementPosition + window.pageYOffset - headerOffset;
      window.scrollTo({ top: offsetPosition, behavior: 'smooth' });
    }
  });
});

document.addEventListener('DOMContentLoaded', () => {
  initStatsAndChart();
  visibleRows = Array.from(document.querySelectorAll('#main-table tbody tr.data-row'));
  paginate();
});
</script>
</body>
</html>`

var tmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"inc": func(i int) int { return i + 1 },
	// truncate64 returns up to 64 runes for the content preview line.
	"truncate64": func(s string) string {
		runes := []rune(s)
		if len(runes) <= 64 {
			return s
		}
		return string(runes[:64]) + "…"
	},
	// siteTier maps a site name to its CSS tier badge class.
	"siteTier": func(name string) string {
		n := strings.ToLower(name)
		switch {
		case strings.Contains(n, "breachforum"), strings.Contains(n, "leakbase"),
			strings.Contains(n, "thejavasea"), strings.Contains(n, "exploit-in"),
			strings.Contains(n, "xss-is"):
			return "t1"
		case strings.Contains(n, "probiv"), strings.Contains(n, "darkforum"),
			strings.Contains(n, "altenens"), strings.Contains(n, "nulled"),
			strings.Contains(n, "leakforum"), strings.Contains(n, "mipped"),
			strings.Contains(n, "hard-tm"), strings.Contains(n, "dublikat"),
			strings.Contains(n, "leetforum"), strings.Contains(n, "in4"),
			strings.Contains(n, "ipbmafia"):
			return "t2"
		case strings.Contains(n, "card"), strings.Contains(n, "carding"),
			strings.Contains(n, "mmgp"), strings.Contains(n, "ezcarder"):
			return "t3"
		case strings.Contains(n, "ransom"), strings.Contains(n, "ransomware"):
			return "t-ransom"
		case strings.Contains(n, "hibp"), strings.Contains(n, "haveibeen"),
			strings.Contains(n, "ddosecret"), strings.Contains(n, "ransomfeed"),
			strings.Contains(n, "ransomwatch"):
			return "t-intel"
		default:
			return "t4"
		}
	},
}).Parse(htmlTmpl))

func (g *Generator) renderHTML(path string, data reportData) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}
