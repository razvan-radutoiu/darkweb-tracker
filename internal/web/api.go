package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"darkweb-tracker/internal/storage"
)

// apiItems handles GET /api/items?page=1&limit=50&site=&q=
func (s *Server) apiItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	// Simple offset pagination via "before" timestamp.
	opts := storage.QueryOptions{
		Limit:     limit + 1, // fetch one extra to detect "has more"
		OrderDesc: true,
	}

	if site := q.Get("site"); site != "" {
		opts.SiteName = site
	}

	// time-based cursor: ?before=unix_timestamp
	if v := q.Get("before"); v != "" {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			opts.Until = time.Unix(ts, 0)
		}
	}

	items, err := s.db.List(r.Context(), opts)
	if err != nil {
		jsonError(w, "db query failed", http.StatusInternalServerError)
		return
	}

	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}

	type apiItem struct {
		ID            int64     `json:"id"`
		Title         string    `json:"title"`
		Link          string    `json:"link"`
		PubDate       string    `json:"pub_date"`
		Author        string    `json:"author"`
		Category      string    `json:"category"`
		Content       string    `json:"content"`
		DownloadLinks string    `json:"download_links"`
		SiteName      string    `json:"site_name"`
		CreatedAt     time.Time `json:"created_at"`
		AIScore       int       `json:"ai_score"`
		AICategory    string    `json:"ai_category"`
		AIUrgent      bool      `json:"ai_urgent"`
		AISummary     string    `json:"ai_summary"`
	}

	out := make([]apiItem, 0, len(items))
	for _, it := range items {
		ai, _ := s.db.GetAnalysis(r.Context(), it.ID)
		ai_score := 0
		ai_cat := ""
		ai_urgent := false
		ai_summary := ""
		if ai != nil {
			ai_score = ai.Score
			ai_cat = ai.Category
			ai_urgent = ai.IsUrgent
			ai_summary = ai.Summary
		}
		out = append(out, apiItem{
			ID:            it.ID,
			Title:         it.Title,
			Link:          it.Link,
			PubDate:       it.PubDate,
			Author:        it.Author,
			Category:      it.Category,
			Content:       truncate(it.Content, 300),
			DownloadLinks: it.DownloadLinks,
			SiteName:      it.SiteName,
			CreatedAt:     it.CreatedAt,
			AIScore:       ai_score,
			AICategory:    ai_cat,
			AIUrgent:      ai_urgent,
			AISummary:     ai_summary,
		})
	}

	var cursor int64
	if hasMore && len(items) > 0 {
		cursor = items[len(items)-1].CreatedAt.Unix()
	}

	jsonOK(w, map[string]any{
		"items":    out,
		"has_more": hasMore,
		"cursor":   cursor,
	})
}

// apiStats handles GET /api/stats
func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Today's range in Beijing time (UTC+8).
	// Storage uses SQLite CURRENT_TIMESTAMP (UTC); TotalCount converts to UTC internally.
	now := time.Now().UTC().Add(8 * time.Hour)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	todayEnd := todayStart.Add(24 * time.Hour)

	todayCount, err := s.db.TotalCount(ctx, todayStart, todayEnd)
	if err != nil {
		jsonError(w, "db error", http.StatusInternalServerError)
		return
	}

	bySource, err := s.db.CountBySourceAll(ctx)
	if err != nil {
		jsonError(w, "db error", http.StatusInternalServerError)
		return
	}

	// Total items all time.
	var totalAll int
	for _, v := range bySource {
		totalAll += v
	}

	// Last 7 days per day.
	type dayCount struct {
		Date  string `json:"date"`
		Count int    `json:"count"`
	}
	var weekly []dayCount
	for i := 6; i >= 0; i-- {
		day := now.AddDate(0, 0, -i)
		ds := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
		de := ds.Add(24 * time.Hour)
		cnt, _ := s.db.TotalCount(ctx, ds, de)
		weekly = append(weekly, dayCount{Date: ds.Format("01/02"), Count: cnt})
	}

	jsonOK(w, map[string]any{
		"today_count": todayCount,
		"total_count": totalAll,
		"by_source":   bySource,
		"weekly":      weekly,
	})
}

// apiMe handles GET /api/me — returns current session user info.
func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := r.Context().Value(ctxSession{}).(sessionPayload)
	if !ok {
		jsonError(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	jsonOK(w, map[string]any{
		"id":         sess.UserID,
		"username":   sess.Username,
		"first_name": sess.FirstName,
		"photo_url":  sess.PhotoURL,
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
