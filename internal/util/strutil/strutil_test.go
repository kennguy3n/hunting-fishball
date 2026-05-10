package strutil_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/util/strutil"
)

func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{name: "all empty", in: []string{"", "", ""}, want: ""},
		{name: "first wins", in: []string{"a", "b", "c"}, want: "a"},
		{name: "skips leading empty", in: []string{"", "second"}, want: "second"},
		{name: "preserves whitespace", in: []string{" "}, want: " "},
		{name: "no args", in: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := strutil.FirstNonEmpty(tc.in...)
			if got != tc.want {
				t.Fatalf("FirstNonEmpty(%v)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}
