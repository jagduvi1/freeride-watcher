// Package db provides SQLite persistence for freeride-watcher.
// A single *DB wraps sql.DB and exposes typed query methods.
package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the underlying sql.DB.
type DB struct {
	*sql.DB
}

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*DB, error) {
	// WAL mode + 5 s busy timeout; pure-Go driver, so one writer at a time.
	dsn := path + "?_journal=WAL&_timeout=5000&_foreign_keys=on"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	db := &DB{sqlDB}
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func (db *DB) migrate() error {
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS config (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		email         TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS sessions (
		token         TEXT PRIMARY KEY,
		user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		csrf_token    TEXT NOT NULL,
		flash_message TEXT NOT NULL DEFAULT '',
		flash_type    TEXT NOT NULL DEFAULT '',
		expires_at    DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS reset_tokens (
		token      TEXT PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		expires_at DATETIME NOT NULL,
		used       INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS watches (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		origin        TEXT NOT NULL,
		destination   TEXT NOT NULL,
		earliest_time TEXT NOT NULL DEFAULT '',
		latest_time   TEXT NOT NULL DEFAULT '',
		weekdays      TEXT NOT NULL DEFAULT '',
		one_time      INTEGER NOT NULL DEFAULT 0,
		active        INTEGER NOT NULL DEFAULT 1,
		created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS routes (
		route_id        TEXT PRIMARY KEY,
		origin          TEXT NOT NULL,
		destination     TEXT NOT NULL,
		departure_at    DATETIME NOT NULL,
		available_until DATETIME,
		car_model       TEXT NOT NULL DEFAULT '',
		raw_json        TEXT NOT NULL DEFAULT '',
		first_seen      DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_routes_departure ON routes(departure_at);

	CREATE TABLE IF NOT EXISTS notified (
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		route_id   TEXT    NOT NULL REFERENCES routes(route_id) ON DELETE CASCADE,
		notified_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (user_id, route_id)
	);

	CREATE TABLE IF NOT EXISTS push_subscriptions (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		endpoint   TEXT UNIQUE NOT NULL,
		p256dh     TEXT NOT NULL,
		auth_key   TEXT NOT NULL,
		user_agent TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	`)
	return err
}

// ── Config ──────────────────────────────────────────────────────────────────

func (db *DB) GetConfig(key string) (string, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (db *DB) SetConfig(key, value string) error {
	_, err := db.Exec(
		`INSERT INTO config(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// ── Users ────────────────────────────────────────────────────────────────────

type User struct {
	ID           int64
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

func (db *DB) CreateUser(email, hash string) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO users(email,password_hash) VALUES(?,?)`, email, hash)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) GetUserByEmail(email string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		`SELECT id,email,password_hash,created_at FROM users WHERE email=?`,
		strings.ToLower(strings.TrimSpace(email)),
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (db *DB) GetUserByID(id int64) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		`SELECT id,email,password_hash,created_at FROM users WHERE id=?`, id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (db *DB) UpdateUserPassword(userID int64, hash string) error {
	_, err := db.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hash, userID)
	return err
}

// ── Sessions ─────────────────────────────────────────────────────────────────

type Session struct {
	Token        string
	UserID       int64
	CSRFToken    string
	FlashMessage string
	FlashType    string
	ExpiresAt    time.Time
}

func (db *DB) CreateSession(token, csrfToken string, userID int64, expiresAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO sessions(token,user_id,csrf_token,expires_at) VALUES(?,?,?,?)`,
		token, userID, csrfToken, expiresAt)
	return err
}

func (db *DB) GetSession(token string) (*Session, error) {
	s := &Session{}
	err := db.QueryRow(
		`SELECT token,user_id,csrf_token,flash_message,flash_type,expires_at
		 FROM sessions WHERE token=? AND expires_at > CURRENT_TIMESTAMP`,
		token,
	).Scan(&s.Token, &s.UserID, &s.CSRFToken, &s.FlashMessage, &s.FlashType, &s.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

// SetFlash stores a one-time flash message on the session.
func (db *DB) SetFlash(token, message, flashType string) error {
	_, err := db.Exec(
		`UPDATE sessions SET flash_message=?,flash_type=? WHERE token=?`,
		message, flashType, token)
	return err
}

// ClearFlash removes the flash message after it has been read.
func (db *DB) ClearFlash(token string) error {
	_, err := db.Exec(
		`UPDATE sessions SET flash_message='',flash_type='' WHERE token=?`, token)
	return err
}

func (db *DB) DeleteSession(token string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token=?`, token)
	return err
}

func (db *DB) PruneExpiredSessions() error {
	_, err := db.Exec(`DELETE FROM sessions WHERE expires_at <= CURRENT_TIMESTAMP`)
	return err
}

// ── Reset tokens ─────────────────────────────────────────────────────────────

type ResetToken struct {
	Token     string
	UserID    int64
	ExpiresAt time.Time
	Used      bool
}

func (db *DB) CreateResetToken(token string, userID int64, expiresAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO reset_tokens(token,user_id,expires_at) VALUES(?,?,?)`,
		token, userID, expiresAt)
	return err
}

func (db *DB) GetResetToken(token string) (*ResetToken, error) {
	rt := &ResetToken{}
	var used int
	err := db.QueryRow(
		`SELECT token,user_id,expires_at,used FROM reset_tokens
		 WHERE token=? AND expires_at > CURRENT_TIMESTAMP AND used=0`,
		token,
	).Scan(&rt.Token, &rt.UserID, &rt.ExpiresAt, &used)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	rt.Used = used == 1
	return rt, err
}

func (db *DB) MarkResetTokenUsed(token string) error {
	_, err := db.Exec(`UPDATE reset_tokens SET used=1 WHERE token=?`, token)
	return err
}

// ── Watches ──────────────────────────────────────────────────────────────────

type Watch struct {
	ID           int64
	UserID       int64
	Origin       string
	Destination  string
	EarliestTime string // "HH:MM" or ""
	LatestTime   string // "HH:MM" or ""
	Weekdays     string // comma-separated ISO weekdays e.g. "1,3,5" (Mon=1,Sun=7), "" = any
	OneTime      bool
	Active       bool
	CreatedAt    time.Time
}

func (db *DB) CreateWatch(w Watch) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO watches(user_id,origin,destination,earliest_time,latest_time,weekdays,one_time)
		 VALUES(?,?,?,?,?,?,?)`,
		w.UserID, w.Origin, w.Destination, w.EarliestTime, w.LatestTime, w.Weekdays, boolInt(w.OneTime))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) GetWatchesByUser(userID int64) ([]Watch, error) {
	rows, err := db.Query(
		`SELECT id,user_id,origin,destination,earliest_time,latest_time,weekdays,one_time,active,created_at
		 FROM watches WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWatches(rows)
}

func (db *DB) GetAllActiveWatches() ([]Watch, error) {
	rows, err := db.Query(
		`SELECT id,user_id,origin,destination,earliest_time,latest_time,weekdays,one_time,active,created_at
		 FROM watches WHERE active=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWatches(rows)
}

func (db *DB) GetWatchByID(id, userID int64) (*Watch, error) {
	w := &Watch{}
	var oneTime, active int
	err := db.QueryRow(
		`SELECT id,user_id,origin,destination,earliest_time,latest_time,weekdays,one_time,active,created_at
		 FROM watches WHERE id=? AND user_id=?`, id, userID,
	).Scan(&w.ID, &w.UserID, &w.Origin, &w.Destination,
		&w.EarliestTime, &w.LatestTime, &w.Weekdays, &oneTime, &active, &w.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	w.OneTime = oneTime == 1
	w.Active = active == 1
	return w, err
}

func (db *DB) UpdateWatch(w Watch) error {
	_, err := db.Exec(
		`UPDATE watches SET origin=?,destination=?,earliest_time=?,latest_time=?,weekdays=?,one_time=?,active=?
		 WHERE id=? AND user_id=?`,
		w.Origin, w.Destination, w.EarliestTime, w.LatestTime, w.Weekdays,
		boolInt(w.OneTime), boolInt(w.Active), w.ID, w.UserID)
	return err
}

func (db *DB) DeleteWatch(id, userID int64) error {
	_, err := db.Exec(`DELETE FROM watches WHERE id=? AND user_id=?`, id, userID)
	return err
}

func (db *DB) DeactivateWatch(id int64) error {
	_, err := db.Exec(`UPDATE watches SET active=0 WHERE id=?`, id)
	return err
}

func scanWatches(rows *sql.Rows) ([]Watch, error) {
	var watches []Watch
	for rows.Next() {
		var w Watch
		var oneTime, active int
		if err := rows.Scan(&w.ID, &w.UserID, &w.Origin, &w.Destination,
			&w.EarliestTime, &w.LatestTime, &w.Weekdays, &oneTime, &active, &w.CreatedAt); err != nil {
			return nil, err
		}
		w.OneTime = oneTime == 1
		w.Active = active == 1
		watches = append(watches, w)
	}
	return watches, rows.Err()
}

// ── Routes ───────────────────────────────────────────────────────────────────

type Route struct {
	RouteID        string
	Origin         string
	Destination    string
	DepartureAt    time.Time
	AvailableUntil time.Time
	CarModel       string
	RawJSON        string
	FirstSeen      time.Time
}

// UpsertRoute inserts a route or ignores if already exists.
// Returns true if it was newly inserted (i.e. a new route).
func (db *DB) UpsertRoute(r Route) (isNew bool, err error) {
	res, err := db.Exec(
		`INSERT OR IGNORE INTO routes(route_id,origin,destination,departure_at,available_until,car_model,raw_json)
		 VALUES(?,?,?,?,?,?,?)`,
		r.RouteID, r.Origin, r.Destination, r.DepartureAt, r.AvailableUntil, r.CarModel, r.RawJSON)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, nil
	}
	// Existing route: update mutable fields so names stay correct if the API
	// or our parsing logic changes (e.g. city→name fix). first_seen is preserved.
	_, err = db.Exec(
		`UPDATE routes SET origin=?,destination=?,available_until=?,car_model=?,raw_json=?
		 WHERE route_id=?`,
		r.Origin, r.Destination, r.AvailableUntil, r.CarModel, r.RawJSON, r.RouteID)
	return false, err
}

func (db *DB) GetAllRoutes() ([]Route, error) {
	rows, err := db.Query(
		`SELECT route_id,origin,destination,departure_at,available_until,car_model,raw_json,first_seen
		 FROM routes ORDER BY departure_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRoutes(rows)
}

func (db *DB) SearchRoutes(q string, limit, offset int) ([]Route, error) {
	var rows *sql.Rows
	var err error
	if q == "" {
		rows, err = db.Query(
			`SELECT route_id,origin,destination,departure_at,available_until,car_model,raw_json,first_seen
			 FROM routes ORDER BY departure_at DESC LIMIT ? OFFSET ?`,
			limit, offset)
	} else {
		like := "%" + q + "%"
		rows, err = db.Query(
			`SELECT route_id,origin,destination,departure_at,available_until,car_model,raw_json,first_seen
			 FROM routes
			 WHERE origin LIKE ? OR destination LIKE ? OR car_model LIKE ?
			 ORDER BY departure_at DESC LIMIT ? OFFSET ?`,
			like, like, like, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRoutes(rows)
}

func (db *DB) CountSearchRoutes(q string) (int, error) {
	if q == "" {
		return db.CountRoutes()
	}
	like := "%" + q + "%"
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM routes WHERE origin LIKE ? OR destination LIKE ? OR car_model LIKE ?`,
		like, like, like).Scan(&n)
	return n, err
}

func (db *DB) CountRoutes() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM routes`).Scan(&n)
	return n, err
}

func scanRoutes(rows *sql.Rows) ([]Route, error) {
	var routes []Route
	for rows.Next() {
		var r Route
		var until sql.NullTime
		if err := rows.Scan(&r.RouteID, &r.Origin, &r.Destination, &r.DepartureAt,
			&until, &r.CarModel, &r.RawJSON, &r.FirstSeen); err != nil {
			return nil, err
		}
		if until.Valid {
			r.AvailableUntil = until.Time
		}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

// ── Notifications ────────────────────────────────────────────────────────────

func (db *DB) HasBeenNotified(userID int64, routeID string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM notified WHERE user_id=? AND route_id=?`, userID, routeID,
	).Scan(&n)
	return n > 0, err
}

func (db *DB) MarkNotified(userID int64, routeID string) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO notified(user_id,route_id) VALUES(?,?)`, userID, routeID)
	return err
}

// ── Push subscriptions ───────────────────────────────────────────────────────

type PushSubscription struct {
	ID        int64
	UserID    int64
	Endpoint  string
	P256dh    string
	AuthKey   string
	UserAgent string
	CreatedAt time.Time
}

func (db *DB) UpsertPushSubscription(s PushSubscription) error {
	_, err := db.Exec(
		`INSERT INTO push_subscriptions(user_id,endpoint,p256dh,auth_key,user_agent) VALUES(?,?,?,?,?)
		 ON CONFLICT(endpoint) DO UPDATE SET p256dh=excluded.p256dh, auth_key=excluded.auth_key,
		 user_agent=excluded.user_agent, user_id=excluded.user_id`,
		s.UserID, s.Endpoint, s.P256dh, s.AuthKey, s.UserAgent)
	return err
}

func (db *DB) DeletePushSubscription(userID int64, endpoint string) error {
	_, err := db.Exec(
		`DELETE FROM push_subscriptions WHERE user_id=? AND endpoint=?`, userID, endpoint)
	return err
}

func (db *DB) DeletePushSubscriptionByEndpoint(endpoint string) error {
	_, err := db.Exec(`DELETE FROM push_subscriptions WHERE endpoint=?`, endpoint)
	return err
}

func (db *DB) GetPushSubscriptionsByUser(userID int64) ([]PushSubscription, error) {
	rows, err := db.Query(
		`SELECT id,user_id,endpoint,p256dh,auth_key,user_agent,created_at
		 FROM push_subscriptions WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPushSubs(rows)
}

func (db *DB) GetAllPushSubscriptionsByUser(userID int64) ([]PushSubscription, error) {
	return db.GetPushSubscriptionsByUser(userID)
}

func (db *DB) GetAllPushSubscriptions() ([]PushSubscription, error) {
	rows, err := db.Query(
		`SELECT id,user_id,endpoint,p256dh,auth_key,user_agent,created_at FROM push_subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPushSubs(rows)
}

func scanPushSubs(rows *sql.Rows) ([]PushSubscription, error) {
	var subs []PushSubscription
	for rows.Next() {
		var s PushSubscription
		if err := rows.Scan(&s.ID, &s.UserID, &s.Endpoint, &s.P256dh, &s.AuthKey,
			&s.UserAgent, &s.CreatedAt); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// ── Admin queries ─────────────────────────────────────────────────────────────

// AdminUser is a User enriched with aggregate counts for the admin view.
type AdminUser struct {
	User
	WatchCount   int
	PushSubCount int
}

// GetAllUsers returns all users with their watch and push subscription counts.
func (db *DB) GetAllUsers() ([]AdminUser, error) {
	rows, err := db.Query(`
		SELECT u.id, u.email, u.password_hash, u.created_at,
		       COUNT(DISTINCT w.id)  AS watch_count,
		       COUNT(DISTINCT ps.id) AS push_count
		FROM users u
		LEFT JOIN watches w  ON w.user_id = u.id
		LEFT JOIN push_subscriptions ps ON ps.user_id = u.id
		GROUP BY u.id
		ORDER BY u.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []AdminUser
	for rows.Next() {
		var au AdminUser
		if err := rows.Scan(&au.ID, &au.Email, &au.PasswordHash, &au.CreatedAt,
			&au.WatchCount, &au.PushSubCount); err != nil {
			return nil, err
		}
		users = append(users, au)
	}
	return users, rows.Err()
}

// DeleteUser removes a user and all their associated data (cascades via FK).
func (db *DB) DeleteUser(userID int64) error {
	_, err := db.Exec(`DELETE FROM users WHERE id=?`, userID)
	return err
}

// DeleteWatchByID removes a watch regardless of owner (admin use).
func (db *DB) DeleteWatchByID(id int64) error {
	_, err := db.Exec(`DELETE FROM watches WHERE id=?`, id)
	return err
}

// NotificationHistoryRow is one row in the admin notification log.
type NotificationHistoryRow struct {
	UserEmail   string
	Origin      string
	Destination string
	NotifiedAt  time.Time
}

// GetNotificationHistory returns the most recent notification events.
func (db *DB) GetNotificationHistory(limit int) ([]NotificationHistoryRow, error) {
	rows, err := db.Query(`
		SELECT u.email, r.origin, r.destination, n.notified_at
		FROM notified n
		JOIN users  u ON u.id = n.user_id
		JOIN routes r ON r.route_id = n.route_id
		ORDER BY n.notified_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NotificationHistoryRow
	for rows.Next() {
		var row NotificationHistoryRow
		if err := rows.Scan(&row.UserEmail, &row.Origin, &row.Destination, &row.NotifiedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SystemStats contains aggregate counts for the admin overview.
type SystemStats struct {
	UserCount         int
	WatchCount        int
	RouteCount        int
	NotificationCount int
	LastFetchAt       time.Time
	LastFetchError    string
}

// GetSystemStats queries aggregate counts from the DB.
// LastFetchAt and LastFetchError must be populated by the caller from the fetcher.
func (db *DB) GetSystemStats() (SystemStats, error) {
	var s SystemStats
	row := db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM users),
			(SELECT COUNT(*) FROM watches WHERE active=1),
			(SELECT COUNT(*) FROM routes),
			(SELECT COUNT(*) FROM notified)
	`)
	err := row.Scan(&s.UserCount, &s.WatchCount, &s.RouteCount, &s.NotificationCount)
	return s, err
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
