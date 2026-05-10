// Package scheduler runs the periodic feed-check loop with graceful shutdown.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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
	sources   []config.DataSource // resolved at startup (remote + local + seeds)
	fetcher   *feed.Fetcher
	db        *storage.DB
	notifier  *notify.Multi
	generator *report.Generator
	analyzer  *analyzer.Analyzer // nil when LLM is disabled
	log       *slog.Logger
}

// New creates a Scheduler. Pass nil for az to disable AI analysis.
// sources is the resolved feed list from sources.Loader (may differ from cfg.DataSources).
func New(
	cfg *config.Config,
	sources []config.DataSource,
	fetcher *feed.Fetcher,
	db *storage.DB,
	notifier *notify.Multi,
	gen *report.Generator,
	az *analyzer.Analyzer,
	log *slog.Logger,
) *Scheduler {
	return &Scheduler{
		cfg:       cfg,
		sources:   sources,
		fetcher:   fetcher,
		db:        db,
		notifier:  notifier,
		generator: gen,
		analyzer:  az,
		log:       log,
	}
}

// RunOnce executes one full poll cycle:
//  1. Fetch all enabled RSS sources
//  2. Deduplicate via DB
//  3. AI analysis (if enabled) — urgent items push immediately
//  4. Generate daily / weekly reports
func (s *Scheduler) RunOnce(ctx context.Context) error {
	s.log.Info("starting poll cycle", "sources", len(s.sources))

	for _, ds := range s.sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.pollSource(ctx, ds)
	}

	// Flush any urgent items that haven't been notified yet.
	// (Covers items that became urgent in previous cycles too.)
	if s.analyzer != nil {
		s.pushPendingUrgent(ctx)
	}

	// Daily report.
	if s.cfg.DailyReport.Enabled {
		s.generateDaily(ctx)
	}

	// Weekly report — only on the configured weekday.
	if s.cfg.WeeklyReport.Enabled && isWeeklyDay(s.cfg.WeeklyReport.PushDay) {
		s.generateWeekly(ctx)
	}

	s.log.Info("poll cycle complete")
	return nil
}

// Run loops indefinitely, calling RunOnce every cfg.Interval.
// Returns when ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	s.notifier.Send(ctx, notify.Message{
		Title: "DarkWeb Forums Tracker 已启动",
		Body:  "开始监控 DarkWeb 论坛数据泄露信息…",
		Kind:  notify.KindStartup,
	})

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	// Run immediately on start.
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
// Core polling — fetch → dedup → insert → (optional) analyze → notify
// ---------------------------------------------------------------------------

func (s *Scheduler) pollSource(ctx context.Context, ds config.DataSource) {
	items, err := s.fetcher.Fetch(ctx, ds)
	if err != nil {
		s.log.Warn("fetch failed", "source", ds.Name, "err", err)
		return
	}

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

	s.log.Info("source polled",
		"source", ds.Name,
		"total", len(items),
		"new", len(newItems),
	)

	if len(newItems) == 0 {
		return
	}

	// AI analysis path.
	if s.analyzer != nil && s.cfg.LLM.Enabled {
		s.analyzeAndNotify(ctx, newItems)
		return
	}

	// No AI — send normal notifications for every new item.
	for _, item := range newItems {
		s.notifier.Send(ctx, notify.Message{
			Title:    item.SiteName + " 新增数据泄露",
			Body:     item.Title,
			Link:     item.Link,
			SiteName: item.SiteName,
			Kind:     notify.KindNormal,
		})
	}
}

// ---------------------------------------------------------------------------
// AI analysis → classify → urgent immediate push
// ---------------------------------------------------------------------------

func (s *Scheduler) analyzeAndNotify(ctx context.Context, items []storage.Item) {
	// Build batch inputs. Optionally skip already-analyzed items.
	var batch []analyzer.BatchItem
	itemByID := make(map[int64]storage.Item, len(items))

	for _, it := range items {
		if s.cfg.LLM.SkipAnalyzedItems {
			existing, err := s.db.GetAnalysis(ctx, it.ID)
			if err != nil {
				s.log.Warn("get analysis check failed", "id", it.ID, "err", err)
			}
			if existing != nil {
				continue // already analyzed
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

	s.log.Info("starting AI analysis batch", "items", len(batch))

	results := s.analyzer.AnalyzeBatch(ctx, batch, s.cfg.LLM.Workers)

	var urgentIDs []int64

	for _, br := range results {
		if br.Err != nil {
			s.log.Warn("analysis failed", "id", br.ID, "err", br.Err)
			continue
		}

		item := itemByID[br.ID]
		res := br.Result

		// Persist to DB.
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

		s.log.Info("item analyzed",
			"title", truncate(item.Title, 60),
			"score", res.Score,
			"category", res.Category,
			"urgent", res.IsUrgent,
		)

		// Immediate push for urgent items.
		if res.IsUrgent {
			s.sendUrgentAlert(ctx, item, res)
			urgentIDs = append(urgentIDs, br.ID)
		}
	}

	// Mark urgent items as notified so they don't fire again.
	if len(urgentIDs) > 0 {
		if err := s.db.MarkUrgentNotified(ctx, urgentIDs); err != nil {
			s.log.Error("mark urgent notified failed", "err", err)
		}
	}
}

// pushPendingUrgent sends immediate alerts for any urgent items found in DB
// that haven't been notified yet (e.g. items from a previous cycle or restart).
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

// sendUrgentAlert fires a KindUrgent notification to all channels.
func (s *Scheduler) sendUrgentAlert(ctx context.Context, item storage.Item, res *analyzer.AnalysisResult) {
	body := fmt.Sprintf(
		"⚠️ 评分: %d/10 | 类别: %s | 置信度: %s\n\n%s",
		res.Score, res.Category, res.ConfidenceLevel, res.Summary,
	)
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
// Report generation
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

	// Build daily summary body with TOP-N AI analysis.
	body := fmt.Sprintf("共收集到 %d 条数据泄露相关信息", result.TotalCount)
	if s.analyzer != nil && s.cfg.LLM.Enabled {
		body += s.buildTopNSummary(ctx)
	}

	s.notifier.Send(ctx, notify.Message{
		Title: "数据泄露监控日报 " + result.Date,
		Body:  body,
		Kind:  notify.KindDaily,
	})
}

// buildTopNSummary appends the TOP-N high-value items to the daily report body.
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
		sb.WriteString(fmt.Sprintf(
			"\n%d. [%d/10] %s\n   %s\n   %s",
			i+1, iwa.Analysis.Score, iwa.Item.Title,
			iwa.Analysis.Summary, iwa.Item.Link,
		))
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
// Helpers
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

// analysisRowToResult converts a storage.AnalysisRow back to analyzer.AnalysisResult
// for use in notification formatting.
func analysisRowToResult(row storage.AnalysisRow) *analyzer.AnalysisResult {
	return &analyzer.AnalysisResult{
		Score:            row.Score,
		Category:         row.Category,
		Tags:             row.Tags,
		Summary:          row.Summary,
		AffectedTargets:  row.AffectedTargets,
		EstimatedRecords: row.EstimatedRecords,
		DataTypes:        row.DataTypes,
		IsUrgent:         row.IsUrgent,
		ConfidenceLevel:  row.ConfidenceLevel,
		Reasoning:        row.Reasoning,
	}
}
