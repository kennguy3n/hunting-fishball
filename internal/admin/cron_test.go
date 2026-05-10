package admin

import (
	"testing"
	"time"
)

func TestParseCron_FiveField(t *testing.T) {
	t.Parallel()
	cs, err := ParseCron("*/15 * * * *", time.UTC)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2024, 1, 1, 0, 7, 0, 0, time.UTC)
	next := cs.Next(from)
	if next.Minute() != 15 || next.Hour() != 0 {
		t.Fatalf("next = %v, want :15 of hour 0", next)
	}
}

func TestParseCron_Every(t *testing.T) {
	t.Parallel()
	cs, err := ParseCron("@every 5m", time.UTC)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	next := cs.Next(from)
	if next.Sub(from) != 5*time.Minute {
		t.Fatalf("next = %v, want from+5m", next)
	}
}

func TestParseCron_Aliases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		expr   string
		fromTS string
		want   string
	}{
		{"@hourly", "2024-01-01T00:30:00Z", "2024-01-01T01:00:00Z"},
		{"@daily", "2024-01-01T00:30:00Z", "2024-01-02T00:00:00Z"},
		{"@weekly", "2024-01-02T00:30:00Z", "2024-01-07T00:00:00Z"}, // Jan 7 2024 = Sunday
	} {
		tc := tc
		t.Run(tc.expr, func(t *testing.T) {
			t.Parallel()
			cs, err := ParseCron(tc.expr, time.UTC)
			if err != nil {
				t.Fatalf("parse %s: %v", tc.expr, err)
			}
			from, _ := time.Parse(time.RFC3339, tc.fromTS)
			want, _ := time.Parse(time.RFC3339, tc.want)
			if got := cs.Next(from); !got.Equal(want) {
				t.Fatalf("Next(%s, %s) = %s, want %s", tc.expr, tc.fromTS, got, want)
			}
		})
	}
}

func TestParseCron_Errors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"not a cron",
		"60 * * * *",      // minute out of range
		"* * 32 * *",      // dom out of range
		"* * * * 7",       // dow out of range (only 0-6)
		"@every banana",   // bad duration
		"@every -5s",      // wrong sign
	}
	for _, expr := range cases {
		expr := expr
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseCron(expr, time.UTC); err == nil {
				t.Fatalf("expected error for %q", expr)
			}
		})
	}
}

func TestParseCron_RangeAndStep(t *testing.T) {
	t.Parallel()
	cs, err := ParseCron("0 9-17/2 * * 1-5", time.UTC)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Monday 2024-01-01 08:00 UTC -> next is 09:00
	from := time.Date(2024, 1, 1, 8, 0, 0, 0, time.UTC)
	want := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
	if got := cs.Next(from); !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}
