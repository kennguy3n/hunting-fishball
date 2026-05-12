// payload_validator.go — Round-14 Task 5.
//
// Strict request-payload validation for the core POST endpoints:
//   /v1/retrieve
//   /v1/retrieve/batch
//   /v1/retrieve/stream
//
// Round-13 Task 11 added a payload SIZE limiter middleware. This
// file adds payload SCHEMA validation: every required field is
// checked, every bounded field is range-checked, and oversized
// nested slices are rejected.
//
// On rejection we return a structured error from
// internal/errors/catalog.go (CodeInvalidPayload =
// "ERR_INVALID_PAYLOAD") with a `fields` array enumerating the
// offending paths. Callers can branch on the typed code to
// distinguish a schema failure from a JSON-envelope decode failure
// (CodeInvalidRequestBody = "ERR_INVALID_REQUEST_BODY").
package retrieval

import (
	"fmt"
	"strings"

	ehttp "github.com/kennguy3n/hunting-fishball/internal/errors"
)

// MaxRetrieveTopK caps the per-request top-K. The cap is enforced
// independently of the handler's MaxTopK config so the validator
// runs without any handler context. The handler still re-clamps
// to its own MaxTopK after validation.
const MaxRetrieveTopK = 1000

// MaxRetrieveStringLen bounds individual string fields (query,
// privacy_mode, skill_id, device_tier) to keep an attacker-
// supplied 100 MiB JSON value from being persisted into analytics.
const MaxRetrieveStringLen = 4096

// MaxRetrieveSliceLen caps the number of entries in the slice
// fields (sources, channels, document_ids).
const MaxRetrieveSliceLen = 256

// PayloadFieldError is one offending field.
type PayloadFieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// PayloadError aggregates one or more field-level rejections.
type PayloadError struct {
	Fields []PayloadFieldError
}

// Error implements error.
func (p *PayloadError) Error() string {
	if p == nil || len(p.Fields) == 0 {
		return "invalid payload"
	}
	parts := make([]string, 0, len(p.Fields))
	for _, f := range p.Fields {
		parts = append(parts, fmt.Sprintf("%s: %s", f.Field, f.Message))
	}
	return strings.Join(parts, "; ")
}

// AsCatalogError adapts the PayloadError into the project-wide
// catalog error. The catalog entry (CodeInvalidPayload) carries
// the HTTP status (400) so callers can route through the standard
// error middleware.
func (p *PayloadError) AsCatalogError() *ehttp.Error {
	return ehttp.New(ehttp.CodeInvalidPayload, p.Error())
}

// ValidateRetrieveRequest checks every constraint on req.
// Returns nil when valid, *PayloadError otherwise.
func ValidateRetrieveRequest(req *RetrieveRequest) error {
	if req == nil {
		return &PayloadError{Fields: []PayloadFieldError{{Field: "", Message: "request body is required"}}}
	}
	var errs []PayloadFieldError
	q := strings.TrimSpace(req.Query)
	if q == "" {
		errs = append(errs, PayloadFieldError{Field: "query", Message: "must not be empty"})
	}
	if len(req.Query) > MaxRetrieveStringLen {
		errs = append(errs, PayloadFieldError{Field: "query", Message: fmt.Sprintf("exceeds %d chars", MaxRetrieveStringLen)})
	}
	if req.TopK < 0 {
		errs = append(errs, PayloadFieldError{Field: "top_k", Message: "must be >= 0"})
	}
	if req.TopK > MaxRetrieveTopK {
		errs = append(errs, PayloadFieldError{Field: "top_k", Message: fmt.Sprintf("must be <= %d", MaxRetrieveTopK)})
	}
	if req.Diversity < 0 || req.Diversity > 1 {
		errs = append(errs, PayloadFieldError{Field: "diversity", Message: "must be in [0, 1]"})
	}
	for i, s := range req.Sources {
		if len(s) > MaxRetrieveStringLen {
			errs = append(errs, PayloadFieldError{Field: fmt.Sprintf("sources[%d]", i), Message: "too long"})
			break
		}
	}
	if len(req.Sources) > MaxRetrieveSliceLen {
		errs = append(errs, PayloadFieldError{Field: "sources", Message: fmt.Sprintf("too many entries (>%d)", MaxRetrieveSliceLen)})
	}
	if len(req.Channels) > MaxRetrieveSliceLen {
		errs = append(errs, PayloadFieldError{Field: "channels", Message: fmt.Sprintf("too many entries (>%d)", MaxRetrieveSliceLen)})
	}
	if len(req.Documents) > MaxRetrieveSliceLen {
		errs = append(errs, PayloadFieldError{Field: "document_ids", Message: fmt.Sprintf("too many entries (>%d)", MaxRetrieveSliceLen)})
	}
	if len(req.PrivacyMode) > MaxRetrieveStringLen {
		errs = append(errs, PayloadFieldError{Field: "privacy_mode", Message: "too long"})
	}
	if len(req.SkillID) > MaxRetrieveStringLen {
		errs = append(errs, PayloadFieldError{Field: "skill_id", Message: "too long"})
	}
	if len(req.DeviceTier) > MaxRetrieveStringLen {
		errs = append(errs, PayloadFieldError{Field: "device_tier", Message: "too long"})
	}
	if len(errs) == 0 {
		return nil
	}
	return &PayloadError{Fields: errs}
}

// ValidateBatchRequest enforces the BatchRequest envelope and
// recurses into every sub-request.
func ValidateBatchRequest(req *BatchRequest) error {
	if req == nil {
		return &PayloadError{Fields: []PayloadFieldError{{Field: "", Message: "request body is required"}}}
	}
	var errs []PayloadFieldError
	if len(req.Requests) == 0 {
		errs = append(errs, PayloadFieldError{Field: "requests", Message: "must not be empty"})
	}
	if len(req.Requests) > MaxBatchSize {
		errs = append(errs, PayloadFieldError{Field: "requests", Message: fmt.Sprintf("too many entries (>%d)", MaxBatchSize)})
	}
	if req.MaxParallel < 0 {
		errs = append(errs, PayloadFieldError{Field: "max_parallel", Message: "must be >= 0"})
	}
	for i := range req.Requests {
		if err := ValidateRetrieveRequest(&req.Requests[i]); err != nil {
			if pe, ok := err.(*PayloadError); ok {
				for _, f := range pe.Fields {
					errs = append(errs, PayloadFieldError{
						Field:   fmt.Sprintf("requests[%d].%s", i, f.Field),
						Message: f.Message,
					})
				}
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &PayloadError{Fields: errs}
}

// PayloadErrorBody is the JSON envelope returned to the client on
// schema failure. The `error` key is preserved for backward
// compatibility with the pre-Round-14 response, while the new
// `code` and `fields` keys give structured detail.
type PayloadErrorBody struct {
	Code   string              `json:"code"`
	Error  string              `json:"error"`
	Fields []PayloadFieldError `json:"fields,omitempty"`
}

// BuildPayloadErrorBody composes the response body for a
// *PayloadError. err may also be a plain error (e.g. from a
// ShouldBindJSON failure) — the body falls back to a generic
// message in that case.
func BuildPayloadErrorBody(err error) PayloadErrorBody {
	if pe, ok := err.(*PayloadError); ok {
		return PayloadErrorBody{
			Code:   string(ehttp.CodeInvalidPayload),
			Error:  pe.Error(),
			Fields: pe.Fields,
		}
	}
	msg := "invalid request body"
	if err != nil {
		msg = err.Error()
	}
	return PayloadErrorBody{
		Code:  string(ehttp.CodeInvalidRequestBody),
		Error: msg,
	}
}
