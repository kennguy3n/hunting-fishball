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

	"github.com/kennguy3n/hunting-fishball/internal/util/strutil"
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

// firstNonEmpty is a thin alias preserving the package-private
// call sites (cursor / page_token aliasing). The implementation
// lives in internal/util/strutil so the audit handler can share
// it without an admin → audit dependency cycle. Addresses
// FLAG_pr-review-job-b10ff0a8305841f98c7f1ed361d5ee8b_0004.
func firstNonEmpty(args ...string) string {
	return strutil.FirstNonEmpty(args...)
}
