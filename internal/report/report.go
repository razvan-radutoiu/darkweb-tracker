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
:root{--bg:#0a0e17;--fg:#00ff41;--fg2:#00e0ff;--card:#121721;--border:#2d3748}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:'Courier New',monospace;background:var(--bg);color:var(--fg2);max-width:1200px;margin:0 auto;padding:24px}
header{border:1px solid var(--fg);padding:24px;text-align:center;margin-bottom:32px;box-shadow:0 0 20px rgba(0,255,65,.3)}
h1{color:var(--fg);text-shadow:0 0 10px rgba(0,255,65,.5);font-size:2rem}
.stats{display:flex;gap:16px;flex-wrap:wrap;margin-bottom:32px}
.stat-card{background:var(--card);border:1px solid var(--border);padding:16px 24px;flex:1;min-width:140px}
.stat-label{font-size:.8rem;color:#6b7280;margin-bottom:4px}
.stat-value{font-size:1.5rem;color:var(--fg);font-weight:700}
table{width:100%;border-collapse:collapse;margin-bottom:32px}
th{background:var(--card);color:var(--fg);padding:12px;border:1px solid var(--border);text-align:left}
td{padding:10px 12px;border:1px solid var(--border);vertical-align:top}
td a{color:var(--fg2);text-decoration:none}
td a:hover{color:var(--fg);text-decoration:underline}
tr:hover td{background:rgba(0,255,65,.04)}
footer{margin-top:48px;text-align:center;color:#6b7280;font-size:.85rem}
</style>
</head>
<body>
<header>
  <h1>{{ .Title }}</h1>
  <p style="color:var(--fg2);margin-top:8px">最后更新：{{ .UpdateTime }}</p>
</header>

<div class="stats">
  <div class="stat-card"><div class="stat-label">总条数</div><div class="stat-value">{{ .TotalCount }}</div></div>
  {{ range $src, $cnt := .BySource }}
  <div class="stat-card"><div class="stat-label">{{ $src }}</div><div class="stat-value">{{ $cnt }}</div></div>
  {{ end }}
</div>

<table>
<thead><tr><th>#</th><th>标题</th><th>来源</th><th>发现时间</th></tr></thead>
<tbody>
{{ range $i, $it := .Items }}
<tr>
  <td>{{ inc $i }}</td>
  <td><a href="{{ $it.Link }}" target="_blank" rel="noopener">{{ $it.Title }}</a></td>
  <td>{{ $it.SiteName }}</td>
  <td>{{ $it.CreatedAt.Format "2006-01-02 15:04" }}</td>
</tr>
{{ end }}
</tbody>
</table>

<footer><p>Power By DarkWeb Forums Tracker</p></footer>
</body>
</html>`

var tmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"inc": func(i int) int { return i + 1 },
}).Parse(htmlTmpl))

func (g *Generator) renderHTML(path string, data reportData) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}
