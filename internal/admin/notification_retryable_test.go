package admin

// notification_retryable_test.go — regression test for the
// isRetryableResponseCode helper.
//
// 429 was previously treated as a permanent failure (so the
// dispatcher set next_retry_at = nil and the retry worker skipped
// the row), which contradicted Send()'s own logic — Send treats 429
// as retryable inside its bounded retry loop. The two must agree,
// otherwise a webhook that 429s for longer than Send's inner retry
// window is dropped instead of being scheduled for a later retry.

import (
	"net/http"
	"testing"
)

func TestIsRetryableResponseCode(t *testing.T) {
	cases := []struct {
		name string
		code int
		want bool
	}{
		{"transport failure (status 0)", 0, true},
		{"200 OK", http.StatusOK, false},
		{"301 moved permanently", http.StatusMovedPermanently, false},
		{"400 bad request", http.StatusBadRequest, false},
		{"401 unauthorized", http.StatusUnauthorized, false},
		{"403 forbidden", http.StatusForbidden, false},
		{"404 not found", http.StatusNotFound, false},
		{"408 request timeout", http.StatusRequestTimeout, false},
		{"429 too many requests", http.StatusTooManyRequests, true},
		{"499 client closed request", 499, false},
		{"500 internal server error", http.StatusInternalServerError, true},
		{"502 bad gateway", http.StatusBadGateway, true},
		{"503 service unavailable", http.StatusServiceUnavailable, true},
		{"504 gateway timeout", http.StatusGatewayTimeout, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isRetryableResponseCode(tc.code)
			if got != tc.want {
				t.Fatalf("isRetryableResponseCode(%d) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}
