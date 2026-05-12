package retrieval

import (
	"strings"
	"testing"

	ehttp "github.com/kennguy3n/hunting-fishball/internal/errors"
)

func TestValidateRetrieveRequest_ValidPayload(t *testing.T) {
	req := &RetrieveRequest{Query: "hello", TopK: 5, Diversity: 0.5}
	if err := ValidateRetrieveRequest(req); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestValidateRetrieveRequest_MissingQuery(t *testing.T) {
	req := &RetrieveRequest{TopK: 5}
	err := ValidateRetrieveRequest(req)
	if err == nil {
		t.Fatalf("expected error")
	}
	pe, ok := err.(*PayloadError)
	if !ok || len(pe.Fields) == 0 || pe.Fields[0].Field != "query" {
		t.Fatalf("expected field=query error, got %+v", err)
	}
}

func TestValidateRetrieveRequest_OutOfRange(t *testing.T) {
	cases := []struct {
		name string
		req  *RetrieveRequest
		want string
	}{
		{"negative top_k", &RetrieveRequest{Query: "x", TopK: -1}, "top_k"},
		{"huge top_k", &RetrieveRequest{Query: "x", TopK: 99999}, "top_k"},
		{"diversity out of range", &RetrieveRequest{Query: "x", Diversity: 2}, "diversity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRetrieveRequest(tc.req)
			if err == nil {
				t.Fatalf("expected error")
			}
			if pe, ok := err.(*PayloadError); !ok || !fieldsContain(pe, tc.want) {
				t.Fatalf("missing %s in %v", tc.want, err)
			}
		})
	}
}

func TestValidateBatchRequest_EmptyAndOversize(t *testing.T) {
	if err := ValidateBatchRequest(&BatchRequest{}); err == nil {
		t.Fatalf("expected error for empty requests")
	}
	big := make([]RetrieveRequest, MaxBatchSize+1)
	for i := range big {
		big[i] = RetrieveRequest{Query: "q"}
	}
	err := ValidateBatchRequest(&BatchRequest{Requests: big})
	if err == nil {
		t.Fatalf("expected error for oversize batch")
	}
	pe, _ := err.(*PayloadError)
	if !fieldsContain(pe, "requests") {
		t.Fatalf("missing requests field in %+v", err)
	}
}

func TestValidateBatchRequest_PropagatesSubRequestErrors(t *testing.T) {
	req := &BatchRequest{Requests: []RetrieveRequest{
		{Query: ""},
		{Query: "ok"},
		{Query: "ok", TopK: -1},
	}}
	err := ValidateBatchRequest(req)
	if err == nil {
		t.Fatalf("expected error")
	}
	pe, ok := err.(*PayloadError)
	if !ok {
		t.Fatalf("expected *PayloadError")
	}
	var foundQuery, foundTopK bool
	for _, f := range pe.Fields {
		if f.Field == "requests[0].query" {
			foundQuery = true
		}
		if f.Field == "requests[2].top_k" {
			foundTopK = true
		}
	}
	if !foundQuery || !foundTopK {
		t.Fatalf("expected per-request errors propagated: %+v", pe.Fields)
	}
}

func TestPayloadError_AsCatalogError(t *testing.T) {
	pe := &PayloadError{Fields: []PayloadFieldError{{Field: "query", Message: "x"}}}
	ce := pe.AsCatalogError()
	if ce == nil || ce.Code != ehttp.CodeInvalidPayload {
		t.Fatalf("got %+v", ce)
	}
}

func TestBuildPayloadErrorBody_RoundTripsCode(t *testing.T) {
	pe := &PayloadError{Fields: []PayloadFieldError{{Field: "query", Message: "missing"}}}
	body := BuildPayloadErrorBody(pe)
	if body.Code != string(ehttp.CodeInvalidPayload) {
		t.Fatalf("code=%q", body.Code)
	}
	if !strings.Contains(body.Error, "query") {
		t.Fatalf("error=%q", body.Error)
	}
	if len(body.Fields) != 1 {
		t.Fatalf("fields=%+v", body.Fields)
	}
}

func TestBuildPayloadErrorBody_RawErrorFallsBack(t *testing.T) {
	body := BuildPayloadErrorBody(stringError("decode boom"))
	if body.Code != string(ehttp.CodeInvalidRequestBody) {
		t.Fatalf("code=%q", body.Code)
	}
	if body.Error != "decode boom" {
		t.Fatalf("error=%q", body.Error)
	}
}

type stringError string

func (s stringError) Error() string { return string(s) }

func fieldsContain(pe *PayloadError, field string) bool {
	if pe == nil {
		return false
	}
	for _, f := range pe.Fields {
		if strings.HasPrefix(f.Field, field) {
			return true
		}
	}
	return false
}
