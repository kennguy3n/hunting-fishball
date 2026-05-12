package admin

import (
	"encoding/json"
	"testing"
)

// FuzzHealthSummaryRequest — Round-14 Task 11.
//
// The summary endpoint does not currently accept a request body
// (it's a GET), but an admin batch wrapper marshals a request
// shape that includes optional probe filters. We fuzz the
// HealthSummaryResponse JSON round-trip since that is the wire
// shape the handler returns and downstream tooling deserialises.
// The fuzz asserts the unmarshal never panics.
func FuzzHealthSummaryRequest(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"status":"ok","components":[]}`))
	f.Add([]byte(`{"status":"degraded","components":[{"name":"db","status":"down"}]}`))
	f.Add([]byte(`{"components":[null]}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("HealthSummaryResponse decode panicked on %q: %v", string(data), r)
			}
		}()
		var resp HealthSummaryResponse
		_ = json.Unmarshal(data, &resp)
	})
}
