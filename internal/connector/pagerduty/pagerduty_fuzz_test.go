package pagerduty_test

import (
	"encoding/json"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector/pagerduty"
)

// FuzzPagerDutyCredentialsDecode \u2014 Round-24 Task 10.
func FuzzPagerDutyCredentialsDecode(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"api_key":"k"}`))
	f.Add([]byte(`{"api_key":"\u0000"}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Credentials decode panicked on %q: %v", string(data), r)
			}
		}()
		var creds pagerduty.Credentials
		_ = json.Unmarshal(data, &creds)
	})
}
