// Package report generates daily/weekly Markdown + HTML reports and RSS feeds.
package report

import (
	"context"
	"fmt"
	"html/template" // was text/template — XSS fix
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/feeds"

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
	// Use Beijing time (UTC+8) for "today" so reports align with user's calendar.
	// List/TotalCount convert to UTC internally for SQLite comparison.
	cst := time.FixedZone("CST", 8*3600)
	today := time.Now().UTC().Add(8 * time.Hour)
	dateStr := today.Format(time.DateOnly)

	dayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, cst)
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

	xmlStr, err := buildRSS(feedTitle, feedDesc, feedLink, g.repoBaseURL, items)
	if err != nil {
		return "", fmt.Errorf("build rss: %w", err)
	}
	outPath := filepath.Join(g.rssDir, filename)
	if err := os.WriteFile(outPath, []byte(xmlStr), 0o644); err != nil {
		return "", fmt.Errorf("write rss: %w", err)
	}

	// Also write as "latest_*".
	latestPath := filepath.Join(g.rssDir, latestFile)
	if err := os.WriteFile(latestPath, []byte(xmlStr), 0o644); err != nil {
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
// RSS XML builder — uses gorilla/feeds for correct escaping and RFC compliance
// ---------------------------------------------------------------------------

// buildRSS generates a valid RSS 2.0 feed from the given items.
// Returns the XML string or an error.
func buildRSS(title, desc, selfLink, siteURL string, items []storage.Item) (string, error) {
	feed := &feeds.Feed{
		Title:       title,
		Link:        &feeds.Link{Href: siteURL, Rel: "alternate"},
		Description: desc,
		Created:     time.Now(),
	}

	for _, it := range items {
		body := it.Content
		if it.FullContent != "" {
			body = it.FullContent
		}
		feed.Items = append(feed.Items, &feeds.Item{
			Title:       it.Title,
			Link:        &feeds.Link{Href: it.Link},
			Description: body,
			Created:     it.CreatedAt,
			Id:          fmt.Sprintf("%s_%s", it.Link, it.CreatedAt.Format("20060102150405")),
		})
	}

	return feed.ToRss()
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
/* ── RESET & BASE ─────────────────────────────────────────────────────────── */
:root{
  --bg:#050709;--bg2:#0b0f17;--bg3:#111722;
  --fg:#39ff14;--fg-dim:#1a8c00;--fg2:#00d4ff;--fg2-dim:#006680;
  --red:#ff2222;--orange:#ff6600;--yellow:#ffd700;--purple:#cc00ff;
  --border:#1a2535;--border-bright:#2a3f5a;
  --muted:#3d5066;--muted2:#566880;
  --row-even:#080c12;--row-odd:#0b1018;--row-hover:#0f1e2e;
  --font:'Courier New',Courier,'Lucida Console',monospace;
}
*{box-sizing:border-box;margin:0;padding:0}
html{scroll-behavior:smooth}
body{font-family:var(--font);background:var(--bg);color:var(--fg2);min-height:100vh;font-size:13px;line-height:1.5;overflow-x:hidden}

/* ── SCANLINE OVERLAY ─────────────────────────────────────────────────────── */
body::before{
  content:'';position:fixed;top:0;left:0;width:100%;height:100%;pointer-events:none;z-index:9999;
  background:repeating-linear-gradient(0deg,transparent,transparent 2px,rgba(0,0,0,.08) 2px,rgba(0,0,0,.08) 4px);
}

/* ── NAVBAR ───────────────────────────────────────────────────────────────── */
nav{
  position:sticky;top:0;z-index:200;
  background:rgba(5,7,9,.97);border-bottom:1px solid var(--fg-dim);
  padding:0 24px;display:flex;align-items:center;gap:0;height:38px;
  font-size:11px;letter-spacing:.08em;
}
.nav-logo{color:var(--fg);font-weight:700;font-size:12px;margin-right:24px;text-shadow:0 0 8px var(--fg);white-space:nowrap}
.nav-logo span{color:var(--red);animation:blink 1.2s step-end infinite}
.nav-tab{color:var(--muted2);text-decoration:none;padding:0 14px;height:38px;display:flex;align-items:center;border-right:1px solid var(--border);transition:color .15s,background .15s}
.nav-tab:first-of-type{border-left:1px solid var(--border)}
.nav-tab:hover{color:var(--fg);background:rgba(57,255,20,.06)}
.nav-sep{flex:1}
.nav-meta{color:var(--muted);font-size:10px;white-space:nowrap}
@keyframes blink{0%,100%{opacity:1}50%{opacity:0}}

/* ── HERO BANNER ──────────────────────────────────────────────────────────── */
.hero{
  border-bottom:1px solid var(--fg-dim);
  padding:20px 24px 16px;
  background:linear-gradient(180deg,rgba(57,255,20,.04) 0%,transparent 100%);
  position:relative;overflow:hidden;
}
.hero::after{
  content:'';position:absolute;bottom:0;left:0;right:0;height:1px;
  background:linear-gradient(90deg,transparent,var(--fg),transparent);
  animation:scanh 3s linear infinite;
}
@keyframes scanh{0%{opacity:0}50%{opacity:1}100%{opacity:0}}
.hero-pre{color:var(--muted2);font-size:10px;margin-bottom:4px;letter-spacing:.1em}
.hero-title{color:var(--fg);font-size:20px;font-weight:700;text-shadow:0 0 12px rgba(57,255,20,.5);letter-spacing:.03em;margin-bottom:6px}
.hero-sub{color:var(--muted2);font-size:11px}
.hero-sub b{color:var(--fg2)}

/* ── STATS BAR ────────────────────────────────────────────────────────────── */
.stats-bar{
  display:flex;flex-wrap:wrap;gap:0;border-bottom:1px solid var(--border);
  background:var(--bg2);
}
.stat-box{
  flex:1;min-width:140px;padding:12px 20px;border-right:1px solid var(--border);
  position:relative;
}
.stat-box:last-child{border-right:none}
.stat-box-label{font-size:9px;color:var(--muted);letter-spacing:.12em;text-transform:uppercase;margin-bottom:4px}
.stat-box-val{font-size:22px;font-weight:700;color:var(--fg);text-shadow:0 0 6px rgba(57,255,20,.4);line-height:1}
.stat-box-sub{font-size:10px;color:var(--muted2);margin-top:3px}

/* ── CHART SECTION ────────────────────────────────────────────────────────── */
.section{padding:16px 24px;border-bottom:1px solid var(--border)}
.section-head{
  font-size:10px;color:var(--muted);letter-spacing:.15em;text-transform:uppercase;
  margin-bottom:12px;display:flex;align-items:center;gap:8px
}
.section-head::before{content:'//';color:var(--fg-dim)}
.bar-grid{display:flex;flex-direction:column;gap:6px}
.bar-row{display:flex;align-items:center;gap:10px;font-size:11px}
.bar-label{width:130px;text-align:right;color:var(--muted2);white-space:nowrap;overflow:hidden;text-overflow:ellipsis;flex-shrink:0}
.bar-track{flex:1;height:14px;background:rgba(255,255,255,.03);border:1px solid var(--border);position:relative;overflow:hidden}
.bar-fill{
  height:100%;width:0%;
  background:repeating-linear-gradient(90deg,var(--fg-dim) 0px,var(--fg-dim) 4px,transparent 4px,transparent 8px);
  transition:width 1.2s cubic-bezier(.4,0,.2,1);position:relative;
}
.bar-fill::after{content:'';position:absolute;right:0;top:0;bottom:0;width:2px;background:var(--fg);box-shadow:0 0 6px var(--fg)}
.bar-cnt{width:40px;color:var(--fg);font-size:10px;text-align:right;flex-shrink:0}

/* ── TOOLBAR ──────────────────────────────────────────────────────────────── */
.toolbar{
  display:flex;flex-wrap:wrap;gap:8px;padding:10px 24px;
  background:var(--bg2);border-bottom:1px solid var(--border);
  align-items:center;position:sticky;top:38px;z-index:100;
}
.toolbar-input{
  background:var(--bg);border:1px solid var(--border-bright);color:var(--fg2);
  padding:6px 12px;font-family:var(--font);font-size:12px;
  outline:none;flex:1;min-width:200px;
}
.toolbar-input::placeholder{color:var(--muted)}
.toolbar-input:focus{border-color:var(--fg);box-shadow:0 0 0 1px var(--fg-dim)}
.toolbar-select{
  background:var(--bg);border:1px solid var(--border-bright);color:var(--fg2);
  padding:6px 10px;font-family:var(--font);font-size:12px;
  outline:none;cursor:pointer;
}
.toolbar-select option{background:var(--bg)}
.toolbar-select:focus{border-color:var(--fg)}
.toolbar-count{color:var(--fg);font-size:11px;letter-spacing:.05em;white-space:nowrap}

/* ── TABLE ────────────────────────────────────────────────────────────────── */
.table-wrap{overflow-x:auto}
table{width:100%;border-collapse:collapse}
thead th{
  background:var(--bg3);color:var(--muted2);
  padding:8px 14px;border-bottom:1px solid var(--fg-dim);
  text-align:left;font-size:10px;letter-spacing:.12em;text-transform:uppercase;
  white-space:nowrap;cursor:pointer;user-select:none;position:sticky;top:80px;z-index:50;
}
thead th:hover{color:var(--fg);background:rgba(57,255,20,.06)}
thead th.sort-asc::after{content:' [ASC]';color:var(--fg);font-size:9px}
thead th.sort-desc::after{content:' [DESC]';color:var(--fg);font-size:9px}
tbody tr.data-row{cursor:pointer;border-bottom:1px solid var(--border)}
tbody tr.data-row:nth-child(even){background:var(--row-even)}
tbody tr.data-row:nth-child(odd){background:var(--row-odd)}
tbody tr.data-row:hover td{background:var(--row-hover)}
tbody tr.data-row.expanded td{background:var(--row-hover);border-bottom:1px solid var(--fg-dim)}
tr.hidden{display:none!important}
td{padding:9px 14px;vertical-align:top;font-size:12px}
td.num{color:var(--muted);font-size:10px;width:44px;text-align:right;font-variant-numeric:tabular-nums}
td.src{white-space:nowrap;width:1px}
td.time{white-space:nowrap;color:var(--muted2);font-size:11px;width:1px}
.row-title a{color:var(--fg2);text-decoration:none;font-weight:700;word-break:break-all;font-size:12px}
.row-title a:hover{color:var(--fg);text-decoration:underline}
.row-preview{display:block;font-size:10px;color:var(--muted);margin-top:3px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:700px}
.row-expanded{
  max-height:0;overflow:hidden;font-size:11px;color:var(--muted2);
  white-space:pre-wrap;word-break:break-all;line-height:1.7;
  border-left:2px solid var(--fg-dim);padding:0 10px;margin-top:0;
  transition:max-height .35s ease,padding .35s ease,margin .35s ease;
}
tr.expanded .row-expanded{max-height:3000px;padding:10px;margin-top:8px}
tr.expanded .row-preview{display:none}

/* ── BADGES ───────────────────────────────────────────────────────────────── */
.badge{display:inline-block;padding:2px 6px;font-size:10px;font-weight:700;letter-spacing:.06em;text-transform:uppercase;border:1px solid;font-family:var(--font)}
.t1{color:var(--red);border-color:var(--red);background:rgba(255,34,34,.08)}
.t2{color:var(--orange);border-color:var(--orange);background:rgba(255,102,0,.08)}
.t3{color:#39ff14;border-color:#1a8c00;background:rgba(57,255,20,.06)}
.t4{color:var(--fg2);border-color:var(--fg2-dim);background:rgba(0,212,255,.06)}
.t-ransom{color:var(--purple);border-color:var(--purple);background:rgba(204,0,255,.08)}
.t-intel{color:var(--yellow);border-color:#806800;background:rgba(255,215,0,.06)}

/* ── SCROLL SENTINEL / LOADER ─────────────────────────────────────────────── */
#scroll-sentinel{height:1px;margin-top:-1px}
#load-more-status{
  text-align:center;padding:16px;font-size:11px;color:var(--muted);
  letter-spacing:.1em;
}
#load-more-status.loading::after{content:' [LOADING...]';animation:blink .6s step-end infinite}
#load-more-status.done{color:var(--muted)}

/* ── FOOTER ───────────────────────────────────────────────────────────────── */
footer{padding:16px 24px;border-top:1px solid var(--border);color:var(--muted);font-size:10px;letter-spacing:.08em;display:flex;justify-content:space-between;flex-wrap:wrap;gap:8px}
footer a{color:var(--muted2);text-decoration:none}
footer a:hover{color:var(--fg2)}
</style>
</head>
<body>

<!-- NAVBAR -->
<nav>
  <span class="nav-logo">[ DWT<span>_</span> ]</span>
  <a class="nav-tab" href="#stats">STATS</a>
  <a class="nav-tab" href="#sources">SOURCES</a>
  <a class="nav-tab" href="#feed">FEED</a>
  <span class="nav-sep"></span>
  <span class="nav-meta">{{ .UpdateTime }} UTC+8</span>
</nav>

<!-- HERO -->
<div class="hero">
  <div class="hero-pre">// DARKWEB INTEL TRACKER — LIVE FEED</div>
  <div class="hero-title">{{ .Title }}</div>
  <div class="hero-sub">last sync: <b>{{ .UpdateTime }}</b> &nbsp;|&nbsp; status: <b style="color:var(--fg)">ONLINE</b></div>
</div>

<!-- STATS BAR -->
<div class="stats-bar" id="stats">
  <div class="stat-box">
    <div class="stat-box-label">total records</div>
    <div class="stat-box-val">{{ .TotalCount }}</div>
    <div class="stat-box-sub">in database</div>
  </div>
  <div class="stat-box">
    <div class="stat-box-label">sources</div>
    <div class="stat-box-val" id="s-src-cnt">-</div>
    <div class="stat-box-sub">active feeds</div>
  </div>
  <div class="stat-box">
    <div class="stat-box-label">top source</div>
    <div class="stat-box-val" id="s-top-src" style="font-size:14px;padding-top:4px">-</div>
    <div class="stat-box-sub" id="s-top-cnt">-</div>
  </div>
  <div class="stat-box">
    <div class="stat-box-label">visible</div>
    <div class="stat-box-val" id="s-visible">{{ .TotalCount }}</div>
    <div class="stat-box-sub">after filter</div>
  </div>
</div>

<!-- SOURCE CHART -->
<div class="section" id="sources">
  <div class="section-head">source distribution</div>
  <div class="bar-grid" id="source-chart">
    {{ range $src, $cnt := .BySource }}
    <div class="bar-row" data-src="{{ $src }}" data-cnt="{{ $cnt }}">
      <div class="bar-label" title="{{ $src }}">{{ $src }}</div>
      <div class="bar-track"><div class="bar-fill"></div></div>
      <div class="bar-cnt">{{ $cnt }}</div>
    </div>
    {{ end }}
  </div>
</div>

<!-- TOOLBAR -->
<div class="toolbar" id="feed">
  <input class="toolbar-input" type="text" id="search" placeholder="> grep -i &quot;keyword&quot; feed.db" oninput="onFilter()">
  <select class="toolbar-select" id="src-filter" onchange="onFilter()">
    <option value="">-- all sources --</option>
    {{ range $src, $cnt := .BySource }}
    <option value="{{ $src }}">{{ $src }} ({{ $cnt }})</option>
    {{ end }}
  </select>
  <span class="toolbar-count" id="filter-count"></span>
</div>

<!-- TABLE -->
<div class="table-wrap">
<table id="main-table">
<thead>
  <tr>
    <th style="width:44px">#</th>
    <th onclick="sortTable(1)" id="th-1">TITLE / CONTENT</th>
    <th onclick="sortTable(2)" id="th-2">SOURCE</th>
    <th onclick="sortTable(3)" id="th-3">TIMESTAMP</th>
  </tr>
</thead>
<tbody id="feed-tbody">
{{ range $i, $it := .Items }}
<tr class="data-row" data-src="{{ $it.SiteName }}" data-title="{{ $it.Title }}" data-content="{{ $it.Content }}" data-time="{{ $it.CreatedAt.Format "2006-01-02 15:04:05" }}" onclick="toggleRow(this)">
  <td class="num">{{ inc $i }}</td>
  <td class="row-title">
    <a href="{{ $it.Link }}" target="_blank" rel="noopener noreferrer" onclick="event.stopPropagation()">{{ $it.Title }}</a>
    {{ if $it.Content }}
    <span class="row-preview">{{ truncate64 $it.Content }}</span>
    <div class="row-expanded" onclick="event.stopPropagation()">{{ $it.Content }}</div>
    {{ end }}
  </td>
  <td class="src"><span class="badge {{ siteTier $it.SiteName }}">{{ $it.SiteName }}</span></td>
  <td class="time">{{ $it.CreatedAt.Format "01-02 15:04" }}</td>
</tr>
{{ end }}
</tbody>
</table>
</div>

<!-- INFINITE SCROLL SENTINEL -->
<div id="scroll-sentinel"></div>
<div id="load-more-status"></div>

<footer>
  <span>DWT &copy; {{ .Date }} &nbsp;|&nbsp; {{ .TotalCount }} records indexed</span>
  <span>// DO NOT DISTRIBUTE WITHOUT AUTHORIZATION</span>
</footer>

<script>
// ── DATA ──────────────────────────────────────────────────────────────────
const BATCH = 50;           // rows to reveal per scroll-load
let allRows   = [];         // all tr.data-row in DOM order
let visibleRows = [];       // rows matching current filter
let renderedTo  = 0;        // how many of visibleRows are currently shown
let sortCol  = -1;
let sortAsc  = true;

// ── INIT ──────────────────────────────────────────────────────────────────
function init() {
  allRows = Array.from(document.querySelectorAll('#main-table tbody tr.data-row'));

  // hide all rows up front — infinite scroll will reveal them
  allRows.forEach(r => { r.style.display = 'none'; });
  visibleRows = allRows.slice();

  initChart();
  updateStats();
  revealBatch();         // show first BATCH immediately
  initScrollObserver();
}

// ── CHART ─────────────────────────────────────────────────────────────────
function initChart() {
  const bars = document.querySelectorAll('#source-chart .bar-row');
  let max = 0, topSrc = '-', topCnt = 0;
  bars.forEach(b => {
    const c = parseInt(b.dataset.cnt, 10);
    if (c > max) max = c;
    if (c > topCnt) { topCnt = c; topSrc = b.dataset.src; }
  });
  document.getElementById('s-src-cnt').textContent = bars.length;
  document.getElementById('s-top-src').textContent = topSrc;
  document.getElementById('s-top-cnt').textContent = topCnt + ' entries';
  setTimeout(() => {
    bars.forEach(b => {
      const pct = max > 0 ? parseInt(b.dataset.cnt,10) / max * 100 : 0;
      b.querySelector('.bar-fill').style.width = Math.max(pct, 1.5) + '%';
    });
  }, 80);
}

// ── FILTER ────────────────────────────────────────────────────────────────
function onFilter() {
  const q   = document.getElementById('search').value.toLowerCase().trim();
  const src = document.getElementById('src-filter').value;

  allRows.forEach(r => { r.style.display = 'none'; r.classList.remove('expanded'); });

  visibleRows = allRows.filter(r => {
    const titleOk = !q || r.dataset.title.toLowerCase().includes(q) || (r.dataset.content||'').toLowerCase().includes(q);
    const srcOk   = !src || r.dataset.src === src;
    return titleOk && srcOk;
  });

  // re-number after filter
  visibleRows.forEach((r, i) => {
    const nc = r.querySelector('td.num');
    if (nc) nc.textContent = i + 1;
  });

  if (sortCol !== -1) applySortOrder();

  renderedTo = 0;
  updateStats();
  revealBatch();
}

// ── SORT ──────────────────────────────────────────────────────────────────
function sortTable(col) {
  if (sortCol === col) { sortAsc = !sortAsc; } else { sortCol = col; sortAsc = true; }
  document.querySelectorAll('thead th').forEach((th, i) => {
    th.classList.remove('sort-asc','sort-desc');
    if (i === col) th.classList.add(sortAsc ? 'sort-asc' : 'sort-desc');
  });
  allRows.forEach(r => { r.style.display='none'; r.classList.remove('expanded'); });
  applySortOrder();
  renderedTo = 0;
  revealBatch();
}

function applySortOrder() {
  visibleRows.sort((a, b) => {
    const key = sortCol === 1 ? 'title' : sortCol === 2 ? 'src' : 'time';
    const va = (key === 'title' ? a.dataset.title : key === 'src' ? a.dataset.src : a.dataset.time).toLowerCase();
    const vb = (key === 'title' ? b.dataset.title : key === 'src' ? b.dataset.src : b.dataset.time).toLowerCase();
    return va < vb ? (sortAsc?-1:1) : va > vb ? (sortAsc?1:-1) : 0;
  });
  const tbody = document.getElementById('feed-tbody');
  visibleRows.forEach((r, i) => {
    tbody.appendChild(r);
    const nc = r.querySelector('td.num');
    if (nc) nc.textContent = i + 1;
  });
}

// ── INFINITE SCROLL ───────────────────────────────────────────────────────
function revealBatch() {
  const status = document.getElementById('load-more-status');
  const end = Math.min(renderedTo + BATCH, visibleRows.length);
  for (let i = renderedTo; i < end; i++) {
    visibleRows[i].style.display = '';
  }
  renderedTo = end;

  const remaining = visibleRows.length - renderedTo;
  if (remaining <= 0) {
    status.textContent = visibleRows.length > 0
      ? '// end of feed — ' + visibleRows.length + ' records'
      : '// no results match your query';
    status.className = 'done';
  } else {
    status.textContent = '// ' + renderedTo + ' / ' + visibleRows.length + ' loaded';
    status.className = '';
  }
}

function initScrollObserver() {
  const sentinel = document.getElementById('scroll-sentinel');
  const obs = new IntersectionObserver(entries => {
    if (entries[0].isIntersecting && renderedTo < visibleRows.length) {
      const status = document.getElementById('load-more-status');
      status.className = 'loading';
      // small delay for the terminal "loading" flicker effect
      setTimeout(revealBatch, 120);
    }
  }, { rootMargin: '200px' });
  obs.observe(sentinel);
}

// ── ROW TOGGLE ────────────────────────────────────────────────────────────
function toggleRow(tr) { tr.classList.toggle('expanded'); }

// ── STATS ─────────────────────────────────────────────────────────────────
function updateStats() {
  document.getElementById('s-visible').textContent = visibleRows.length;
  const fc = document.getElementById('filter-count');
  fc.textContent = visibleRows.length < allRows.length
    ? '[' + visibleRows.length + ' matched]' : '';
}

// ── SMOOTH ANCHOR ─────────────────────────────────────────────────────────
document.querySelectorAll('a[href^="#"]').forEach(a => {
  a.addEventListener('click', e => {
    const t = document.querySelector(a.getAttribute('href'));
    if (t) { e.preventDefault(); t.scrollIntoView({behavior:'smooth',block:'start'}); }
  });
});

document.addEventListener('DOMContentLoaded', init);
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
