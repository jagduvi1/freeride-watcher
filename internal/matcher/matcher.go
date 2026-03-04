// Package matcher determines whether a route satisfies a user's watch criteria.
package matcher

import (
	"strconv"
	"strings"
	"time"

	"github.com/jagduvi1/freeride-watcher/internal/db"
)

// Match returns true if route satisfies all of watch's criteria.
func Match(route db.Route, watch db.Watch) bool {
	// ── City matching (case-insensitive substring) ────────────────────────────
	if watch.Origin != "" {
		if !strings.Contains(
			strings.ToLower(route.Origin),
			strings.ToLower(watch.Origin),
		) {
			return false
		}
	}
	if watch.Destination != "" {
		if !strings.Contains(
			strings.ToLower(route.Destination),
			strings.ToLower(watch.Destination),
		) {
			return false
		}
	}

	dep := route.DepartureAt

	// ── Earliest departure time ───────────────────────────────────────────────
	if watch.EarliestTime != "" {
		et := parseHHMM(watch.EarliestTime)
		depMins := dep.Hour()*60 + dep.Minute()
		if depMins < et {
			return false
		}
	}

	// ── Latest departure time ─────────────────────────────────────────────────
	if watch.LatestTime != "" {
		lt := parseHHMM(watch.LatestTime)
		depMins := dep.Hour()*60 + dep.Minute()
		if depMins > lt {
			return false
		}
	}

	// ── Weekday filter ────────────────────────────────────────────────────────
	if watch.Weekdays != "" {
		// Weekdays stored as comma-separated ISO values: 1=Mon, 7=Sun
		routeWD := isoWeekday(dep.Weekday())
		if !weekdayAllowed(routeWD, watch.Weekdays) {
			return false
		}
	}

	return true
}

// parseHHMM converts "HH:MM" to total minutes since midnight.
func parseHHMM(s string) int {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h*60 + m
}

// isoWeekday converts Go's time.Weekday (0=Sun) to ISO 8601 (1=Mon, 7=Sun).
func isoWeekday(wd time.Weekday) int {
	if wd == time.Sunday {
		return 7
	}
	return int(wd)
}

// weekdayAllowed checks if the given ISO weekday is in the comma-separated list.
func weekdayAllowed(wd int, list string) bool {
	for _, part := range strings.Split(list, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && n == wd {
			return true
		}
	}
	return false
}
