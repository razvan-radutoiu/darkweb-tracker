package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AnalysisRow holds a single AI analysis result for a feed item.
type AnalysisRow struct {
	ID               int64
	ItemID           int64
	Score            int
	Category         string
	Tags             []string // stored as JSON array
	Summary          string
	AffectedTargets  []string // stored as JSON array
	EstimatedRecords int
	DataTypes        []string // stored as JSON array
	IsUrgent         bool
	ConfidenceLevel  string
	Reasoning        string
	ModelUsed        string
	AnalyzedAt       time.Time
}

// ItemWithAnalysis pairs a feed item with its analysis result.
type ItemWithAnalysis struct {
	Item     Item
	Analysis AnalysisRow
}

// TrainingRecord is a flattened record suitable for ML training datasets.
// JSON tags use snake_case for Python compatibility.
type TrainingRecord struct {
	ItemID           int64    `json:"item_id"`
	Title            string   `json:"title"`
	Content          string   `json:"content"`
	SiteName         string   `json:"site_name"`
	Score            int      `json:"score"`
	Category         string   `json:"category"`
	Tags             []string `json:"tags"`
	Summary          string   `json:"summary"`
	AffectedTargets  []string `json:"affected_targets"`
	EstimatedRecords int      `json:"estimated_records"`
	DataTypes        []string `json:"data_types"`
	IsUrgent         bool     `json:"is_urgent"`
	ConfidenceLevel  string   `json:"confidence_level"`
	Reasoning        string   `json:"reasoning"`
	ModelUsed        string   `json:"model_used"`
	CreatedAt        string   `json:"created_at"`
	AnalyzedAt       string   `json:"analyzed_at"`
}

const analysisSchema = `
CREATE TABLE IF NOT EXISTS analysis_results (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id          INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    score            INTEGER NOT NULL CHECK(score BETWEEN 1 AND 10),
    category         TEXT    NOT NULL,
    tags             TEXT    NOT NULL DEFAULT '[]',
    summary          TEXT    NOT NULL DEFAULT '',
    affected_targets TEXT    NOT NULL DEFAULT '[]',
    estimated_records INTEGER NOT NULL DEFAULT -1,
    data_types       TEXT    NOT NULL DEFAULT '[]',
    is_urgent        INTEGER NOT NULL DEFAULT 0,
    confidence_level TEXT    NOT NULL DEFAULT 'low',
    reasoning        TEXT    NOT NULL DEFAULT '',
    model_used       TEXT    NOT NULL DEFAULT '',
    analyzed_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_analysis_item     ON analysis_results(item_id);
CREATE INDEX IF NOT EXISTS idx_analysis_score    ON analysis_results(score);
CREATE INDEX IF NOT EXISTS idx_analysis_urgent   ON analysis_results(is_urgent);
CREATE INDEX IF NOT EXISTS idx_analysis_category ON analysis_results(category);
`

// MigrateAnalysis creates the analysis_results table and adds denormalized
// columns to the items table. The operation is idempotent.
func (d *DB) MigrateAnalysis(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, analysisSchema); err != nil {
		return fmt.Errorf("migrate analysis schema: %w", err)
	}

	// Add denormalized columns to items for fast filtering.
	// SQLite lacks ADD COLUMN IF NOT EXISTS, so we ignore errors from
	// duplicate column additions.
	d.db.ExecContext(ctx, `ALTER TABLE items ADD COLUMN ai_score INTEGER DEFAULT 0`)
	d.db.ExecContext(ctx, `ALTER TABLE items ADD COLUMN ai_category TEXT DEFAULT ''`)
	d.db.ExecContext(ctx, `ALTER TABLE items ADD COLUMN ai_urgent INTEGER DEFAULT 0`)
	d.db.ExecContext(ctx, `ALTER TABLE items ADD COLUMN urgent_notified INTEGER DEFAULT 0`)

	return nil
}

// InsertAnalysis persists an analysis result and updates denormalized columns
// on the parent item. Uses INSERT OR REPLACE to handle re-analysis.
func (d *DB) InsertAnalysis(ctx context.Context, itemID int64, row AnalysisRow, modelUsed string) error {
	tagsJSON, err := json.Marshal(row.Tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	targetsJSON, err := json.Marshal(row.AffectedTargets)
	if err != nil {
		return fmt.Errorf("marshal affected_targets: %w", err)
	}
	dataTypesJSON, err := json.Marshal(row.DataTypes)
	if err != nil {
		return fmt.Errorf("marshal data_types: %w", err)
	}

	urgentInt := 0
	if row.IsUrgent {
		urgentInt = 1
	}

	_, err = d.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO analysis_results
			(item_id, score, category, tags, summary, affected_targets,
			 estimated_records, data_types, is_urgent, confidence_level,
			 reasoning, model_used, analyzed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		itemID, row.Score, row.Category, string(tagsJSON), row.Summary,
		string(targetsJSON), row.EstimatedRecords, string(dataTypesJSON),
		urgentInt, row.ConfidenceLevel, row.Reasoning, modelUsed,
	)
	if err != nil {
		return fmt.Errorf("insert analysis for item %d: %w", itemID, err)
	}

	_, err = d.db.ExecContext(ctx, `
		UPDATE items SET ai_score = ?, ai_category = ?, ai_urgent = ? WHERE id = ?`,
		row.Score, row.Category, urgentInt, itemID,
	)
	if err != nil {
		return fmt.Errorf("update item denorm columns for %d: %w", itemID, err)
	}
	return nil
}

// GetAnalysis retrieves the analysis result for the given item.
// Returns (nil, nil) when no analysis exists yet.
func (d *DB) GetAnalysis(ctx context.Context, itemID int64) (*AnalysisRow, error) {
	var (
		r          AnalysisRow
		tagsStr    string
		targetsStr string
		typesStr   string
		urgentInt  int
	)
	err := d.db.QueryRowContext(ctx, `
		SELECT id, item_id, score, category, tags, summary,
		       affected_targets, estimated_records, data_types,
		       is_urgent, confidence_level, reasoning, model_used, analyzed_at
		FROM analysis_results WHERE item_id = ?`, itemID,
	).Scan(
		&r.ID, &r.ItemID, &r.Score, &r.Category, &tagsStr, &r.Summary,
		&targetsStr, &r.EstimatedRecords, &typesStr,
		&urgentInt, &r.ConfidenceLevel, &r.Reasoning, &r.ModelUsed, &r.AnalyzedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get analysis for item %d: %w", itemID, err)
	}

	r.IsUrgent = urgentInt != 0

	if err := json.Unmarshal([]byte(tagsStr), &r.Tags); err != nil {
		return nil, fmt.Errorf("unmarshal tags: %w", err)
	}
	if err := json.Unmarshal([]byte(targetsStr), &r.AffectedTargets); err != nil {
		return nil, fmt.Errorf("unmarshal affected_targets: %w", err)
	}
	if err := json.Unmarshal([]byte(typesStr), &r.DataTypes); err != nil {
		return nil, fmt.Errorf("unmarshal data_types: %w", err)
	}

	return &r, nil
}

// TopScoredToday returns the highest-scored items analysed today (Beijing time),
// limited to the requested count. Only items with analysis results are included.
func (d *DB) TopScoredToday(ctx context.Context, limit int) ([]ItemWithAnalysis, error) {
	// SQLite date('now') is UTC; add 8 hours to align with Beijing calendar day.
	rows, err := d.db.QueryContext(ctx, `
		SELECT i.id, i.title, i.link, i.pub_date, i.author, i.category,
		       i.content, i.download_links, i.site_name, i.created_at,
		       a.id, a.item_id, a.score, a.category, a.tags, a.summary,
		       a.affected_targets, a.estimated_records, a.data_types,
		       a.is_urgent, a.confidence_level, a.reasoning, a.model_used,
		       a.analyzed_at
		FROM items i
		INNER JOIN analysis_results a ON a.item_id = i.id
		WHERE date(i.created_at, '+8 hours') = date('now', '+8 hours')
		ORDER BY a.score DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("top scored today: %w", err)
	}
	defer rows.Close()

	return scanItemsWithAnalysis(rows)
}

// UrgentUnnotified returns urgent items that have not yet been flagged as
// notified. Results are ordered newest-first.
func (d *DB) UrgentUnnotified(ctx context.Context) ([]ItemWithAnalysis, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT i.id, i.title, i.link, i.pub_date, i.author, i.category,
		       i.content, i.download_links, i.site_name, i.created_at,
		       a.id, a.item_id, a.score, a.category, a.tags, a.summary,
		       a.affected_targets, a.estimated_records, a.data_types,
		       a.is_urgent, a.confidence_level, a.reasoning, a.model_used,
		       a.analyzed_at
		FROM items i
		INNER JOIN analysis_results a ON a.item_id = i.id
		WHERE i.ai_urgent = 1 AND i.urgent_notified = 0
		ORDER BY a.analyzed_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("urgent unnotified: %w", err)
	}
	defer rows.Close()

	return scanItemsWithAnalysis(rows)
}

// MarkUrgentNotified flags the given items as having been notified so they
// are excluded from future UrgentUnnotified calls.
func (d *DB) MarkUrgentNotified(ctx context.Context, itemIDs []int64) error {
	if len(itemIDs) == 0 {
		return nil
	}

	placeholders := make([]string, len(itemIDs))
	args := make([]any, len(itemIDs))
	for i, id := range itemIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	q := fmt.Sprintf(
		`UPDATE items SET urgent_notified = 1 WHERE id IN (%s)`,
		strings.Join(placeholders, ","),
	)
	if _, err := d.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("mark urgent notified: %w", err)
	}
	return nil
}

// ExportForTraining returns flattened records joining items and analysis
// results for the given time window, ordered by score descending.
func (d *DB) ExportForTraining(ctx context.Context, since, until time.Time) ([]TrainingRecord, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT i.id, i.title, i.content, i.site_name, i.created_at,
		       a.score, a.category, a.tags, a.summary,
		       a.affected_targets, a.estimated_records, a.data_types,
		       a.is_urgent, a.confidence_level, a.reasoning, a.model_used,
		       a.analyzed_at
		FROM items i
		INNER JOIN analysis_results a ON a.item_id = i.id
		WHERE i.created_at BETWEEN ? AND ?
		ORDER BY a.score DESC`,
		since.UTC().Format(time.DateTime), until.UTC().Format(time.DateTime),
	)
	if err != nil {
		return nil, fmt.Errorf("export for training: %w", err)
	}
	defer rows.Close()

	var records []TrainingRecord
	for rows.Next() {
		var (
			tr         TrainingRecord
			tagsStr    string
			targetsStr string
			typesStr   string
			urgentInt  int
			createdAt  time.Time
			analyzedAt time.Time
		)
		if err := rows.Scan(
			&tr.ItemID, &tr.Title, &tr.Content, &tr.SiteName, &createdAt,
			&tr.Score, &tr.Category, &tagsStr, &tr.Summary,
			&targetsStr, &tr.EstimatedRecords, &typesStr,
			&urgentInt, &tr.ConfidenceLevel, &tr.Reasoning, &tr.ModelUsed,
			&analyzedAt,
		); err != nil {
			return nil, fmt.Errorf("scan training record: %w", err)
		}

		tr.IsUrgent = urgentInt != 0
		tr.CreatedAt = createdAt.Format(time.DateTime)
		tr.AnalyzedAt = analyzedAt.Format(time.DateTime)

		if err := json.Unmarshal([]byte(tagsStr), &tr.Tags); err != nil {
			return nil, fmt.Errorf("unmarshal tags: %w", err)
		}
		if err := json.Unmarshal([]byte(targetsStr), &tr.AffectedTargets); err != nil {
			return nil, fmt.Errorf("unmarshal affected_targets: %w", err)
		}
		if err := json.Unmarshal([]byte(typesStr), &tr.DataTypes); err != nil {
			return nil, fmt.Errorf("unmarshal data_types: %w", err)
		}

		records = append(records, tr)
	}
	return records, rows.Err()
}

// scanItemsWithAnalysis is a shared helper that scans rows produced by the
// 24-column items+analysis_results SELECT used by TopScoredToday and
// UrgentUnnotified.
func scanItemsWithAnalysis(rows *sql.Rows) ([]ItemWithAnalysis, error) {
	var result []ItemWithAnalysis
	for rows.Next() {
		var (
			it         Item
			ar         AnalysisRow
			tagsStr    string
			targetsStr string
			typesStr   string
			urgentInt  int
		)
		if err := rows.Scan(
			&it.ID, &it.Title, &it.Link, &it.PubDate, &it.Author, &it.Category,
			&it.Content, &it.DownloadLinks, &it.SiteName, &it.CreatedAt,
			&ar.ID, &ar.ItemID, &ar.Score, &ar.Category, &tagsStr, &ar.Summary,
			&targetsStr, &ar.EstimatedRecords, &typesStr,
			&urgentInt, &ar.ConfidenceLevel, &ar.Reasoning, &ar.ModelUsed,
			&ar.AnalyzedAt,
		); err != nil {
			return nil, fmt.Errorf("scan item with analysis: %w", err)
		}

		ar.IsUrgent = urgentInt != 0

		if err := json.Unmarshal([]byte(tagsStr), &ar.Tags); err != nil {
			return nil, fmt.Errorf("unmarshal tags: %w", err)
		}
		if err := json.Unmarshal([]byte(targetsStr), &ar.AffectedTargets); err != nil {
			return nil, fmt.Errorf("unmarshal affected_targets: %w", err)
		}
		if err := json.Unmarshal([]byte(typesStr), &ar.DataTypes); err != nil {
			return nil, fmt.Errorf("unmarshal data_types: %w", err)
		}

		result = append(result, ItemWithAnalysis{Item: it, Analysis: ar})
	}
	return result, rows.Err()
}
