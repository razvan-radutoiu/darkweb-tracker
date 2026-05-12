// Package scheduler runs the periodic feed-check loop with graceful shutdown.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"darkweb-tracker/internal/analyzer"
	"darkweb-tracker/internal/config"
	"darkweb-tracker/internal/feed"
	"darkweb-tracker/internal/filter"
	"darkweb-tracker/internal/notify"
	"darkweb-tracker/internal/report"
	"darkweb-tracker/internal/storage"
)

// beijingTime returns now in UTC+8 (no tzdata dependency).
func beijingTime() time.Time {
	return time.Now().UTC().Add(8 * time.Hour)
}

// Scheduler orchestrates polling, deduplication, AI analysis, notification, and reporting.
type Scheduler struct {
	cfg       *config.Config
	sources   []config.DataSource
	fetcher   *feed.Fetcher
	db        *storage.DB
	notifier  *notify.Multi
	generator *report.Generator
	analyzer  analyzer.ItemAnalyzer
	filter    *filter.Filter
	log       *slog.Logger

	// Dedup guards: prevent daily/weekly reports from firing multiple times
	// when polling interval is short (e.g. 1 minute).
	lastDailyDate  string // "2006-01-02" in Beijing time
	lastWeeklyDate string // "2006-Www" ISO week in Beijing time
}

func New(
	cfg *config.Config,
	sources []config.DataSource,
	fetcher *feed.Fetcher,
	db *storage.DB,
	notifier *notify.Multi,
	gen *report.Generator,
	az analyzer.ItemAnalyzer,
	f *filter.Filter,
	log *slog.Logger,
) *Scheduler {
	return &Scheduler{
		cfg: cfg, sources: sources, fetcher: fetcher,
		db: db, notifier: notifier, generator: gen,
		analyzer: az, filter: f, log: log,
	}
}

// ---------------------------------------------------------------------------
// RunOnce — channel-pipeline architecture
//
//   Fetch goroutines (one per source) push new items into itemsCh as soon as
//   each source completes — without waiting for slow sources.
//   Analysis workers drain itemsCh concurrently, analyze each item immediately
//   through the provider chain (LLM → fallback → rules engine), persist the
//   result, and push to notification channels if score >= notify_min_score.
//
//   Flow:
//     [Source A] ──┐
//     [Source B] ──┼──► itemsCh ──► [worker 0] ──► analyze ──► push
//     [Source C] ──┘           └──► [worker 1] ──► analyze ──► push
//
// ---------------------------------------------------------------------------

func (s *Scheduler) RunOnce(ctx context.Context) error {
	start := time.Now()
	s.log.Info("poll cycle start", "sources", len(s.sources))

	// itemsCh carries newly-inserted items from fetch goroutines to analysis workers.
	// Buffer size = total sources × typical max new items per source to avoid blocking.
	itemsCh := make(chan storage.Item, 512)

	var totalNew atomic.Int64

	// ── Start analysis workers ──────────────────────────────────────────────
	workers := s.cfg.LLM.Workers
	if workers <= 0 {
		workers = 3
	}

	var workerWg sync.WaitGroup
	if s.analyzer != nil {
		for w := 0; w < workers; w++ {
			workerWg.Add(1)
			go s.analyzeWorker(ctx, &workerWg, itemsCh, &totalNew)
		}
	}

	// ── Fetch all sources in parallel ───────────────────────────────────────
	// Each goroutine pushes new items immediately upon completion — the fastest
	// sources feed the workers right away without waiting for slow ones.
	var fetchWg sync.WaitGroup
	for _, src := range s.sources {
		fetchWg.Add(1)
		go func(src config.DataSource) {
			defer fetchWg.Done()

			items, err := s.fetcher.Fetch(ctx, src)
			if err != nil {
				s.log.Warn("fetch failed", "source", src.Name, "err", err)
				return
			}

			newItems := s.insertNew(ctx, items)
			if len(newItems) == 0 {
				return
			}

			// Filter out posts older than MaxItemAgeDays.
			// Old posts are already in DB (for future dedup) but skip analysis/push.
			freshItems, staleCount := s.freshItems(newItems)
			if staleCount > 0 {
				s.log.Info("source: skipped old posts",
					"source", src.Name,
					"stale", staleCount,
					"fresh", len(freshItems),
					"max_age_days", s.cfg.MaxItemAgeDays,
				)
			}
			if len(freshItems) == 0 {
				return
			}
			s.log.Info("source: new items queued",
				"source", src.Name,
				"fresh", len(freshItems),
				"total_fetched", len(items),
			)

			if s.analyzer != nil {
				// Hand off to analysis workers immediately.
				for _, item := range freshItems {
					select {
					case itemsCh <- item:
					case <-ctx.Done():
						return
					}
				}
			} else {
				// No AI configured: push each item directly as plain notification.
				for _, item := range freshItems {
					totalNew.Add(1)
					s.notifier.Send(ctx, notify.Message{
						Title:    item.SiteName + " 新增数据泄露",
						Body:     item.Title,
						Link:     item.Link,
						SiteName: item.SiteName,
						Kind:     notify.KindNormal,
					})
				}
			}
		}(src)
	}

	// Wait for all fetches to finish, then close the channel so workers
	// know there is no more work coming this cycle.
	fetchWg.Wait()
	close(itemsCh)

	// Wait for all analysis workers to finish processing the remaining items.
	workerWg.Wait()

	s.log.Info("poll cycle complete",
		"new_items", totalNew.Load(),
		"elapsed", time.Since(start).Round(time.Millisecond),
	)

	// ── Safety net: push any urgent items that somehow weren't notified ─────
	if s.analyzer != nil {
		s.pushPendingUrgent(ctx)
	}

	// ── Reports: generate at most once per calendar day / week ──────────────
	// Using Beijing time so reports align with the user's timezone.
	now := beijingTime()
	today := now.Format("2006-01-02")
	_, isoWeek := now.ISOWeek()
	thisWeek := fmt.Sprintf("%d-W%02d", now.Year(), isoWeek)

	if s.cfg.DailyReport.Enabled && s.lastDailyDate != today {
		s.generateDaily(ctx)
		s.lastDailyDate = today
	}

	if s.cfg.WeeklyReport.Enabled &&
		isWeeklyDay(s.cfg.WeeklyReport.PushDay) &&
		s.lastWeeklyDate != thisWeek {
		s.generateWeekly(ctx)
		s.lastWeeklyDate = thisWeek
	}

	// ── Data TTL: prune old records ──────────────────────────────────────────
	if s.cfg.RetentionDays > 0 {
		if n, err := s.db.Prune(ctx, s.cfg.RetentionDays); err != nil {
			s.log.Error("prune failed", "err", err)
		} else if n > 0 {
			s.log.Info("pruned old records", "deleted", n, "retention_days", s.cfg.RetentionDays)
		}
	}

	return nil
}

// Run loops indefinitely, calling RunOnce at every cfg.Interval tick.
func (s *Scheduler) Run(ctx context.Context) error {
	// Build a startup summary so the user can verify config at a glance.
	analyzerInfo := "规则引擎（无 LLM）"
	if s.cfg.LLM.Enabled && s.cfg.LLM.APIKey != "" {
		analyzerInfo = fmt.Sprintf("LLM ✅  provider=%s  model=%s",
			s.cfg.LLM.Provider, s.cfg.LLM.Model)
		if len(s.cfg.LLM.FallbackProviders) > 0 {
			analyzerInfo += fmt.Sprintf("  fallbacks=%d", len(s.cfg.LLM.FallbackProviders))
		}
	} else if s.cfg.LLM.Enabled {
		analyzerInfo = "LLM ⚠️  已启用但 API Key 为空，降级到规则引擎"
	}

	s.notifier.Send(ctx, notify.Message{
		Title: "DarkWeb Forums Tracker 已启动",
		Body: fmt.Sprintf(
			"轮询间隔: %s\n分析引擎: %s\n推送阈值: score≥%d\n帖子时效: %d 天内",
			s.cfg.Interval,
			analyzerInfo,
			s.cfg.LLM.NotifyMinScore,
			s.cfg.MaxItemAgeDays,
		),
		Kind: notify.KindStartup,
	})

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	// Run immediately on startup, then on every tick.
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
// Analysis worker
// ---------------------------------------------------------------------------

// analyzeWorker drains itemsCh, analyzes each item through the provider chain,
// persists the result, and pushes a notification if score >= notify_min_score.
// One goroutine per worker; all workers share the analyzer's rate limiter.
func (s *Scheduler) analyzeWorker(
	ctx context.Context,
	wg *sync.WaitGroup,
	items <-chan storage.Item,
	total *atomic.Int64,
) {
	defer wg.Done()

	minScore := s.cfg.LLM.NotifyMinScore
	if minScore <= 0 {
		minScore = 5
	}

	for item := range items {
		total.Add(1)

		// Fast title pre-filter — free, runs in microseconds.
		fr := s.filter.QuickFilter(item.Title, item.SiteName)
		if fr.Action == filter.ActionSkip {
			s.log.Debug("title filter: skipped",
				"title", truncate(item.Title, 80),
				"reason", fr.Reason,
			)
			continue
		}

		// Analyze single item — goes through the full provider chain:
		// primary LLM → fallback providers → rules engine (never fails).
		results := s.analyzer.AnalyzeBatch(ctx, []analyzer.BatchItem{
			{
				ID:            item.ID,
				Title:         item.Title,
				Content:       item.Content,
				FullContent:   item.FullContent,
				SiteName:      item.SiteName,
				PubDate:       item.PubDate,
				Author:        item.Author,
				DownloadLinks: item.DownloadLinks,
			},
		}, 1)

		if len(results) == 0 {
			s.log.Warn("analysis returned empty result", "id", item.ID)
			continue
		}
		br := results[0]
		if br.Err != nil {
			s.log.Warn("analysis failed", "id", item.ID, "title", truncate(item.Title, 60), "err", br.Err)
			continue
		}
		res := br.Result

		// Persist to DB.
		row := storage.AnalysisRow{
			ItemID:           item.ID,
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
		if err := s.db.InsertAnalysis(ctx, item.ID, row, s.cfg.LLM.Model); err != nil {
			s.log.Error("persist analysis failed", "id", item.ID, "err", err)
			continue
		}

		s.log.Debug("analyzed",
			"title", truncate(item.Title, 60),
			"site", item.SiteName,
			"score", res.Score,
			"category", res.Category,
			"urgent", res.IsUrgent,
		)

		// Push immediately if above the configured threshold.
		if res.Score < minScore {
			continue
		}

		kind := notify.KindNormal
		if res.IsUrgent {
			kind = notify.KindUrgent
			if err := s.db.MarkUrgentNotified(ctx, []int64{item.ID}); err != nil {
				s.log.Error("mark urgent notified failed", "err", err)
			}
		}

		s.notifier.Send(ctx, notify.Message{
			Title:            item.Title,
			Body:             res.Summary,
			Link:             item.Link,
			SiteName:         item.SiteName,
			Kind:             kind,
			Score:            res.Score,
			Category:         res.Category,
			Summary:          res.Summary,
			AffectedTargets:  res.AffectedTargets,
			DataTypes:        res.DataTypes,
			EstimatedRecords: res.EstimatedRecords,
			IsUrgent:         res.IsUrgent,
			ConfidenceLevel:  res.ConfidenceLevel,
		})
	}
}

// ---------------------------------------------------------------------------
// Feed fetching & deduplication
// ---------------------------------------------------------------------------

// insertNew checks each item against the DB and inserts new ones.
// Returns only the items that were actually inserted.
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

// freshItems filters newItems to only those whose pub_date is within
// cfg.MaxItemAgeDays. Items without a parseable pub_date are kept (assume fresh).
// Returns (fresh, staleCount).
func (s *Scheduler) freshItems(items []storage.Item) (fresh []storage.Item, stale int) {
	maxAge := s.cfg.MaxItemAgeDays
	if maxAge <= 0 {
		// Filter disabled — treat everything as fresh.
		return items, 0
	}
	cutoff := time.Now().Add(-time.Duration(maxAge) * 24 * time.Hour)

	for _, item := range items {
		if item.PubDate == "" {
			// No date info from feed → assume fresh, include it.
			fresh = append(fresh, item)
			continue
		}
		t, err := time.Parse(time.RFC3339, item.PubDate)
		if err != nil {
			// Unparseable date → assume fresh.
			fresh = append(fresh, item)
			continue
		}
		if t.After(cutoff) {
			fresh = append(fresh, item)
		} else {
			stale++
		}
	}
	return fresh, stale
}

// ---------------------------------------------------------------------------
// Safety net: push urgent items that were analyzed but never notified
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
		res := analysisRowToResult(iwa.Analysis)
		s.notifier.Send(ctx, notify.Message{
			Title:            fmt.Sprintf("[%s] %s", iwa.Item.SiteName, iwa.Item.Title),
			Body:             res.Summary,
			Link:             iwa.Item.Link,
			SiteName:         iwa.Item.SiteName,
			Kind:             notify.KindUrgent,
			Score:            res.Score,
			Category:         res.Category,
			Summary:          res.Summary,
			AffectedTargets:  res.AffectedTargets,
			DataTypes:        res.DataTypes,
			EstimatedRecords: res.EstimatedRecords,
			IsUrgent:         true,
			ConfidenceLevel:  res.ConfidenceLevel,
		})
		ids = append(ids, iwa.Item.ID)
	}
	if err := s.db.MarkUrgentNotified(ctx, ids); err != nil {
		s.log.Error("mark pending urgent notified failed", "err", err)
	}
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
// Helpers
// ---------------------------------------------------------------------------

func (s *Scheduler) isSleepTime() bool {
	if !s.cfg.NightSleep.Enabled {
		return false
	}
	bjHour := beijingTime().Hour()
	return bjHour >= s.cfg.NightSleep.StartHour && bjHour < s.cfg.NightSleep.EndHour
}

func isWeeklyDay(target time.Weekday) bool {
	return beijingTime().Weekday() == target
}

// truncate cuts s to at most n runes (not bytes), appending "…" if truncated.
// Using rune slicing prevents broken multi-byte characters (e.g. CJK, Cyrillic).
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
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
