// Package storage handles SQLite persistence for feed items.
// Uses modernc.org/sqlite (pure Go, no CGO required).
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Item represents a single feed entry stored in the database.
type Item struct {
	ID            int64
	Title         string
	Link          string
	PubDate       string
	Author        string
	Category      string
	Content       string // RSS teaser (always available)
	FullContent   string // full page body as Markdown (optional, requires FETCH_FULL_CONTENT)
	DownloadLinks string
	SiteName      string
	CreatedAt     time.Time
}

// DB wraps an SQLite connection with domain-specific methods.
type DB struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS items (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	title          TEXT    NOT NULL,
	link           TEXT    NOT NULL UNIQUE,   -- dedup key
	pub_date       TEXT,
	author         TEXT,
	category       TEXT,
	content        TEXT,
	full_content   TEXT    NOT NULL DEFAULT '', -- full page body as Markdown
	download_links TEXT,
	site_name      TEXT    NOT NULL,
	created_at     DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_items_site ON items(site_name);
CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at);
`

// Open opens (or creates) the SQLite database at path.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// SQLite is not safe for concurrent writes; a single writer is fine.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	// Idempotent migration: add full_content column to existing databases.
	// SQLite ignores the error when the column already exists.
	db.Exec(`ALTER TABLE items ADD COLUMN full_content TEXT NOT NULL DEFAULT ''`)

	return &DB{db: db}, nil
}

// Close closes the database connection.
func (d *DB) Close() error { return d.db.Close() }

// Exists reports whether an item with the given link is already stored.
func (d *DB) Exists(ctx context.Context, link string) (bool, error) {
	var count int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM items WHERE link = ?`, link,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("exists check: %w", err)
	}
	return count > 0, nil
}

// Insert stores a new item. Returns ErrDuplicate if the link already exists.
func (d *DB) Insert(ctx context.Context, item Item) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO items
			(title, link, pub_date, author, category, content, full_content, download_links, site_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.Title, item.Link, item.PubDate, item.Author,
		item.Category, item.Content, item.FullContent, item.DownloadLinks, item.SiteName,
	)
	if err != nil {
		return fmt.Errorf("insert item: %w", err)
	}
	return nil
}

// QueryOptions filters for item queries.
type QueryOptions struct {
	SiteName  string
	Since     time.Time
	Until     time.Time
	OrderDesc bool
	Limit     int
}

// List returns items matching the given options.
func (d *DB) List(ctx context.Context, opts QueryOptions) ([]Item, error) {
	q := `SELECT id, title, link, pub_date, author, category, content, full_content, download_links, site_name, created_at
	      FROM items WHERE 1=1`
	var args []any

	if opts.SiteName != "" {
		q += " AND site_name = ?"
		args = append(args, opts.SiteName)
	}
	// SQLite CURRENT_TIMESTAMP stores UTC; always compare in UTC to avoid
	// timezone-shifted results (e.g. CST "today" start ≠ UTC "today" start).
	if !opts.Since.IsZero() {
		q += " AND created_at >= ?"
		args = append(args, opts.Since.UTC().Format(time.DateTime))
	}
	if !opts.Until.IsZero() {
		q += " AND created_at <= ?"
		args = append(args, opts.Until.UTC().Format(time.DateTime))
	}

	if opts.OrderDesc {
		q += " ORDER BY created_at DESC"
	} else {
		q += " ORDER BY created_at ASC"
	}

	if opts.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, opts.Limit)
	}

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(
			&it.ID, &it.Title, &it.Link, &it.PubDate,
			&it.Author, &it.Category, &it.Content, &it.FullContent,
			&it.DownloadLinks, &it.SiteName, &it.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// CountBySource returns (siteName → count) for items in the time range.
func (d *DB) CountBySource(ctx context.Context, since, until time.Time) (map[string]int, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT site_name, COUNT(*) FROM items
		WHERE created_at >= ? AND created_at <= ?
		GROUP BY site_name ORDER BY COUNT(*) DESC`,
		since.UTC().Format(time.DateTime), until.UTC().Format(time.DateTime),
	)
	if err != nil {
		return nil, fmt.Errorf("count by source: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		result[name] = count
	}
	return result, rows.Err()
}

// TotalCount returns the number of items in the time range.
func (d *DB) TotalCount(ctx context.Context, since, until time.Time) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM items WHERE created_at >= ? AND created_at <= ?`,
		since.UTC().Format(time.DateTime), until.UTC().Format(time.DateTime),
	).Scan(&n)
	return n, err
}

// CountBySourceAll returns (siteName → count) for ALL items (no time filter).
func (d *DB) CountBySourceAll(ctx context.Context) (map[string]int, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT site_name, COUNT(*) FROM items
		GROUP BY site_name ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, fmt.Errorf("count by source all: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		result[name] = count
	}
	return result, rows.Err()
}

// Prune deletes items (and their analysis results via CASCADE) older than
// retentionDays. Returns the number of rows deleted.
// retentionDays <= 0 is a no-op.
func (d *DB) Prune(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.DateTime)
	res, err := d.db.ExecContext(ctx,
		`DELETE FROM items WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune items: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Reclaim space after large deletes.
		d.db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	}
	return n, nil
}
