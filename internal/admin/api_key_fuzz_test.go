package admin

import (
	"encoding/json"
	"testing"
)

// FuzzAPIKeyRowDecode — Round-14 Task 11.
//
// Fuzzes APIKeyRow JSON unmarshal. The handler accepts admin
// payloads that decode into APIKeyRow; we want to ensure the
// decoder cannot be tricked into a panic on adversarial input.
// The body of the fuzz is therefore intentionally narrow: feed
// bytes to json.Unmarshal and ignore the resulting error. The
// recover() guard catches any panic that escapes the decoder.
func FuzzAPIKeyRowDecode(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"id":"x"}`))
	f.Add([]byte(`{"id":"x","tenant_id":"y","status":"active"}`))
	f.Add([]byte(`{"id":"x","grace_until":"2020-01-01T00:00:00Z"}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"id":"\u0000"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("APIKeyRow decode panicked on %q: %v", string(data), r)
			}
		}()
		var row APIKeyRow
		_ = json.Unmarshal(data, &row)
	})
}
