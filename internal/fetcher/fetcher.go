// Package fetcher periodically polls the Hertz Freerider API, stores new routes,
// matches them against active watches, and sends notifications.
package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jagduvi1/freeride-watcher/internal/config"
	"github.com/jagduvi1/freeride-watcher/internal/db"
	"github.com/jagduvi1/freeride-watcher/internal/matcher"
	"github.com/jagduvi1/freeride-watcher/internal/notify"
)

// Status is readable by the HTTP layer to show on the home page.
type Status struct {
	LastFetchAt    time.Time
	LastFetchError string
	RouteCount     int64
}

// Fetcher runs the background polling loop.
type Fetcher struct {
	cfg     *config.Config
	db      *db.DB
	pusher  *notify.Pusher
	emailer *notify.Emailer

	status atomic.Value // holds *Status

	httpClient *http.Client
	etag       string
}

// New creates a Fetcher.
func New(cfg *config.Config, database *db.DB, pusher *notify.Pusher, emailer *notify.Emailer) *Fetcher {
	f := &Fetcher{
		cfg:     cfg,
		db:      database,
		pusher:  pusher,
		emailer: emailer,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	f.status.Store(&Status{})
	return f
}

// GetStatus returns a snapshot of the last fetch status.
func (f *Fetcher) GetStatus() *Status {
	return f.status.Load().(*Status)
}

// Start runs the fetch loop until ctx is cancelled.
// It runs an immediate fetch, then repeats at cfg.FetchInterval ± cfg.FetchJitter.
func (f *Fetcher) Start(ctx context.Context) {
	slog.Info("fetcher starting", "interval", f.cfg.FetchInterval, "jitter", f.cfg.FetchJitter)

	// Immediate first run
	f.runOnce(ctx)

	backoff := time.Second
	const maxBackoff = 10 * time.Minute

	for {
		jitter := time.Duration(rand.Int64N(int64(f.cfg.FetchJitter)*2)) - f.cfg.FetchJitter
		next := f.cfg.FetchInterval + jitter

		select {
		case <-ctx.Done():
			slog.Info("fetcher stopped")
			return
		case <-time.After(next):
		}

		err := f.runOnce(ctx)
		if err != nil {
			slog.Warn("fetch error, backing off", "backoff", backoff, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		} else {
			backoff = time.Second // reset on success
		}
	}
}

// runOnce fetches, stores, matches and notifies. Returns the first error encountered.
func (f *Fetcher) runOnce(ctx context.Context) error {
	routes, err := f.fetch(ctx)
	st := &Status{LastFetchAt: time.Now()}

	if err != nil {
		st.LastFetchError = err.Error()
		f.status.Store(st)
		return err
	}

	newCount := 0
	for _, r := range routes {
		isNew, upsertErr := f.db.UpsertRoute(r)
		if upsertErr != nil {
			slog.Warn("upsert route failed", "route_id", r.RouteID, "err", upsertErr)
			continue
		}
		if isNew {
			newCount++
			slog.Info("new route found", "route_id", r.RouteID, "origin", r.Origin, "destination", r.Destination, "departure", r.DepartureAt.Format("2006-01-02 15:04"))
			f.notify(ctx, r)
		}
	}

	if newCount > 0 {
		slog.Info("fetch complete", "total", len(routes), "new", newCount)
	}

	count, _ := f.db.CountRoutes()
	st.RouteCount = int64(count)
	f.status.Store(st)

	return nil
}

// ── API client ────────────────────────────────────────────────────────────────

// apiGroup mirrors the top-level JSON structure returned by the Hertz Freerider API.
// The API returns an array of groups, each representing a pickup→return location pair.
type apiGroup struct {
	PickupLocationName string     `json:"pickupLocationName"`
	ReturnLocationName string     `json:"returnLocationName"`
	Routes             []apiRoute `json:"routes"`
}

type apiRoute struct {
	ID               int64       `json:"id"`
	TransportOfferID int64       `json:"transportOfferId"`
	PickupLocation   apiLocation `json:"pickupLocation"`
	ReturnLocation   apiLocation `json:"returnLocation"`
	AvailableAt      hertzTime   `json:"availableAt"`
	LatestReturn     hertzTime   `json:"latestReturn"`
	ExpireTime       hertzTime   `json:"expireTime"`
	CarModel         string      `json:"carModel"`
	PublicDescription string     `json:"publicDescription"`
	Distance         float64     `json:"distance"`
	TravelTime       int         `json:"travelTime"`
}

// hertzTime unmarshals timestamps that the Hertz API returns without a
// timezone suffix (e.g. "2026-03-03T11:45:00"). Falls back to RFC3339 if
// the suffix is present. Times without a zone are treated as UTC.
type hertzTime struct{ time.Time }

func (t *hertzTime) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339, s); err == nil {
		t.Time = parsed
		return nil
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		t.Time = parsed.UTC()
		return nil
	}
	return fmt.Errorf("cannot parse hertz timestamp %q", s)
}

type apiLocation struct {
	Name    string  `json:"name"`
	City    string  `json:"city"`
	Country string  `json:"country"`
	Address string  `json:"address"`
	GeoLat  float64 `json:"geoLat"`
	GeoLon  float64 `json:"geoLon"`
}

func (l apiLocation) displayCity() string {
	if l.City != "" {
		return l.City
	}
	return l.Name
}

func (f *Fetcher) fetch(ctx context.Context) ([]db.Route, error) {
	slog.Info("fetching routes", "url", f.cfg.HertzAPIURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.cfg.HertzAPIURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "freeride-watcher/1.0 (github.com/jagduvi1/freeride-watcher)")
	if f.etag != "" {
		req.Header.Set("If-None-Match", f.etag)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	slog.Info("api response", "status", resp.StatusCode)

	if resp.StatusCode == http.StatusNotModified {
		slog.Info("routes unchanged (304 Not Modified)")
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		f.etag = etag
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	slog.Debug("api raw response (first 512 bytes)", "body", preview(body, 512))

	routes, parseErr := parseRoutes(body)
	if parseErr != nil {
		return nil, parseErr
	}
	slog.Info("routes parsed", "count", len(routes))
	return routes, nil
}

func parseRoutes(body []byte) ([]db.Route, error) {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, nil
	}

	var groups []apiGroup
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	var routes []db.Route
	for _, g := range groups {
		for _, ar := range g.Routes {
			raw, _ := json.Marshal(ar)
			r := db.Route{
				RouteID:        fmt.Sprintf("hertz-%d", ar.ID),
				Origin:         ar.PickupLocation.displayCity(),
				Destination:    ar.ReturnLocation.displayCity(),
				DepartureAt:    ar.AvailableAt.Time,
				AvailableUntil: ar.LatestReturn.Time,
				CarModel:       ar.CarModel,
				RawJSON:        string(raw),
			}
			// Fallback: if city is blank, use group-level names.
			if r.Origin == "" {
				r.Origin = g.PickupLocationName
			}
			if r.Destination == "" {
				r.Destination = g.ReturnLocationName
			}
			if !r.DepartureAt.IsZero() {
				routes = append(routes, r)
			}
		}
	}
	return routes, nil
}

// ── Matching & notification ───────────────────────────────────────────────────

// ScanWatchAgainstExistingRoutes checks all cached routes in the DB against
// a single watch and sends notifications for any matches not already notified.
// Called after a watch is created or edited so the user gets immediate results.
func (f *Fetcher) ScanWatchAgainstExistingRoutes(ctx context.Context, watch db.Watch) {
	routes, err := f.db.GetAllRoutes()
	if err != nil {
		slog.Warn("scan watch: get routes failed", "err", err)
		return
	}

	matched := 0
	for _, route := range routes {
		if !matcher.Match(route, watch) {
			continue
		}
		already, err := f.db.HasBeenNotified(watch.UserID, route.RouteID)
		if err != nil || already {
			continue
		}
		slog.Info("scan watch matched existing route", "watch_id", watch.ID, "route_id", route.RouteID, "origin", route.Origin, "destination", route.Destination)
		if err := f.db.MarkNotified(watch.UserID, route.RouteID); err != nil {
			slog.Warn("mark notified failed", "err", err)
			continue
		}
		f.sendNotification(ctx, watch.UserID, route)
		matched++
	}
	if matched > 0 {
		slog.Info("scan watch complete", "watch_id", watch.ID, "matched", matched)
	}
}

func (f *Fetcher) notify(ctx context.Context, route db.Route) {
	watches, err := f.db.GetAllActiveWatches()
	if err != nil {
		slog.Warn("get watches failed", "err", err)
		return
	}

	// Collect distinct user IDs that have a matching watch.
	notifyUsers := map[int64]db.Watch{}
	for _, w := range watches {
		if !matcher.Match(route, w) {
			continue
		}
		already, err := f.db.HasBeenNotified(w.UserID, route.RouteID)
		if err != nil || already {
			continue
		}
		notifyUsers[w.UserID] = w
	}

	if len(notifyUsers) > 0 {
		slog.Info("notifying users", "route_id", route.RouteID, "user_count", len(notifyUsers))
	}

	for userID, watch := range notifyUsers {
		if err := f.db.MarkNotified(userID, route.RouteID); err != nil {
			slog.Warn("mark notified failed", "err", err)
			continue
		}
		f.sendNotification(ctx, userID, route)

		// Deactivate one-time watches.
		if watch.OneTime {
			_ = f.db.DeactivateWatch(watch.ID)
		}
	}
}

// sendNotification delivers a push (and email fallback) to a single user for a route.
func (f *Fetcher) sendNotification(ctx context.Context, userID int64, route db.Route) {
	payload := notify.PushPayload{
		Title: "🚗 Freerider route available!",
		Body: fmt.Sprintf("%s → %s on %s",
			route.Origin, route.Destination,
			route.DepartureAt.Format("Mon Jan 2 at 15:04")),
		URL: "/dashboard",
	}

	subs, err := f.db.GetPushSubscriptionsByUser(userID)
	if err != nil {
		slog.Warn("get push subs failed", "user_id", userID, "err", err)
	}
	for _, sub := range subs {
		if err := f.pusher.Send(sub, payload); err != nil {
			if err == notify.ErrGone {
				_ = f.db.DeletePushSubscriptionByEndpoint(sub.Endpoint)
			} else {
				slog.Warn("push send failed", "user_id", userID, "err", err)
			}
		}
	}

	// Email fallback if user has no active push subscriptions.
	if len(subs) == 0 && f.emailer != nil {
		user, _ := f.db.GetUserByID(userID)
		if user != nil {
			body := fmt.Sprintf(
				"Hello,\n\nA Hertz Freerider route matching your watch was found:\n\n"+
					"  %s → %s\n  Available: %s\n  Car: %s\n\n"+
					"Log in at %s to see all routes.\n\n"+
					"— Freerider Watcher\n",
				route.Origin, route.Destination,
				route.DepartureAt.Format("Mon Jan 2 2006 at 15:04"),
				route.CarModel,
				f.cfg.BaseURL,
			)
			if err := f.emailer.Send(user.Email, "Freerider route available!", body); err != nil {
				slog.Warn("email send failed", "user_id", userID, "err", err)
			}
		}
	}
}

// preview returns the first n bytes of b as a string (safe slice).
func preview(b []byte, n int) string {
	if len(b) < n {
		n = len(b)
	}
	return string(b[:n])
}
