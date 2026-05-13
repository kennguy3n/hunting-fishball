package quip_test

import (
	"encoding/json"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector/quip"
)

// FuzzQuipCredentialsDecode \u2014 Round-24 Task 10.
//
// Fuzzes the Credentials JSON decoder so an attacker-controlled
// blob cannot panic the connector before Validate runs.
func FuzzQuipCredentialsDecode(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"access_token":"x"}`))
	f.Add([]byte(`{"access_token":"\u0000"}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Credentials decode panicked on %q: %v", string(data), r)
			}
		}()
		var creds quip.Credentials
		_ = json.Unmarshal(data, &creds)
	})
}
