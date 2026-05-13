package pipeline_test

// ingest_event_content_type_test.go — Round-24 Task 15.
//
// Asserts the new ContentType field round-trips through JSON
// encode/decode so producers and consumers can rely on the
// multimodal routing hint surviving the Kafka envelope.

import (
	"encoding/json"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func TestIngestEvent_ContentTypeRoundTrips(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"text", "text"},
		{"image", "image"},
		{"audio", "audio"},
		{"video_thumbnail", "video"},
		{"empty", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			evt := pipeline.IngestEvent{
				Kind:        pipeline.EventDocumentChanged,
				TenantID:    "t",
				SourceID:    "s",
				DocumentID:  "d",
				ContentType: tc.in,
			}
			b, err := json.Marshal(evt)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got pipeline.IngestEvent
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.ContentType != tc.in {
				t.Fatalf("ContentType round-trip: got %q want %q", got.ContentType, tc.in)
			}
		})
	}
}

func TestIngestEvent_ContentTypeOmitemptyOnEmpty(t *testing.T) {
	t.Parallel()
	evt := pipeline.IngestEvent{Kind: pipeline.EventDocumentChanged, TenantID: "t", SourceID: "s", DocumentID: "d"}
	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(b); contains(got, "content_type") {
		t.Fatalf("ContentType must omitempty on empty, got %s", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
