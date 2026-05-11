// Package scheduler runs the periodic feed-check loop with graceful shutdown.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"darkweb-tracker/internal/analyzer"
	"darkweb-tracker/internal/config"
	"darkweb-tracker/internal/feed"
	"darkweb-tracker/internal/notify"
	"darkweb-tracker/internal/report"
	"darkweb-tracker/internal/storage"
)

// Scheduler orchestrates polling, deduplication, AI analysis, notification, and reporting.
type Scheduler struct {
	cfg       *config.Config
	sources   []config.DataSource
	fetcher   *feed.Fetcher
	db        *storage.DB
	notifier  *notify.Multi
	generator *report.Generator
	analyzer  analyzer.ItemAnalyzer // nil when no analyzer configured
	log       *slog.Logger
}

func New(
	cfg *config.Config,
	sources []config.DataSource,
	fetcher *feed.Fetcher,
	db *storage.DB,
	notifier *notify.Multi,
	gen *report.Generator,
	az analyzer.ItemAnalyzer,
	log *slog.Logger,
) *Scheduler {
	return &Scheduler{
		cfg: cfg, sources: sources, fetcher: fetcher,
		db: db, notifier: notifier, generator: gen,
		analyzer: az, log: log,
	}
}

// RunOnce 执行一次完整采集周期：
//
//  1. 并行抓取所有 RSS 源
//  2. 全部去重入库
//  3. 对所有新条目一次性批量 AI 分析
//  4. 紧急条目立即推送
//  5. 生成日报 / 周报
func (s *Scheduler) RunOnce(ctx context.Context) error {
	start := time.Now()
	s.log.Info("poll cycle start", "sources", len(s.sources))

	// ── Phase 1: 并行抓取所有源 ──────────────────────────────────────────────
	allNew := s.fetchAllSources(ctx)
	s.log.Info("fetch complete",
		"new_items", len(allNew),
		"elapsed", time.Since(start).Round(time.Millisecond),
	)

	// ── Phase 2: 一次性批量 AI 分析 ──────────────────────────────────────────
	if len(allNew) > 0 {
		if s.analyzer != nil {
			s.batchAnalyzeAndNotify(ctx, allNew)
		} else {
			// 无 AI：直接推送每条新数据
			for _, item := range allNew {
				s.notifier.Send(ctx, notify.Message{
					Title:    item.SiteName + " 新增数据泄露",
					Body:     item.Title,
					Link:     item.Link,
					SiteName: item.SiteName,
					Kind:     notify.KindNormal,
				})
			}
		}
	}

	// ── Phase 3: 补推遗漏的紧急项 ────────────────────────────────────────────
	if s.analyzer != nil {
		s.pushPendingUrgent(ctx)
	}

	// ── Phase 4: 报告 ─────────────────────────────────────────────────────────
	if s.cfg.DailyReport.Enabled {
		s.generateDaily(ctx)
	}
	if s.cfg.WeeklyReport.Enabled && isWeeklyDay(s.cfg.WeeklyReport.PushDay) {
		s.generateWeekly(ctx)
	}

	s.log.Info("poll cycle complete",
		"new_items", len(allNew),
		"total_elapsed", time.Since(start).Round(time.Millisecond),
	)
	return nil
}

func (s *Scheduler) Run(ctx context.Context) error {
	s.notifier.Send(ctx, notify.Message{
		Title: "DarkWeb Forums Tracker 已启动",
		Body:  "开始监控 DarkWeb 论坛数据泄露信息…",
		Kind:  notify.KindStartup,
	})

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	if err := s.RunOnce(ctx); err != nil {
		s.log.Error("initial run failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopped")
			return ctx.Err()
		case <-ticker.C:
			if s.isSleepTime() {
				s.log.Info("night sleep active, skipping poll")
				continue
			}
			if err := s.RunOnce(ctx); err != nil {
				s.log.Error("poll cycle failed", "err", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Phase 1: 并行抓取所有源，返回全部新条目
// ---------------------------------------------------------------------------

// fetchAllSources 并发抓取所有启用的 RSS 源，聚合去重后的新条目。
// 各源之间完全并行，fetch 耗时取决于最慢的那个源，而不是所有源之和。
func (s *Scheduler) fetchAllSources(ctx context.Context) []storage.Item {
	type result struct {
		items []storage.Item
		err   error
		name  string
	}

	results := make(chan result, len(s.sources))
	var wg sync.WaitGroup

	for _, ds := range s.sources {
		wg.Add(1)
		go func(src config.DataSource) {
			defer wg.Done()
			items, err := s.fetcher.Fetch(ctx, src)
			results <- result{items: items, err: err, name: src.Name}
		}(ds)
	}

	// 等所有 goroutine 完成后关闭 channel
	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集结果，去重入库
	var allNew []storage.Item
	sourceStats := make(map[string][2]int) // name → [total, new]

	for r := range results {
		if r.err != nil {
			s.log.Warn("fetch failed", "source", r.name, "err", r.err)
			continue
		}

		newItems := s.insertNew(ctx, r.items)
		sourceStats[r.name] = [2]int{len(r.items), len(newItems)}
		allNew = append(allNew, newItems...)
	}

	// 打印每个源的统计
	for name, stats := range sourceStats {
		s.log.Info("source fetched",
			"source", name,
			"total", stats[0],
			"new", stats[1],
		)
	}

	return allNew
}

// insertNew 对一批条目做去重检查，把新条目写入 DB 并返回。
func (s *Scheduler) insertNew(ctx context.Context, items []storage.Item) []storage.Item {
	var newItems []storage.Item
	for _, item := range items {
		exists, err := s.db.Exists(ctx, item.Link)
		if err != nil {
			s.log.Error("db exists check failed", "err", err)
			continue
		}
		if exists {
			continue
		}
		if err := s.db.Insert(ctx, item); err != nil {
			s.log.Error("db insert failed", "err", err)
			continue
		}
		newItems = append(newItems, item)
	}
	return newItems
}

// ---------------------------------------------------------------------------
// Phase 2: 一次性批量 AI 分析
// ---------------------------------------------------------------------------

func (s *Scheduler) batchAnalyzeAndNotify(ctx context.Context, items []storage.Item) {
	// 过滤掉已分析过的条目
	var batch []analyzer.BatchItem
	itemByID := make(map[int64]storage.Item, len(items))

	for _, it := range items {
		if s.cfg.LLM.SkipAnalyzedItems {
			if existing, _ := s.db.GetAnalysis(ctx, it.ID); existing != nil {
				continue
			}
		}
		batch = append(batch, analyzer.BatchItem{
			ID:       it.ID,
			Title:    it.Title,
			Content:  it.Content,
			SiteName: it.SiteName,
		})
		itemByID[it.ID] = it
	}

	if len(batch) == 0 {
		return
	}

	s.log.Info("AI batch analysis start", "items", len(batch))
	start := time.Now()

	// 全部送进 worker pool，并发分析（受 rate limiter 控制速率）
	results := s.analyzer.AnalyzeBatch(ctx, batch, s.cfg.LLM.Workers)

	var urgentIDs []int64
	success, failed := 0, 0

	for _, br := range results {
		if br.Err != nil {
			s.log.Warn("analysis failed", "id", br.ID, "err", br.Err)
			failed++
			continue
		}

		item := itemByID[br.ID]
		res := br.Result

		// 持久化
		row := storage.AnalysisRow{
			ItemID:           br.ID,
			Score:            res.Score,
			Category:         res.Category,
			Tags:             res.Tags,
			Summary:          res.Summary,
			AffectedTargets:  res.AffectedTargets,
			EstimatedRecords: res.EstimatedRecords,
			DataTypes:        res.DataTypes,
			IsUrgent:         res.IsUrgent,
			ConfidenceLevel:  res.ConfidenceLevel,
			Reasoning:        res.Reasoning,
		}
		if err := s.db.InsertAnalysis(ctx, br.ID, row, s.cfg.LLM.Model); err != nil {
			s.log.Error("persist analysis failed", "id", br.ID, "err", err)
			continue
		}

		success++
		s.log.Debug("analyzed",
			"title", truncate(item.Title, 60),
			"score", res.Score,
			"category", res.Category,
			"urgent", res.IsUrgent,
		)

		// 紧急项收集，稍后统一推送
		if res.IsUrgent {
			urgentIDs = append(urgentIDs, br.ID)
			s.sendUrgentAlert(ctx, item, res)
		}
	}

	s.log.Info("AI batch analysis done",
		"success", success,
		"failed", failed,
		"urgent", len(urgentIDs),
		"elapsed", time.Since(start).Round(time.Millisecond),
	)

	if len(urgentIDs) > 0 {
		if err := s.db.MarkUrgentNotified(ctx, urgentIDs); err != nil {
			s.log.Error("mark urgent notified failed", "err", err)
		}
	}
}

// ---------------------------------------------------------------------------
// 紧急告警
// ---------------------------------------------------------------------------

func (s *Scheduler) pushPendingUrgent(ctx context.Context) {
	pending, err := s.db.UrgentUnnotified(ctx)
	if err != nil {
		s.log.Error("query urgent unnotified failed", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	s.log.Info("pushing pending urgent items", "count", len(pending))
	var ids []int64
	for _, iwa := range pending {
		s.sendUrgentAlert(ctx, iwa.Item, analysisRowToResult(iwa.Analysis))
		ids = append(ids, iwa.Item.ID)
	}
	if err := s.db.MarkUrgentNotified(ctx, ids); err != nil {
		s.log.Error("mark pending urgent notified failed", "err", err)
	}
}

func (s *Scheduler) sendUrgentAlert(ctx context.Context, item storage.Item, res *analyzer.AnalysisResult) {
	body := fmt.Sprintf("⚠️ 评分: %d/10 | 类别: %s | 置信度: %s\n\n%s",
		res.Score, res.Category, res.ConfidenceLevel, res.Summary)
	if len(res.AffectedTargets) > 0 {
		body += "\n🎯 影响目标: " + strings.Join(res.AffectedTargets, ", ")
	}
	if len(res.DataTypes) > 0 {
		body += "\n📦 数据类型: " + strings.Join(res.DataTypes, ", ")
	}
	if res.EstimatedRecords > 0 {
		body += fmt.Sprintf("\n📊 估计记录数: %d", res.EstimatedRecords)
	}
	s.notifier.Send(ctx, notify.Message{
		Title:    fmt.Sprintf("🚨 紧急告警 [%s] %s", item.SiteName, item.Title),
		Body:     body,
		Link:     item.Link,
		SiteName: item.SiteName,
		Kind:     notify.KindUrgent,
	})
}

// ---------------------------------------------------------------------------
// 报告生成
// ---------------------------------------------------------------------------

func (s *Scheduler) generateDaily(ctx context.Context) {
	result, err := s.generator.Daily(ctx)
	if err != nil {
		s.log.Error("daily report failed", "err", err)
		return
	}
	s.log.Info("daily report generated", "file", result.MarkdownFile, "count", result.TotalCount)

	if _, err := s.generator.GenerateRSS(ctx, "daily"); err != nil {
		s.log.Error("daily rss failed", "err", err)
	}

	// 每次运行后刷新全量 index.html（覆盖 workflow 的旧版跳转页）
	allResult, err := s.generator.AllTime(ctx)
	if err != nil {
		s.log.Error("all-time index.html failed", "err", err)
	} else {
		s.log.Info("all-time index.html updated", "file", allResult.HTMLFile, "total", allResult.TotalCount)
	}

	body := fmt.Sprintf("共收集到 %d 条数据泄露相关信息", result.TotalCount)
	if s.analyzer != nil {
		body += s.buildTopNSummary(ctx)
	}
	s.notifier.Send(ctx, notify.Message{
		Title: "数据泄露监控日报 " + result.Date,
		Body:  body,
		Kind:  notify.KindDaily,
	})
}

func (s *Scheduler) buildTopNSummary(ctx context.Context) string {
	n := s.cfg.LLM.DailyTopN
	if n <= 0 {
		n = 20
	}
	top, err := s.db.TopScoredToday(ctx, n)
	if err != nil || len(top) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n\n📊 今日 TOP %d 高价值情报：\n", len(top)))
	for i, iwa := range top {
		sb.WriteString(fmt.Sprintf("\n%d. [%d/10] %s\n   %s\n   %s",
			i+1, iwa.Analysis.Score, iwa.Item.Title,
			iwa.Analysis.Summary, iwa.Item.Link))
	}
	return sb.String()
}

func (s *Scheduler) generateWeekly(ctx context.Context) {
	result, err := s.generator.Weekly(ctx)
	if err != nil {
		s.log.Error("weekly report failed", "err", err)
		return
	}
	s.log.Info("weekly report generated", "file", result.MarkdownFile, "count", result.TotalCount)
	if _, err := s.generator.GenerateRSS(ctx, "weekly"); err != nil {
		s.log.Error("weekly rss failed", "err", err)
	}
	s.notifier.Send(ctx, notify.Message{
		Title: fmt.Sprintf("数据泄露监控周报 %s – %s", result.StartDate, result.EndDate),
		Body:  fmt.Sprintf("共收集到 %d 条数据泄露相关信息", result.TotalCount),
		Kind:  notify.KindWeekly,
	})
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

func (s *Scheduler) isSleepTime() bool {
	if !s.cfg.NightSleep.Enabled {
		return false
	}
	bjHour := (time.Now().UTC().Hour() + 8) % 24
	return bjHour >= s.cfg.NightSleep.StartHour && bjHour < s.cfg.NightSleep.EndHour
}

func isWeeklyDay(target time.Weekday) bool {
	return time.Now().UTC().Add(8 * time.Hour).Weekday() == target
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func analysisRowToResult(row storage.AnalysisRow) *analyzer.AnalysisResult {
	return &analyzer.AnalysisResult{
		Score: row.Score, Category: row.Category, Tags: row.Tags,
		Summary: row.Summary, AffectedTargets: row.AffectedTargets,
		EstimatedRecords: row.EstimatedRecords, DataTypes: row.DataTypes,
		IsUrgent: row.IsUrgent, ConfidenceLevel: row.ConfidenceLevel,
		Reasoning: row.Reasoning,
	}
}
