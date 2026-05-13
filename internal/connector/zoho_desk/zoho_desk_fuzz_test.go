package zoho_desk_test

import (
	"encoding/json"
	"testing"

	zohodesk "github.com/kennguy3n/hunting-fishball/internal/connector/zoho_desk"
)

// FuzzZohoDeskCredentialsDecode \u2014 Round-24 Task 10.
func FuzzZohoDeskCredentialsDecode(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"access_token":"tok","org_id":"1"}`))
	f.Add([]byte(`{"access_token":"\u0000"}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Credentials decode panicked on %q: %v", string(data), r)
			}
		}()
		var creds zohodesk.Credentials
		_ = json.Unmarshal(data, &creds)
	})
}
