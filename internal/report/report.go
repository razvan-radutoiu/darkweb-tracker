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
:root{--bg:#0a0e17;--fg:#00ff41;--fg2:#00e0ff;--card:#121721;--border:#2d3748;--hover:rgba(0,255,65,.06)}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:'Courier New',monospace;background:var(--bg);color:var(--fg2);max-width:1400px;margin:0 auto;padding:24px;min-height:100vh}
/* Header */
header{border:1px solid var(--fg);padding:20px 28px;text-align:center;margin-bottom:24px;box-shadow:0 0 20px rgba(0,255,65,.2)}
h1{color:var(--fg);text-shadow:0 0 10px rgba(0,255,65,.4);font-size:1.8rem;margin-bottom:6px}
.subtitle{color:var(--fg2);font-size:.9rem;opacity:.8}
/* Stats */
.stats{display:flex;gap:12px;flex-wrap:wrap;margin-bottom:20px}
.stat-card{background:var(--card);border:1px solid var(--border);padding:14px 20px;flex:1;min-width:120px;border-radius:4px}
.stat-label{font-size:.75rem;color:#6b7280;margin-bottom:4px;text-transform:uppercase;letter-spacing:.05em}
.stat-value{font-size:1.4rem;color:var(--fg);font-weight:700}
/* Toolbar */
.toolbar{display:flex;gap:12px;margin-bottom:16px;flex-wrap:wrap;align-items:center}
.toolbar input,.toolbar select{background:var(--card);border:1px solid var(--border);color:var(--fg2);
  padding:8px 12px;font-family:inherit;font-size:.85rem;border-radius:4px;outline:none;flex:1;min-width:160px}
.toolbar input:focus,.toolbar select:focus{border-color:var(--fg)}
.toolbar select option{background:var(--bg)}
.count-badge{color:#6b7280;font-size:.8rem;white-space:nowrap}
/* Table */
.table-wrap{overflow-x:auto}
table{width:100%;border-collapse:collapse;margin-bottom:24px;font-size:.85rem}
th{background:var(--card);color:var(--fg);padding:10px 14px;border:1px solid var(--border);
   text-align:left;position:sticky;top:0;z-index:1;white-space:nowrap}
td{padding:9px 14px;border:1px solid var(--border);vertical-align:top;line-height:1.5}
td.num{color:#6b7280;text-align:right;white-space:nowrap;width:50px}
td.src{white-space:nowrap;font-size:.8rem}
td.time{white-space:nowrap;color:#6b7280;font-size:.8rem}
td a{color:var(--fg2);text-decoration:none;word-break:break-all}
td a:hover{color:var(--fg);text-decoration:underline}
tr:hover td{background:var(--hover)}
tr.hidden{display:none}
/* Site tier badges */
.badge{display:inline-block;padding:2px 7px;border-radius:3px;font-size:.72rem;font-weight:700;letter-spacing:.04em;white-space:nowrap}
.t1{background:#7c0000;color:#ff6b6b;border:1px solid #ff4444}   /* TIER1: BreachForums etc */
.t2{background:#4a3000;color:#ffa500;border:1px solid #ff8c00}   /* TIER2: high-value */
.t3{background:#1a3a1a;color:#66cc66;border:1px solid #44aa44}   /* TIER3: carding */
.t4{background:#1a2040;color:#7b9ef5;border:1px solid #4a6ed8}   /* TIER4: hacking */
.t-ransom{background:#3d0050;color:#da70d6;border:1px solid #9932cc} /* Ransomware */
.t-intel{background:#1a2a2a;color:#40e0d0;border:1px solid #20b2aa}  /* Intel/tracker */
/* Content preview tooltip */
td.title-cell{max-width:600px}
.preview{display:block;font-size:.75rem;color:#6b7280;margin-top:3px;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;max-width:580px}
/* Pagination */
.pagination{display:flex;gap:8px;justify-content:center;align-items:center;margin:16px 0;flex-wrap:wrap}
.pagination button{background:var(--card);border:1px solid var(--border);color:var(--fg2);
  padding:6px 14px;font-family:inherit;font-size:.85rem;cursor:pointer;border-radius:4px}
.pagination button:hover{border-color:var(--fg);color:var(--fg)}
.pagination button.active{background:var(--fg);color:var(--bg);border-color:var(--fg);font-weight:700}
.pagination button:disabled{opacity:.35;cursor:not-allowed}
.page-info{color:#6b7280;font-size:.8rem}
/* Footer */
footer{margin-top:40px;text-align:center;color:#6b7280;font-size:.8rem;border-top:1px solid var(--border);padding-top:16px}
</style>
</head>
<body>
<header>
  <h1>{{ .Title }}</h1>
  <p class="subtitle">最后更新：{{ .UpdateTime }}</p>
</header>

<div class="stats">
  <div class="stat-card"><div class="stat-label">总条数</div><div class="stat-value" id="total-count">{{ .TotalCount }}</div></div>
  {{ range $src, $cnt := .BySource }}
  <div class="stat-card"><div class="stat-label">{{ $src }}</div><div class="stat-value">{{ $cnt }}</div></div>
  {{ end }}
</div>

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
<thead><tr><th>#</th><th>标题 / 内容摘要</th><th>来源</th><th>发现时间</th></tr></thead>
<tbody>
{{ range $i, $it := .Items }}
<tr data-src="{{ $it.SiteName }}" data-title="{{ $it.Title }}" data-content="{{ $it.Content }}">
  <td class="num">{{ inc $i }}</td>
  <td class="title-cell">
    <a href="{{ $it.Link }}" target="_blank" rel="noopener noreferrer">{{ $it.Title }}</a>
    {{ if $it.Content }}<span class="preview">{{ truncate64 $it.Content }}</span>{{ end }}
  </td>
  <td class="src"><span class="badge {{ siteTier $it.SiteName }}">{{ $it.SiteName }}</span></td>
  <td class="time">{{ $it.CreatedAt.Format "01-02 15:04" }}</td>
</tr>
{{ end }}
</tbody>
</table>
</div>

<div class="pagination" id="pagination"></div>
<p class="page-info" id="page-info" style="text-align:center;color:#6b7280;font-size:.8rem;margin-top:8px"></p>

<footer><p>DarkWeb Forums Tracker · {{ .TotalCount }} 条记录 · {{ .Date }}</p></footer>

<script>
const PAGE_SIZE = 100;
let currentPage = 1;
let visibleRows = [];

function filterTable() {
  const q = document.getElementById('search').value.toLowerCase();
  const src = document.getElementById('src-filter').value;
  const rows = document.querySelectorAll('#main-table tbody tr');
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

// Init
document.addEventListener('DOMContentLoaded', () => {
  visibleRows = Array.from(document.querySelectorAll('#main-table tbody tr'));
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
