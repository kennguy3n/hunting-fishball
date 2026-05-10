// Package strutil holds tiny string helpers shared across the
// admin and audit handlers. The package exists so the same
// "accept new query alias alongside the legacy one" plumbing can
// live in exactly one spot — addresses
// FLAG_pr-review-job-b10ff0a8305841f98c7f1ed361d5ee8b_0004 which
// flagged the duplicated firstNonEmpty implementations.
//
// Keep this package narrow: only general-purpose string helpers
// belong here. Domain logic stays with its package.
package strutil

// FirstNonEmpty returns the first non-empty string in args, or ""
// if all of them are empty. Whitespace is NOT trimmed: callers
// that need trim semantics should TrimSpace before calling.
//
// Used by handlers that accept a new query-parameter alias
// (`cursor`) alongside an existing one (`page_token`) without
// silently preferring one over the other.
func FirstNonEmpty(args ...string) string {
	for _, a := range args {
		if a != "" {
			return a
		}
	}
	return ""
}
