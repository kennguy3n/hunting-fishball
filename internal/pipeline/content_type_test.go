package pipeline

// content_type_test.go — Round-19 Task 22.

import "testing"

func TestContentTypeFromMIME(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mime string
		want DocumentContentType
	}{
		{"", ContentTypeText},
		{"text/plain", ContentTypeText},
		{"application/pdf", ContentTypeText},
		{"image/png", ContentTypeImage},
		{"image/jpeg", ContentTypeImage},
		{"audio/mpeg", ContentTypeAudio},
		{"audio/wav", ContentTypeAudio},
		{"video/mp4", ContentTypeVideo},
		{"video/quicktime", ContentTypeVideo},
		{"application/json", ContentTypeText},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.mime, func(t *testing.T) {
			t.Parallel()
			got := ContentTypeFromMIME(tc.mime)
			if got != tc.want {
				t.Fatalf("ContentTypeFromMIME(%q) = %q, want %q", tc.mime, got, tc.want)
			}
		})
	}
}
