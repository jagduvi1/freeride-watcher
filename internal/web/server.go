// Package web handles HTTP serving, routing, middleware, and HTML rendering.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jagduvi1/freeride-watcher/internal/config"
	"github.com/jagduvi1/freeride-watcher/internal/db"
	"github.com/jagduvi1/freeride-watcher/internal/fetcher"
	"github.com/jagduvi1/freeride-watcher/internal/notify"
)

//go:embed templates static
var embeddedFiles embed.FS

// contextKey is a private type for context values.
type contextKey string

const (
	ctxUser    contextKey = "user"
	ctxSession contextKey = "session"
)

// TemplateData is passed to every template render.
type TemplateData struct {
	Title          string
	User           *db.User
	Session        *db.Session
	CSRFToken      string
	VAPIDPublicKey string
	Flash          string
	FlashType      string
	Data           any
}

// Server is the main HTTP server.
type Server struct {
	cfg     *config.Config
	db      *db.DB
	pusher  *notify.Pusher
	emailer *notify.Emailer
	fetcher *fetcher.Fetcher

	templates map[string]*template.Template
	mux       *http.ServeMux

	// Per-IP rate limiter (login / register / forgot endpoints).
	rl   map[string]*rateBucket
	rlMu sync.Mutex
}

// NewServer wires up routes and parses templates.
func NewServer(cfg *config.Config, database *db.DB, pusher *notify.Pusher, emailer *notify.Emailer, f *fetcher.Fetcher) *Server {
	s := &Server{
		cfg:     cfg,
		db:      database,
		pusher:  pusher,
		emailer: emailer,
		fetcher: f,
		rl:      make(map[string]*rateBucket),
	}

	if err := s.loadTemplates(); err != nil {
		slog.Error("template load failed", "err", err)
		panic(err)
	}

	s.mux = s.buildMux()

	// Periodic housekeeping.
	go s.runHousekeeping()

	return s
}

// Start serves HTTP until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      s.mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("server shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

// ── Routing ───────────────────────────────────────────────────────────────────

func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Static assets (served from embedded FS).
	staticFS, _ := fs.Sub(embeddedFiles, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Service worker and manifest must be at root scope for PWA.
	mux.HandleFunc("GET /sw.js", s.serveEmbedded("static/sw.js", "application/javascript"))
	mux.HandleFunc("GET /manifest.json", s.serveEmbedded("static/manifest.json", "application/manifest+json"))

	// Public routes.
	mux.Handle("GET /", s.sessionMiddleware(http.HandlerFunc(s.handleHome)))
	mux.Handle("GET /login", s.sessionMiddleware(http.HandlerFunc(s.handleLoginForm)))
	mux.Handle("POST /login", s.sessionMiddleware(s.rateLimitMiddleware(http.HandlerFunc(s.handleLoginSubmit))))
	mux.Handle("GET /register", s.sessionMiddleware(http.HandlerFunc(s.handleRegisterForm)))
	mux.Handle("POST /register", s.sessionMiddleware(s.rateLimitMiddleware(http.HandlerFunc(s.handleRegisterSubmit))))
	mux.Handle("POST /logout", s.sessionMiddleware(http.HandlerFunc(s.handleLogout)))
	mux.Handle("GET /forgot", s.sessionMiddleware(http.HandlerFunc(s.handleForgotForm)))
	mux.Handle("POST /forgot", s.sessionMiddleware(s.rateLimitMiddleware(http.HandlerFunc(s.handleForgotSubmit))))
	mux.Handle("GET /reset/{token}", s.sessionMiddleware(http.HandlerFunc(s.handleResetForm)))
	mux.Handle("POST /reset/{token}", s.sessionMiddleware(http.HandlerFunc(s.handleResetSubmit)))

	// Authenticated routes.
	mux.Handle("GET /dashboard", s.sessionMiddleware(s.requireAuth(http.HandlerFunc(s.handleDashboard))))
	mux.Handle("GET /watches/new", s.sessionMiddleware(s.requireAuth(http.HandlerFunc(s.handleWatchNewForm))))
	mux.Handle("POST /watches/new", s.sessionMiddleware(s.requireAuth(s.csrfMiddleware(http.HandlerFunc(s.handleWatchNewSubmit)))))
	mux.Handle("POST /watches/{id}/delete", s.sessionMiddleware(s.requireAuth(s.csrfMiddleware(http.HandlerFunc(s.handleWatchDelete)))))

	// Push API (JSON, CSRF via header).
	mux.Handle("POST /api/push/subscribe", s.sessionMiddleware(s.requireAuth(s.csrfMiddleware(http.HandlerFunc(s.handlePushSubscribe)))))
	mux.Handle("DELETE /api/push/subscribe", s.sessionMiddleware(s.requireAuth(s.csrfMiddleware(http.HandlerFunc(s.handlePushUnsubscribe)))))
	mux.Handle("POST /api/push/test", s.sessionMiddleware(s.requireAuth(s.csrfMiddleware(http.HandlerFunc(s.handlePushTest)))))

	// Debug route list (authenticated).
	mux.Handle("GET /routes", s.sessionMiddleware(s.requireAuth(http.HandlerFunc(s.handleRoutes))))

	// Health check (no auth).
	mux.HandleFunc("GET /health", s.handleHealth)

	return mux
}

// ── Middleware ────────────────────────────────────────────────────────────────

func (s *Server) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		sess, err := s.db.GetSession(cookie.Value)
		if err != nil || sess == nil {
			// Clear stale cookie.
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "", MaxAge: -1, Path: "/"})
			next.ServeHTTP(w, r)
			return
		}

		user, err := s.db.GetUserByID(sess.UserID)
		if err != nil || user == nil {
			next.ServeHTTP(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), ctxUser, user)
		ctx = context.WithValue(ctx, ctxSession, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userFromCtx(r) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromCtx(r)
		if sess == nil {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			if err := r.ParseForm(); err == nil {
				token = r.FormValue("csrf_token")
			}
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRFToken)) != 1 {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Rate limiting ─────────────────────────────────────────────────────────────

type rateBucket struct {
	count    int
	resetAt  time.Time
}

const (
	rateLimit   = 10              // max requests
	rateWindow  = 5 * time.Minute // per window
)

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		s.rlMu.Lock()
		b, ok := s.rl[ip]
		if !ok || time.Now().After(b.resetAt) {
			b = &rateBucket{resetAt: time.Now().Add(rateWindow)}
			s.rl[ip] = b
		}
		b.count++
		over := b.count > rateLimit
		s.rlMu.Unlock()

		if over {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Template rendering ────────────────────────────────────────────────────────

func (s *Server) loadTemplates() error {
	funcs := template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("Mon 2 Jan 2006, 15:04")
		},
		"fmtDate": func(t time.Time) string {
			return t.Format("2006-01-02")
		},
		"weekdayLabel": func(n string) string {
			m := map[string]string{
				"1": "Mon", "2": "Tue", "3": "Wed",
				"4": "Thu", "5": "Fri", "6": "Sat", "7": "Sun",
			}
			var out []string
			for _, p := range strings.Split(n, ",") {
				if label, ok := m[strings.TrimSpace(p)]; ok {
					out = append(out, label)
				}
			}
			if len(out) == 0 {
				return "Any day"
			}
			return strings.Join(out, ", ")
		},
	}

	pages := []string{
		"home", "login", "register", "forgot", "reset",
		"dashboard", "watch_new", "routes",
	}
	s.templates = make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		t, err := template.New("base").
			Funcs(funcs).
			ParseFS(embeddedFiles,
				"templates/base.html",
				fmt.Sprintf("templates/%s.html", page),
			)
		if err != nil {
			return fmt.Errorf("parse template %s: %w", page, err)
		}
		s.templates[page] = t
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data TemplateData) {
	// Populate user / session / CSRF from context if not already set.
	if data.User == nil {
		data.User = userFromCtx(r)
	}
	if data.CSRFToken == "" {
		if sess := sessionFromCtx(r); sess != nil {
			data.CSRFToken = sess.CSRFToken
			// Propagate flash and clear it.
			if sess.FlashMessage != "" && data.Flash == "" {
				data.Flash = sess.FlashMessage
				data.FlashType = sess.FlashType
				_ = s.db.ClearFlash(sess.Token)
			}
		}
	}
	data.VAPIDPublicKey = s.cfg.VAPIDPublicKey

	tmpl, ok := s.templates[page]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		slog.Error("template render", "page", page, "err", err)
	}
}

// ── Context helpers ───────────────────────────────────────────────────────────

func userFromCtx(r *http.Request) *db.User {
	u, _ := r.Context().Value(ctxUser).(*db.User)
	return u
}

func sessionFromCtx(r *http.Request) *db.Session {
	s, _ := r.Context().Value(ctxSession).(*db.Session)
	return s
}

// ── Misc helpers ──────────────────────────────────────────────────────────────

func (s *Server) serveEmbedded(path, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := embeddedFiles.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		if contentType == "application/javascript" {
			// Service worker needs special header to extend scope.
			w.Header().Set("Service-Worker-Allowed", "/")
		}
		w.Write(data) //nolint:errcheck
	}
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, url, flash, flashType string) {
	if flash != "" {
		if sess := sessionFromCtx(r); sess != nil {
			_ = s.db.SetFlash(sess.Token, flash, flashType)
		}
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.SplitN(fwd, ",", 2)[0]
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (s *Server) runHousekeeping() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		_ = s.db.PruneExpiredSessions()

		// Clean up rate limiter buckets.
		s.rlMu.Lock()
		now := time.Now()
		for ip, b := range s.rl {
			if now.After(b.resetAt) {
				delete(s.rl, ip)
			}
		}
		s.rlMu.Unlock()
	}
}
