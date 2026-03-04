package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jagduvi1/freeride-watcher/internal/auth"
	"github.com/jagduvi1/freeride-watcher/internal/db"
	"github.com/jagduvi1/freeride-watcher/internal/matcher"
	"github.com/jagduvi1/freeride-watcher/internal/notify"
)

// ── Health ────────────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
}

// ── Home ──────────────────────────────────────────────────────────────────────

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	type homeData struct {
		Status     interface{}
		RouteCount int
	}

	rc, _ := s.db.CountRoutes()
	s.render(w, r, "home", TemplateData{
		Title: "Freerider Watcher",
		Data: homeData{
			Status:     s.fetcher.GetStatus(),
			RouteCount: rc,
		},
	})
}

// ── Auth: Register ────────────────────────────────────────────────────────────

func (s *Server) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	if userFromCtx(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, r, "register", TemplateData{Title: "Register"})
}

func (s *Server) handleRegisterSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	renderErr := func(msg string) {
		s.render(w, r, "register", TemplateData{Title: "Register", Flash: msg, FlashType: "error"})
	}

	if email == "" || password == "" {
		renderErr("Email and password are required.")
		return
	}
	if len(password) < 8 {
		renderErr("Password must be at least 8 characters.")
		return
	}
	if password != confirm {
		renderErr("Passwords do not match.")
		return
	}

	// Check if email already in use.
	existing, err := s.db.GetUserByEmail(email)
	if err != nil {
		slog.Error("register lookup", "err", err)
		renderErr("An error occurred. Please try again.")
		return
	}
	if existing != nil {
		renderErr("An account with that email already exists.")
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		slog.Error("hash password", "err", err)
		renderErr("An error occurred. Please try again.")
		return
	}

	userID, err := s.db.CreateUser(email, hash)
	if err != nil {
		slog.Error("create user", "err", err)
		renderErr("An error occurred. Please try again.")
		return
	}

	if err := s.startSession(w, r, userID); err != nil {
		slog.Error("start session", "err", err)
	}
	s.redirect(w, r, "/dashboard", "Welcome! Your account has been created.", "success")
}

// ── Auth: Login ───────────────────────────────────────────────────────────────

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if userFromCtx(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", TemplateData{Title: "Log in"})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")

	renderErr := func() {
		s.render(w, r, "login", TemplateData{
			Title:     "Log in",
			Flash:     "Invalid email or password.",
			FlashType: "error",
		})
	}

	user, err := s.db.GetUserByEmail(email)
	if err != nil {
		slog.Error("login lookup", "err", err)
		renderErr()
		return
	}
	if user == nil || !auth.CheckPassword(user.PasswordHash, password) {
		renderErr()
		return
	}

	if err := s.startSession(w, r, user.ID); err != nil {
		slog.Error("start session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/dashboard"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// ── Auth: Logout ──────────────────────────────────────────────────────────────

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// CSRF check is optional here since logout only affects the user's own session.
	if sess := sessionFromCtx(r); sess != nil {
		_ = s.db.DeleteSession(sess.Token)
	}
	http.SetCookie(w, &http.Cookie{
		Name: "session", Value: "", MaxAge: -1, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── Auth: Password reset ──────────────────────────────────────────────────────

func (s *Server) handleForgotForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "forgot", TemplateData{Title: "Reset password"})
}

func (s *Server) handleForgotSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))

	// Always show success to avoid user enumeration.
	success := TemplateData{
		Title:     "Reset password",
		Flash:     "If that email is registered, a reset link has been sent.",
		FlashType: "success",
	}

	user, err := s.db.GetUserByEmail(email)
	if err != nil || user == nil {
		s.render(w, r, "forgot", success)
		return
	}

	token, err := auth.GenerateToken()
	if err != nil {
		slog.Error("generate reset token", "err", err)
		s.render(w, r, "forgot", success)
		return
	}
	expiresAt := time.Now().Add(s.cfg.ResetTokenTTL)
	if err := s.db.CreateResetToken(token, user.ID, expiresAt); err != nil {
		slog.Error("create reset token", "err", err)
		s.render(w, r, "forgot", success)
		return
	}

	resetURL := fmt.Sprintf("%s/reset/%s", s.cfg.BaseURL, token)
	body := fmt.Sprintf(
		"Hello,\n\nClick the link below to reset your password (expires in 1 hour):\n\n%s\n\n"+
			"If you did not request this, you can safely ignore this email.\n\n— Freerider Watcher\n",
		resetURL,
	)
	if err := s.emailer.Send(user.Email, "Reset your Freerider Watcher password", body); err != nil {
		slog.Warn("reset email failed", "err", err)
		// Log the link so operators can test locally when Mailgun is not configured.
		slog.Info("RESET LINK (Mailgun not configured)", "url", resetURL)
	}

	s.render(w, r, "forgot", success)
}

func (s *Server) handleResetForm(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	rt, err := s.db.GetResetToken(token)
	if err != nil || rt == nil {
		s.render(w, r, "reset", TemplateData{
			Title:     "Reset password",
			Flash:     "This reset link is invalid or has expired.",
			FlashType: "error",
		})
		return
	}
	s.render(w, r, "reset", TemplateData{
		Title: "Reset password",
		Data:  token,
	})
}

func (s *Server) handleResetSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	renderErr := func(msg string) {
		s.render(w, r, "reset", TemplateData{
			Title: "Reset password", Flash: msg, FlashType: "error", Data: token,
		})
	}

	if len(password) < 8 {
		renderErr("Password must be at least 8 characters.")
		return
	}
	if password != confirm {
		renderErr("Passwords do not match.")
		return
	}

	rt, err := s.db.GetResetToken(token)
	if err != nil || rt == nil {
		renderErr("This reset link is invalid or has expired.")
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		renderErr("An error occurred. Please try again.")
		return
	}
	if err := s.db.UpdateUserPassword(rt.UserID, hash); err != nil {
		slog.Error("update password", "err", err)
		renderErr("An error occurred. Please try again.")
		return
	}
	_ = s.db.MarkResetTokenUsed(token)

	s.redirect(w, r, "/login", "Password updated. You can now log in.", "success")
}

// ── Dashboard ─────────────────────────────────────────────────────────────────

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r)
	watches, err := s.db.GetWatchesByUser(user.ID)
	if err != nil {
		slog.Error("get watches", "err", err)
		watches = nil
	}

	pushSubs, err := s.db.GetPushSubscriptionsByUser(user.ID)
	if err != nil {
		slog.Error("get push subs", "err", err)
	}

	type dashData struct {
		Watches      []db.Watch
		PushSubCount int
		PushEnabled  bool
	}

	s.render(w, r, "dashboard", TemplateData{
		Title: "Dashboard",
		Data: dashData{
			Watches:      watches,
			PushSubCount: len(pushSubs),
			PushEnabled:  s.pusher.Configured(),
		},
	})
}

// ── Watches ───────────────────────────────────────────────────────────────────

func (s *Server) handleWatchNewForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "watch_new", TemplateData{Title: "New watch"})
}

func (s *Server) handleWatchNewSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	user := userFromCtx(r)
	origin := strings.TrimSpace(r.FormValue("origin"))
	destination := strings.TrimSpace(r.FormValue("destination"))

	if origin == "" || destination == "" {
		s.render(w, r, "watch_new", TemplateData{
			Title:     "New watch",
			Flash:     "Origin and destination are required.",
			FlashType: "error",
		})
		return
	}

	w2 := db.Watch{
		UserID:       user.ID,
		Origin:       origin,
		Destination:  destination,
		EarliestTime: strings.TrimSpace(r.FormValue("earliest_time")),
		LatestTime:   strings.TrimSpace(r.FormValue("latest_time")),
		Weekdays:     buildWeekdays(r.Form["weekdays"]),
		OneTime:      r.FormValue("one_time") == "1",
	}

	watchID, err := s.db.CreateWatch(w2)
	if err != nil {
		slog.Error("create watch", "err", err)
		s.render(w, r, "watch_new", TemplateData{
			Title:     "New watch",
			Flash:     "An error occurred. Please try again.",
			FlashType: "error",
		})
		return
	}
	w2.ID = watchID

	// Scan existing cached routes immediately so the user gets notified if
	// there are already matching routes in the DB.
	go s.fetcher.ScanWatchAgainstExistingRoutes(r.Context(), w2)

	s.redirect(w, r, "/dashboard", "Watch created!", "success")
}

func (s *Server) handleWatchEditForm(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	watch, err := s.db.GetWatchByID(id, user.ID)
	if err != nil || watch == nil {
		s.redirect(w, r, "/dashboard", "Watch not found.", "error")
		return
	}
	s.render(w, r, "watch_edit", TemplateData{Title: "Edit watch", Data: watch})
}

func (s *Server) handleWatchEditSubmit(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	existing, err := s.db.GetWatchByID(id, user.ID)
	if err != nil || existing == nil {
		s.redirect(w, r, "/dashboard", "Watch not found.", "error")
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	origin := strings.TrimSpace(r.FormValue("origin"))
	destination := strings.TrimSpace(r.FormValue("destination"))
	if origin == "" || destination == "" {
		s.render(w, r, "watch_edit", TemplateData{
			Title:     "Edit watch",
			Flash:     "Origin and destination are required.",
			FlashType: "error",
			Data:      existing,
		})
		return
	}

	updated := db.Watch{
		ID:           id,
		UserID:       user.ID,
		Origin:       origin,
		Destination:  destination,
		EarliestTime: strings.TrimSpace(r.FormValue("earliest_time")),
		LatestTime:   strings.TrimSpace(r.FormValue("latest_time")),
		Weekdays:     buildWeekdays(r.Form["weekdays"]),
		OneTime:      r.FormValue("one_time") == "1",
		Active:       existing.Active,
	}

	if err := s.db.UpdateWatch(updated); err != nil {
		slog.Error("update watch", "err", err)
		s.render(w, r, "watch_edit", TemplateData{
			Title:     "Edit watch",
			Flash:     "An error occurred. Please try again.",
			FlashType: "error",
			Data:      existing,
		})
		return
	}

	// Re-scan existing cached routes against the updated watch so the user
	// gets notified immediately if there are already matching routes in the DB.
	go s.fetcher.ScanWatchAgainstExistingRoutes(r.Context(), updated)

	s.redirect(w, r, "/dashboard", "Watch updated!", "success")
}

func (s *Server) handleWatchDelete(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r)
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := s.db.DeleteWatch(id, user.ID); err != nil {
		slog.Error("delete watch", "err", err)
		s.redirect(w, r, "/dashboard", "Failed to delete watch.", "error")
		return
	}
	s.redirect(w, r, "/dashboard", "Watch deleted.", "success")
}

func (s *Server) handleWatchDetail(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	watch, err := s.db.GetWatchByID(id, user.ID)
	if err != nil || watch == nil {
		s.redirect(w, r, "/dashboard", "Watch not found.", "error")
		return
	}

	allRoutes, err := s.db.GetAllRoutes()
	if err != nil {
		slog.Error("get routes for watch detail", "err", err)
	}

	var matched []db.Route
	for _, route := range allRoutes {
		if matcher.Match(route, *watch) {
			matched = append(matched, route)
		}
	}
	// Sort upcoming first.
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].DepartureAt.Before(matched[j].DepartureAt)
	})

	type watchDetailData struct {
		Watch  *db.Watch
		Routes []db.Route
	}
	s.render(w, r, "watch_detail", TemplateData{
		Title: watch.Origin + " → " + watch.Destination,
		Data:  watchDetailData{Watch: watch, Routes: matched},
	})
}

// buildWeekdays converts a slice of weekday string values to a comma-separated list.
func buildWeekdays(vals []string) string {
	valid := map[string]bool{"1": true, "2": true, "3": true, "4": true, "5": true, "6": true, "7": true}
	var out []string
	seen := map[string]bool{}
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if valid[v] && !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return strings.Join(out, ",")
}

// ── Push notifications ────────────────────────────────────────────────────────

type pushSubscribeBody struct {
	Endpoint       string `json:"endpoint"`
	ExpirationTime *int64 `json:"expirationTime"`
	Keys           struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r)
	var body pushSubscribeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Endpoint == "" || body.Keys.P256dh == "" || body.Keys.Auth == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}

	sub := db.PushSubscription{
		UserID:    user.ID,
		Endpoint:  body.Endpoint,
		P256dh:    body.Keys.P256dh,
		AuthKey:   body.Keys.Auth,
		UserAgent: r.Header.Get("User-Agent"),
	}
	if err := s.db.UpsertPushSubscription(sub); err != nil {
		slog.Error("upsert push sub", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r)
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_ = s.db.DeletePushSubscription(user.ID, body.Endpoint)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r)
	subs, err := s.db.GetPushSubscriptionsByUser(user.ID)
	if err != nil || len(subs) == 0 {
		http.Error(w, "no subscriptions found", http.StatusBadRequest)
		return
	}

	payload := notify.PushPayload{
		Title: "Test notification",
		Body:  "Push notifications are working!",
		URL:   "/dashboard",
	}
	for _, sub := range subs {
		if err := s.pusher.Send(sub, payload); err != nil {
			if err == notify.ErrGone {
				_ = s.db.DeletePushSubscriptionByEndpoint(sub.Endpoint)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Routes (debug) ────────────────────────────────────────────────────────────

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	routes, err := s.db.GetAllRoutes()
	if err != nil {
		slog.Error("get routes", "err", err)
		routes = nil
	}
	s.render(w, r, "routes", TemplateData{
		Title: "Cached routes",
		Data:  routes,
	})
}

// ── Session helpers ───────────────────────────────────────────────────────────

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := auth.GenerateToken()
	if err != nil {
		return err
	}
	csrf, err := auth.GenerateToken()
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(s.cfg.SessionMaxAge)
	if err := s.db.CreateSession(token, csrf, userID, expiresAt); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.SessionMaxAge.Seconds()),
	})
	return nil
}
