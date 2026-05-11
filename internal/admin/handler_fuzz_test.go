package admin

// handler_fuzz_test.go — Round-10 Task 14.
//
// Native Go fuzz targets over the three admin request shapes that
// receive JSON directly from operators / external automation. The
// targets run alongside the unit tests via `go test` (seed corpus
// only) and at length via `make fuzz` which now picks up
// ./internal/admin/... in addition to retrieval.
//
// Each Fuzz target follows the same shape:
//   - seed a handful of realistic and adversarial payloads
//   - run json.Unmarshal into the production struct
//   - cross-check Validate() if the type defines one
//   - assert no panic regardless of input

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzABTestConfigDecode exercises ABTestConfig's JSON tags. The
// store's writer path (Upsert) hits Validate after decode, so
// targets must not panic on any byte sequence.
func FuzzABTestConfigDecode(f *testing.F) {
	seeds := []string{
		`{"experiment_name":"a","status":"active","traffic_split_percent":50}`,
		`{"experiment_name":"","status":"","traffic_split_percent":-1}`,
		`{"experiment_name":"x","status":"draft","control_config":{"k":1},"variant_config":{"k":2}}`,
		`{"start_at":"2026-01-01T00:00:00Z","end_at":"2026-02-01T00:00:00Z"}`,
		`{}`,
		`null`,
		`{"traffic_split_percent":99999999999999999}`,
		`{"experiment_name":"\u0000","status":"\xc3\x28"}`,
		`{"control_config":` + strings.Repeat(`{"a":`, 256) + `1` + strings.Repeat("}", 256) + `}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var c ABTestConfig
		// Errors are fine; panics are not.
		_ = json.Unmarshal(body, &c)
		// Validate is called on every Upsert; must be panic-free.
		_ = c.Validate()
	})
}

// FuzzConnectorTemplateDecode covers the JSON the connector-
// template create endpoint accepts. The store rejects empty
// connector types; the fuzzer needs to confirm the Validate
// path stays panic-free regardless.
func FuzzConnectorTemplateDecode(f *testing.F) {
	seeds := []string{
		`{"connector_type":"google-drive","default_config":{"folder_id":"root"}}`,
		`{"connector_type":"","default_config":null,"description":""}`,
		`{"connector_type":"x","default_config":{"deep":{"nested":{"value":1}}}}`,
		`{"connector_type":"x","default_config":{"big":"` + strings.Repeat("a", 4096) + `"}}`,
		`{"connector_type":"\u0000"}`,
		`{}`,
		`null`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var c ConnectorTemplate
		_ = json.Unmarshal(body, &c)
		_ = c.Validate()
	})
}

// FuzzNotificationPreferenceDecode pins the NotificationPreference
// decode path against malformed channel / target / event_type
// values. The handler calls Validate before persisting, so the
// fuzzer asserts both decode and Validate are panic-free.
func FuzzNotificationPreferenceDecode(f *testing.F) {
	seeds := []string{
		`{"event_type":"source.created","channel":"webhook","target":"https://h/a"}`,
		`{"event_type":"","channel":"","target":""}`,
		`{"event_type":"x","channel":"email","target":"user@example.com","enabled":true}`,
		`{"event_type":"\u0000","channel":"\xc3\x28","target":"\u0000"}`,
		`{"target":"` + strings.Repeat("a", 2048) + `"}`,
		`{}`,
		`null`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var p NotificationPreference
		_ = json.Unmarshal(body, &p)
		_ = p.Validate()
	})
}
