package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"darkweb-tracker/internal/config"
	"darkweb-tracker/internal/storage"
)

//go:embed static
var staticFiles embed.FS

// ctxSession is a private context key type to avoid collisions.
type ctxSession struct{}

// Server is the built-in HTTP web interface.
type Server struct {
	cfg          config.WebConfig
	db           *storage.DB
	client       *http.Client
	log          *slog.Logger
	secret       string
	channelTitle string // fetched from Telegram getChat at startup
}

// New creates a Server. httpClient is reused for Telegram API calls.
func New(cfg config.WebConfig, db *storage.DB, httpClient *http.Client, log *slog.Logger) *Server {
	secret := cfg.SessionSecret
	if secret == "" {
		secret = RandomSecret()
		log.Warn("web: WEB_SESSION_SECRET not set — sessions will reset on restart")
	}
	return &Server{
		cfg:    cfg,
		db:     db,
		client: httpClient,
		log:    log,
		secret: secret,
	}
}

// Start registers routes and blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	// Pre-fetch channel display name so the login page shows a human-readable name.
	// Priority: manual override (WEB_CHANNEL_TITLE) > Telegram getChat API > raw channel_id.
	if s.cfg.ChannelTitle != "" {
		s.channelTitle = s.cfg.ChannelTitle
	} else if s.cfg.ChannelID != "" && s.cfg.BotToken != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		if title, err := fetchChatTitle(fetchCtx, s.client, s.cfg.BotToken, s.cfg.ChannelID); err == nil {
			s.channelTitle = title
			s.log.Info("web: channel title fetched", "title", title)
		} else {
			s.log.Warn("web: could not fetch channel title, will show raw ID", "err", err)
		}
	}
	mux := http.NewServeMux()

	// Static files from embedded FS.
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	// Auth routes.
	mux.HandleFunc("GET /auth/telegram", s.handleTelegramCallback)
	mux.HandleFunc("GET /logout", s.handleLogout)

	// Public config needed by the login page.
	mux.HandleFunc("GET /api/web-config", s.handleWebConfig)

	// API routes — require valid session + channel membership.
	mux.HandleFunc("GET /api/me", s.withAuth(s.apiMe))
	mux.HandleFunc("GET /api/items", s.withAuth(s.apiItems))
	mux.HandleFunc("GET /api/stats", s.withAuth(s.apiStats))

	// Catch-all → serve index.html (SPA).
	mux.HandleFunc("/", s.handleIndex)

	addr := fmt.Sprintf(":%d", s.cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      s.requestLogger(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Shutdown when context is cancelled.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	s.log.Info("web server listening", "addr", addr,
		"bot", s.cfg.BotUsername,
		"channel", s.cfg.ChannelID,
	)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("web server: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Route handlers
// ---------------------------------------------------------------------------

// handleIndex serves index.html for all non-API routes (SPA catch-all).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// handleWebConfig returns the public configuration needed by the login page.
// Does NOT expose the bot token or session secret.
func (s *Server) handleWebConfig(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]string{
		"bot_username":  s.cfg.BotUsername,
		"channel_id":    s.cfg.ChannelID,
		"channel_title": s.channelTitle, // human-readable group name
	})
}

// handleTelegramCallback processes the redirect from the Telegram Login Widget.
// Telegram sends: /auth/telegram?id=…&first_name=…&hash=…
func (s *Server) handleTelegramCallback(w http.ResponseWriter, r *http.Request) {
	user, err := VerifyTelegramAuth(r.URL.Query(), s.cfg.BotToken)
	if err != nil {
		s.log.Warn("telegram auth failed", "err", err, "ip", r.RemoteAddr)
		http.Redirect(w, r, "/?error=auth_failed", http.StatusFound)
		return
	}

	// Check channel membership.
	if s.cfg.ChannelID != "" {
		isMember, err := CheckChannelMembership(r.Context(), s.client, s.cfg.BotToken, s.cfg.ChannelID, user.ID)
		if err != nil {
			s.log.Warn("channel membership check failed", "err", err, "uid", user.ID)
			http.Redirect(w, r, "/?error=membership_check_failed", http.StatusFound)
			return
		}
		if !isMember {
			s.log.Info("access denied — not a channel member", "uid", user.ID, "username", user.Username)
			http.Redirect(w, r, "/?error=not_member", http.StatusFound)
			return
		}
	}

	// Issue session cookie.
	cookie := CreateSession(user, s.secret)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    cookie,
		Path:     "/",
		MaxAge:   int(sessionDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	s.log.Info("user authenticated", "uid", user.ID, "username", user.Username)
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout clears the session cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:   sessionCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// withAuth wraps a handler to require a valid session cookie.
// Unauthenticated requests receive a 401 JSON response.
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			jsonError(w, "not authenticated", http.StatusUnauthorized)
			return
		}
		sess, err := ValidateSession(cookie.Value, s.secret)
		if err != nil {
			jsonError(w, "session invalid or expired", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxSession{}, sess)
		next(w, r.WithContext(ctx))
	}
}

// requestLogger logs method, path, and duration for every request.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("http", "method", r.Method, "path", r.URL.Path,
			"dur", time.Since(start).Truncate(time.Millisecond))
	})
}

// ---------------------------------------------------------------------------
// Telegram helpers
// ---------------------------------------------------------------------------

// fetchChatTitle calls the Telegram Bot API getChat to retrieve the human-readable
// title of a channel or group. Returns the title on success, or an error.
func fetchChatTitle(ctx context.Context, client *http.Client, botToken, channelID string) (string, error) {
	apiURL := fmt.Sprintf(
		"https://api.telegram.org/bot%s/getChat?chat_id=%s",
		botToken,
		url.QueryEscape(channelID),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Title    string `json:"title"`
			Username string `json:"username"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("telegram api: %s", result.Description)
	}
	if result.Result.Title != "" {
		return result.Result.Title, nil
	}
	return result.Result.Username, nil
}
