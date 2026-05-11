// DarkWeb Forums Tracker — Go implementation
// Monitors dark web forum RSS feeds, scores them with AI, and pushes alerts.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	"golang.org/x/net/proxy"

	"darkweb-tracker/internal/analyzer"
	"darkweb-tracker/internal/config"
	"darkweb-tracker/internal/feed"
	"darkweb-tracker/internal/notify"
	"darkweb-tracker/internal/report"
	"darkweb-tracker/internal/scheduler"
	"darkweb-tracker/internal/sources"
	"darkweb-tracker/internal/storage"
)

const banner = `
╔══════════════════════════════════════════════════════╗
║         DarkWeb Forums Tracker  (Go + AI)            ║
║  监控 DarkWeb 论坛数据泄露 · AI 评分 · 多渠道推送    ║
╚══════════════════════════════════════════════════════╝`

// Version is set by ldflags at build time.
var Version = "dev"

func main() {
	// ── Flags ────────────────────────────────────────────────────────────────
	var (
		cfgFile    = flag.String("config", "config.yaml", "path to config file")
		dbFile     = flag.String("db", "data_leaks.db", "SQLite database path")
		archiveDir = flag.String("archive", "archive", "directory for reports")
		rssDir     = flag.String("rss", "rss", "directory for RSS XML files")
		once       = flag.Bool("once", false, "run one poll cycle then exit")
		debug      = flag.Bool("debug", false, "enable debug logging")
		ver        = flag.Bool("version", false, "print version and exit")

		// Export sub-command flags.
		export      = flag.Bool("export-training", false, "export training dataset as JSONL and exit")
		exportSince = flag.String("since", "", "export since date YYYY-MM-DD (default: 30 days ago)")
		exportUntil = flag.String("until", "", "export until date YYYY-MM-DD (default: today)")
		exportOut   = flag.String("out", "training_data.jsonl", "output file for training export")
	)
	flag.Parse()

	if *ver {
		fmt.Printf("DarkWeb Forums Tracker %s (Go)\n", Version)
		os.Exit(0)
	}

	// ── Logger ───────────────────────────────────────────────────────────────
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(tint.NewHandler(os.Stderr, &tint.Options{
		Level:      level,
		TimeFormat: time.DateTime,
	}))
	slog.SetDefault(log)

	fmt.Println(banner)
	log.Info("starting", "version", Version, "config", *cfgFile)

	// ── Config ───────────────────────────────────────────────────────────────
	cfg, err := config.Load(*cfgFile)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}

	// ── Storage ──────────────────────────────────────────────────────────────
	db, err := storage.Open(*dbFile)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Error("close database", "err", err)
		}
	}()

	// Run schema migrations (idempotent).
	ctx := context.Background()
	if err := db.MigrateAnalysis(ctx); err != nil {
		log.Error("db migration failed", "err", err)
		os.Exit(1)
	}
	log.Info("database ready", "path", *dbFile)

	// ── Export mode ──────────────────────────────────────────────────────────
	if *export {
		runExport(ctx, db, log, *exportSince, *exportUntil, *exportOut)
		return
	}

	// ── HTTP client ──────────────────────────────────────────────────────────
	httpClient := buildHTTPClient(cfg.Proxy)

	// ── Notifiers ────────────────────────────────────────────────────────────
	notifiers := buildNotifiers(cfg, httpClient, log)

	// ── AI Analyzer ──────────────────────────────────────────────────────────
	// Build provider chain: primary (if configured) → fallbacks → rules engine.
	// HybridAnalyzer tries each in order per item; rules engine never fails.
	az := buildAnalyzer(cfg, log)

	// ── Source loading (remote > local > builtin seeds) ──────────────────────
	srcLoader := sources.New(cfg.Sources, httpClient, log)
	activeSources, err := srcLoader.Load(ctx, cfg.DataSources)
	if err != nil {
		log.Error("source loading failed", "err", err)
		os.Exit(1)
	}
	log.Info("active sources ready", "count", len(activeSources))
	for _, ds := range activeSources {
		log.Debug("source", "name", ds.Name, "url", ds.RSSURL)
	}

	// ── Other components ─────────────────────────────────────────────────────
	gen := report.New(db, *archiveDir, *rssDir,
		"https://github.com/your-username/darkweb-tracker")
	fetcher := feed.New(cfg.Proxy, log)

	// ── Scheduler ────────────────────────────────────────────────────────────
	sched := scheduler.New(cfg, activeSources, fetcher, db, notifiers, gen, az, log)

	// ── Run ──────────────────────────────────────────────────────────────────
	runCtx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *once {
		log.Info("running single poll cycle (--once)")
		if err := sched.RunOnce(runCtx); err != nil {
			log.Error("run once failed", "err", err)
			os.Exit(1)
		}
		log.Info("done")
		return
	}

	log.Info("entering loop mode", "interval", cfg.Interval)
	if err := sched.Run(runCtx); err != nil && err != context.Canceled {
		log.Error("scheduler error", "err", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}

// ---------------------------------------------------------------------------
// Export training dataset
// ---------------------------------------------------------------------------

func runExport(ctx context.Context, db *storage.DB, log *slog.Logger,
	sinceStr, untilStr, outFile string) {

	now := time.Now()
	since := now.AddDate(0, 0, -30)
	until := now

	if sinceStr != "" {
		t, err := time.Parse(time.DateOnly, sinceStr)
		if err != nil {
			log.Error("invalid --since date", "value", sinceStr, "err", err)
			os.Exit(1)
		}
		since = t
	}
	if untilStr != "" {
		t, err := time.Parse(time.DateOnly, untilStr)
		if err != nil {
			log.Error("invalid --until date", "value", untilStr, "err", err)
			os.Exit(1)
		}
		until = t.Add(24*time.Hour - time.Second) // inclusive end of day
	}

	log.Info("exporting training data", "since", since.Format(time.DateOnly),
		"until", until.Format(time.DateOnly), "out", outFile)

	records, err := db.ExportForTraining(ctx, since, until)
	if err != nil {
		log.Error("export failed", "err", err)
		os.Exit(1)
	}

	f, err := os.Create(outFile)
	if err != nil {
		log.Error("create output file", "err", err)
		os.Exit(1)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			log.Error("encode record", "err", err)
			os.Exit(1)
		}
	}

	log.Info("export complete", "records", len(records), "file", outFile)
}

// ---------------------------------------------------------------------------
// Component builders
// ---------------------------------------------------------------------------

func buildHTTPClient(proxyCfg config.ProxyConfig) *http.Client {
	// Tor SOCKS5 takes highest priority
	if proxyCfg.TorSOCKS != "" {
		hostPort := proxyCfg.TorSOCKS
		if u, err := url.Parse(proxyCfg.TorSOCKS); err == nil && u.Host != "" {
			hostPort = u.Host
		}
		if dialer, err := proxy.SOCKS5("tcp", hostPort, nil, proxy.Direct); err == nil {
			transport := &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				},
				ResponseHeaderTimeout: 45 * time.Second,
				TLSHandshakeTimeout:   20 * time.Second,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
			}
			return &http.Client{Timeout: 60 * time.Second, Transport: transport}
		}
	}

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	if proxyCfg.Enabled && proxyCfg.HTTP != "" {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
}

func buildNotifiers(cfg *config.Config, client *http.Client, log *slog.Logger) *notify.Multi {
	var ns []notify.Notifier

	// DingTalk
	dt := config.DingTalk()
	if dt.Enabled && dt.Webhook != "" {
		ns = append(ns, notify.NewDingTalk(dt.Webhook, dt.SecretKey, client))
		log.Info("notifier enabled", "channel", "DingTalk")
	}

	// Feishu
	if cfg.Push.Feishu.Enabled && cfg.Push.Feishu.Webhook != "" {
		ns = append(ns, notify.NewFeishu(cfg.Push.Feishu.Webhook, client))
		log.Info("notifier enabled", "channel", "Feishu")
	}

	// Telegram
	tg := cfg.Push.Telegram
	if tg.Enabled && tg.Token != "" && tg.ChatID != "" {
		ns = append(ns, notify.NewTelegram(tg.Token, tg.ChatID, client))
		log.Info("notifier enabled", "channel", "Telegram")
	}

	// Discord
	dc := cfg.Push.Discord
	if dc.Enabled && dc.Webhook != "" {
		ns = append(ns, notify.NewDiscord(
			dc.Webhook,
			dc.SendNormalMsg,
			dc.SendDailyReport,
			dc.SendWeeklyReport,
			client,
		))
		log.Info("notifier enabled", "channel", "Discord")
	}

	if len(ns) == 0 {
		log.Warn("no notification channels configured — running in silent mode")
	}

	return notify.NewMulti(log, ns...)
}

// buildAnalyzer constructs the analysis pipeline:
//   primary LLM (if LLM_ENABLED + key set)
//   → fallback providers (LLM_FALLBACK_1_*, LLM_FALLBACK_2_*, …)
//   → RulesEngine (always last, zero cost, never fails)
//
// If no LLM is configured at all, only the RulesEngine runs.
// The HybridAnalyzer tries each provider per item; on any error it moves
// to the next provider automatically — no manual intervention needed.
func buildAnalyzer(cfg *config.Config, log *slog.Logger) analyzer.ItemAnalyzer {
	type entry = struct {
		Name     string
		Analyzer analyzer.ItemAnalyzer
	}

	var providers []entry

	// Primary provider
	if cfg.LLM.Enabled && cfg.LLM.APIKey != "" {
		providers = append(providers, entry{
			Name:     cfg.LLM.Provider,
			Analyzer: analyzer.New(cfg.LLM, log),
		})
		log.Info("analyzer: primary provider ready",
			"provider", cfg.LLM.Provider,
			"model", cfg.LLM.Model,
			"rpm", cfg.LLM.RPM,
		)
	} else if cfg.LLM.Enabled {
		log.Warn("analyzer: LLM_ENABLED=true but API key empty — skipping primary LLM")
	}

	// Fallback providers from config
	for _, fp := range cfg.LLM.FallbackProviders {
		if fp.APIKey == "" || fp.BaseURL == "" {
			continue
		}
		fbCfg := cfg.LLM // copy base settings (workers, thresholds, etc.)
		fbCfg.Provider = fp.Name
		fbCfg.BaseURL = fp.BaseURL
		fbCfg.APIKey = fp.APIKey
		fbCfg.Model = fp.Model
		if fp.RPM > 0 {
			fbCfg.RPM = fp.RPM
			fbCfg.RPMBurst = max(1, fp.RPM/6)
		}
		providers = append(providers, entry{
			Name:     fp.Name,
			Analyzer: analyzer.New(fbCfg, log),
		})
		log.Info("analyzer: fallback provider registered",
			"provider", fp.Name,
			"model", fp.Model,
			"rpm", fbCfg.RPM,
		)
	}

	// Always append rules engine as the final fallback
	providers = append(providers, entry{
		Name:     "rules-engine",
		Analyzer: analyzer.NewRulesEngine(log),
	})

	if len(providers) == 1 {
		// Only rules engine — no LLM configured at all
		cfg.LLM.Model = "rules-engine"
		log.Info("analyzer: rules-based scoring active (no LLM configured)")
		return providers[0].Analyzer
	}

	log.Info("analyzer: hybrid chain ready", "providers", len(providers))
	return analyzer.NewHybridAnalyzer(log, providers...)
}
