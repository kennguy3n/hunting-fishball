// Package errors holds the structured error catalogue and the Gin
// middleware that maps Go errors to JSON responses with stable
// machine-readable codes.
//
// The contract:
//
//   - Every operator-visible error has an entry in DefaultCatalog
//     keyed by a string Code (`ERR_TENANT_NOT_FOUND`, etc.). Each
//     entry carries an HTTP status, a human message template, and
//     an optional retry hint.
//   - Handlers create errors via Wrap(code, cause) or NewError(code,
//     msg). Both produce *Error which the middleware unwraps.
//   - The middleware writes
//     { "error": { "code": "...", "message": "...", "retry": "..." } }
//     and the appropriate status. Callers can attach extra
//     `details` via Error.With(key, value).
//
// We deliberately keep Code values as plain strings (rather than
// an iota enum) so they survive across binary versions and so
// log/alert pipelines can match on them without consulting the
// Go source.
package errors

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is the stable machine-readable identifier for an error.
type Code string

// Catalogue codes. Add entries here when introducing a new
// surface; keep the prefix `ERR_` so log scanners can match on it.
const (
	CodeUnknown            Code = "ERR_UNKNOWN"
	CodeBadRequest         Code = "ERR_BAD_REQUEST"
	CodeUnauthenticated    Code = "ERR_UNAUTHENTICATED"
	CodeForbidden          Code = "ERR_FORBIDDEN"
	CodeNotFound           Code = "ERR_NOT_FOUND"
	CodeConflict           Code = "ERR_CONFLICT"
	CodeRateLimited        Code = "ERR_RATE_LIMITED"
	CodeInternal           Code = "ERR_INTERNAL"
	CodeBackendDegraded    Code = "ERR_BACKEND_DEGRADED"
	CodeTenantNotFound     Code = "ERR_TENANT_NOT_FOUND"
	CodeSourceNotFound     Code = "ERR_SOURCE_NOT_FOUND"
	CodePolicyConflict     Code = "ERR_POLICY_CONFLICT"
	CodeRetentionViolation Code = "ERR_RETENTION_VIOLATION"
	CodeServiceUnavailable Code = "ERR_SERVICE_UNAVAILABLE"

	// Round-9 Task 12: admin-surface error codes. Each maps to
	// a previously raw c.JSON(500, ...) site in the Round-6/7/8
	// admin handlers, giving operators stable codes to alert
	// on (rather than the generic "internal server error"
	// envelope).
	CodeCacheWarmFailed      Code = "ERR_CACHE_WARM_FAILED"
	CodeBudgetInvalid        Code = "ERR_BUDGET_INVALID"
	CodeBudgetLookupFailed   Code = "ERR_BUDGET_LOOKUP_FAILED"
	CodeCacheConfigFailed    Code = "ERR_CACHE_CONFIG_FAILED"
	CodeSyncHistoryFailed    Code = "ERR_SYNC_HISTORY_FAILED"
	CodePinnedResultsFailed  Code = "ERR_PINNED_RESULTS_FAILED"
	CodePipelineHealthFailed Code = "ERR_PIPELINE_HEALTH_FAILED"

	// Round-11 Task 13: admin handler audit additions. The common
	// gin.H{"error": "missing tenant context"} pattern landed in
	// dozens of handlers; the catalog now owns the canonical code
	// so downstream alerting can match on it uniformly.
	CodeMissingTenant         Code = "ERR_MISSING_TENANT"
	CodeInvalidRequestBody    Code = "ERR_INVALID_REQUEST_BODY"
	CodeChunkQualityFailed    Code = "ERR_CHUNK_QUALITY_FAILED"
	CodePolicyHistoryFailed   Code = "ERR_POLICY_HISTORY_FAILED"
	CodeABTestResultsFailed   Code = "ERR_ABTEST_RESULTS_FAILED"
	CodeDashboardListFailed   Code = "ERR_DASHBOARD_LIST_FAILED"
	CodeSimulatorPersistFail  Code = "ERR_SIMULATOR_PERSIST_FAILED"
	CodeSimulatorEvalFail     Code = "ERR_SIMULATOR_EVAL_FAILED"
	CodeSimulatorDraftMissing Code = "ERR_SIMULATOR_DRAFT_MISSING"
	CodeSyncStreamFailed      Code = "ERR_SYNC_STREAM_FAILED"

	// CodeInvalidPayload — Round-14 Task 5. Distinguishes a
	// schema-validation failure (a required field missing, an
	// out-of-range integer, a wrong-type value) from the broader
	// ERR_INVALID_REQUEST_BODY (malformed JSON envelope).
	// Callers can branch on the typed code to decide whether
	// retrying with a corrected payload is worth attempting.
	CodeInvalidPayload Code = "ERR_INVALID_PAYLOAD"
)

// Entry is one entry in the catalogue.
type Entry struct {
	HTTPStatus int
	Message    string
	Retry      string
}

// DefaultCatalog is the canonical lookup table. New codes must
// register here and keep stable HTTP statuses across versions.
var DefaultCatalog = map[Code]Entry{
	CodeUnknown:            {http.StatusInternalServerError, "unexpected error", ""},
	CodeBadRequest:         {http.StatusBadRequest, "bad request", ""},
	CodeUnauthenticated:    {http.StatusUnauthorized, "authentication required", ""},
	CodeForbidden:          {http.StatusForbidden, "forbidden", ""},
	CodeNotFound:           {http.StatusNotFound, "resource not found", ""},
	CodeConflict:           {http.StatusConflict, "resource conflict", ""},
	CodeRateLimited:        {http.StatusTooManyRequests, "rate limit exceeded", "back off and retry after the Retry-After header"},
	CodeInternal:           {http.StatusInternalServerError, "internal server error", "retry after a short delay"},
	CodeBackendDegraded:    {http.StatusServiceUnavailable, "downstream backend is degraded", "results may be partial; check policy.degraded flag"},
	CodeTenantNotFound:     {http.StatusNotFound, "tenant not found", ""},
	CodeSourceNotFound:     {http.StatusNotFound, "source not found", ""},
	CodePolicyConflict:     {http.StatusConflict, "policy draft conflicts with live state", "rebase your draft against the latest snapshot"},
	CodeRetentionViolation: {http.StatusUnprocessableEntity, "retention policy violated", "shorten the requested window or relax the policy"},
	CodeServiceUnavailable: {http.StatusServiceUnavailable, "service unavailable", "retry after a short delay"},

	// Round-9 Task 12: admin error mappings.
	CodeCacheWarmFailed:      {http.StatusInternalServerError, "cache warm failed", "retry the warm job; if it persists, check Redis health"},
	CodeBudgetInvalid:        {http.StatusBadRequest, "latency budget input invalid", "ensure max_latency_ms is a positive integer"},
	CodeBudgetLookupFailed:   {http.StatusInternalServerError, "latency budget lookup failed", "check Postgres connectivity"},
	CodeCacheConfigFailed:    {http.StatusInternalServerError, "cache config update failed", "check Postgres connectivity"},
	CodeSyncHistoryFailed:    {http.StatusInternalServerError, "sync history query failed", "check Postgres connectivity"},
	CodePinnedResultsFailed:  {http.StatusInternalServerError, "pinned results update failed", "check Postgres connectivity"},
	CodePipelineHealthFailed: {http.StatusInternalServerError, "pipeline health snapshot failed", "check ingest service health"},

	// Round-11 Task 13: admin handler audit additions.
	CodeMissingTenant:         {http.StatusUnauthorized, "missing tenant context", ""},
	CodeInvalidRequestBody:    {http.StatusBadRequest, "invalid request body", "verify the JSON envelope and required fields"},
	CodeChunkQualityFailed:    {http.StatusInternalServerError, "chunk quality query failed", "check Postgres connectivity"},
	CodePolicyHistoryFailed:   {http.StatusInternalServerError, "policy history query failed", "check Postgres connectivity"},
	CodeABTestResultsFailed:   {http.StatusInternalServerError, "ab-test results aggregation failed", "check Postgres connectivity"},
	CodeDashboardListFailed:   {http.StatusInternalServerError, "dashboard list failed", "check Postgres connectivity"},
	CodeSimulatorPersistFail:  {http.StatusInternalServerError, "simulator draft persist failed", "check Postgres connectivity"},
	CodeSimulatorEvalFail:     {http.StatusInternalServerError, "simulator evaluation failed", "retry; if it persists, inspect the draft snapshot"},
	CodeSimulatorDraftMissing: {http.StatusNotFound, "simulator draft not found", ""},
	CodeSyncStreamFailed:      {http.StatusInternalServerError, "sync progress stream failed", "retry; if persistent, check ingest service health"},

	// Round-14 Task 5.
	CodeInvalidPayload: {http.StatusBadRequest, "request payload failed schema validation", "fix the named field(s) and retry"},
}

// Error is the typed application error. Handlers create it via
// New / Wrap and the middleware unwraps it.
type Error struct {
	Code    Code
	Message string
	Cause   error
	Details map[string]any
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %s", e.Code, e.Message, e.Cause.Error())
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap supports errors.Is / errors.As against the wrapped Cause.
func (e *Error) Unwrap() error { return e.Cause }

// With attaches a structured detail to the error and returns the
// same pointer so callers can chain.
func (e *Error) With(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any, 1)
	}
	e.Details[key] = value
	return e
}

// New constructs an *Error with a concrete message.
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// Wrap constructs an *Error wrapping cause; the message is
// derived from the catalogue entry.
func Wrap(code Code, cause error) *Error {
	msg := ""
	if e, ok := DefaultCatalog[code]; ok {
		msg = e.Message
	}
	return &Error{Code: code, Message: msg, Cause: cause}
}

// Resolve returns the catalogue entry for e.Code (falling back to
// CodeUnknown when the code is not registered).
func Resolve(e *Error) Entry {
	if e == nil {
		return DefaultCatalog[CodeUnknown]
	}
	if entry, ok := DefaultCatalog[e.Code]; ok {
		return entry
	}
	return DefaultCatalog[CodeUnknown]
}

// As reports whether err is (or wraps) an *Error and stores the
// pointer in target. Wraps the stdlib errors.As for ergonomic use.
func As(err error, target **Error) bool {
	return errors.As(err, target)
}
