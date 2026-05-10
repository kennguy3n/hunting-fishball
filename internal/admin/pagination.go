// Package admin — pagination.go centralises cursor-pagination
// helpers used by the source / DLQ list handlers.
//
// Round-5 Task 3: every admin list endpoint now accepts a
// `cursor` (opaque string the previous response emitted) plus a
// `limit` (default 50, capped at 200). The response envelope adds
// `next_cursor`; an empty value signals the last page. The audit
// handler keeps emitting `next_page_token` as well so existing
// clients don't break — see internal/audit/handler.go for the
// alias logic.
package admin

import (
	"errors"
	"strconv"
)

// MaxPageLimit is the absolute ceiling all admin list endpoints
// enforce. Larger values are clamped silently.
const MaxPageLimit = 200

// DefaultPageLimit applies when the client omits `limit`.
const DefaultPageLimit = 50

// parsePageLimit converts a raw `limit` query string to an int
// suitable for ListFilter.PageSize. Empty input returns 0 so the
// repository layer falls back to its own default; non-numeric
// input is rejected with a stable error string the handler
// surfaces as 400.
func parsePageLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.New("limit must be a non-negative integer")
	}
	if n > MaxPageLimit {
		n = MaxPageLimit
	}
	return n, nil
}

// firstNonEmpty returns the first non-empty string in args. It is
// used by handlers that accept a new query-parameter alias
// (`cursor`) alongside an existing one (`page_token`) without
// silently preferring one over the other.
func firstNonEmpty(args ...string) string {
	for _, a := range args {
		if a != "" {
			return a
		}
	}
	return ""
}
