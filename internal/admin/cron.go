// Minimal in-house cron parser used by the SyncSchedule model.
//
// We deliberately do not pull in github.com/robfig/cron/v3 because
// the only consumer is the source-sync scheduler — the dependency
// surface is not worth a transitive build of the entire robfig
// runtime. The grammar covers the cases the platform actually
// uses:
//
//	@hourly | @daily | @weekly | @monthly | @yearly
//	@every <duration>           (e.g. @every 5m, @every 1h30m)
//	<min> <hour> <dom> <month> <dow>
//	  fields support: *, lists (a,b,c), ranges (a-b), steps (*/N).
//
// Day-of-week uses 0..6 with Sunday=0 (Linux cron convention).
//
// The grammar is intentionally a strict subset; constructs we do
// not support (named days, "L", "?", "W", seven-field with seconds
// or year) parse-error so operators get an obvious error rather
// than silent misbehaviour.
package admin

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronSchedule is a parsed cron expression that can compute the
// next fire time after a given reference time.
type CronSchedule struct {
	expr string

	// every >0 means @every duration mode (other fields unused).
	every time.Duration

	minutes  []int
	hours    []int
	doms     []int // day of month 1..31
	months   []int // 1..12
	dows     []int // day of week 0..6 (Sun=0)
	hasDOM   bool
	hasDOW   bool
	location *time.Location
}

// fieldRange is the inclusive [min,max] for one cron field.
type fieldRange struct{ min, max int }

var (
	rangeMinute = fieldRange{0, 59}
	rangeHour   = fieldRange{0, 23}
	rangeDOM    = fieldRange{1, 31}
	rangeMonth  = fieldRange{1, 12}
	rangeDOW    = fieldRange{0, 6}
)

// ParseCron parses expr and returns a CronSchedule. The schedule
// computes Next() in UTC by default; supply a non-nil location to
// override.
func ParseCron(expr string, loc *time.Location) (*CronSchedule, error) {
	if loc == nil {
		loc = time.UTC
	}
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, errors.New("cron: empty expression")
	}
	cs := &CronSchedule{expr: expr, location: loc}

	if strings.HasPrefix(expr, "@every ") {
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(expr, "@every ")))
		if err != nil {
			return nil, fmt.Errorf("cron: @every: %w", err)
		}
		if d <= 0 {
			return nil, errors.New("cron: @every duration must be > 0")
		}
		cs.every = d
		return cs, nil
	}

	switch expr {
	case "@hourly":
		expr = "0 * * * *"
	case "@daily", "@midnight":
		expr = "0 0 * * *"
	case "@weekly":
		expr = "0 0 * * 0"
	case "@monthly":
		expr = "0 0 1 * *"
	case "@yearly", "@annually":
		expr = "0 0 1 1 *"
	}

	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields, got %d (%q)", len(fields), expr)
	}
	var err error
	if cs.minutes, err = parseField(fields[0], rangeMinute); err != nil {
		return nil, fmt.Errorf("cron: minute: %w", err)
	}
	if cs.hours, err = parseField(fields[1], rangeHour); err != nil {
		return nil, fmt.Errorf("cron: hour: %w", err)
	}
	if cs.doms, err = parseField(fields[2], rangeDOM); err != nil {
		return nil, fmt.Errorf("cron: day-of-month: %w", err)
	}
	cs.hasDOM = fields[2] != "*"
	if cs.months, err = parseField(fields[3], rangeMonth); err != nil {
		return nil, fmt.Errorf("cron: month: %w", err)
	}
	if cs.dows, err = parseField(fields[4], rangeDOW); err != nil {
		return nil, fmt.Errorf("cron: day-of-week: %w", err)
	}
	cs.hasDOW = fields[4] != "*"
	return cs, nil
}

// parseField parses one cron field into a sorted set of integers
// inside fr. Supports comma-lists, ranges, and step values.
func parseField(s string, fr fieldRange) ([]int, error) {
	if s == "" {
		return nil, errors.New("empty field")
	}
	seen := make(map[int]struct{})
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return nil, errors.New("empty list element")
		}
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("bad step %q", part[i+1:])
			}
			step = n
			part = part[:i]
		}
		var lo, hi int
		switch {
		case part == "*":
			lo, hi = fr.min, fr.max
		case strings.Contains(part, "-"):
			j := strings.Index(part, "-")
			a, err := strconv.Atoi(part[:j])
			if err != nil {
				return nil, fmt.Errorf("bad range start %q", part[:j])
			}
			b, err := strconv.Atoi(part[j+1:])
			if err != nil {
				return nil, fmt.Errorf("bad range end %q", part[j+1:])
			}
			lo, hi = a, b
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("bad value %q", part)
			}
			lo, hi = n, n
		}
		if lo < fr.min || hi > fr.max || lo > hi {
			return nil, fmt.Errorf("out of range [%d-%d]: %s", fr.min, fr.max, part)
		}
		for v := lo; v <= hi; v += step {
			seen[v] = struct{}{}
		}
	}
	out := make([]int, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// Next returns the soonest time strictly after `from` that matches
// the schedule. The result is in cs.location.
func (cs *CronSchedule) Next(from time.Time) time.Time {
	if cs == nil {
		return time.Time{}
	}
	if cs.every > 0 {
		return from.Add(cs.every).In(cs.location)
	}
	t := from.In(cs.location).Add(time.Minute - time.Duration(from.Second())*time.Second - time.Duration(from.Nanosecond()))
	t = t.Truncate(time.Minute)
	// Cap iteration at 4 years to avoid infinite loops on impossible
	// expressions ("0 0 31 2 *" — Feb 31st never exists).
	limit := from.AddDate(4, 0, 0)
	for !t.After(limit) {
		if !contains(cs.months, int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, cs.location)
			continue
		}
		if !cs.matchDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, cs.location)
			continue
		}
		if !contains(cs.hours, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, cs.location)
			continue
		}
		if !contains(cs.minutes, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// matchDay implements the Vixie-cron OR semantics: when both DOM
// and DOW are restricted (neither is "*"), the day matches if
// EITHER list contains the candidate; when only one is restricted,
// only that list is consulted.
func (cs *CronSchedule) matchDay(t time.Time) bool {
	domHit := contains(cs.doms, t.Day())
	dowHit := contains(cs.dows, int(t.Weekday()))
	switch {
	case cs.hasDOM && cs.hasDOW:
		return domHit || dowHit
	case cs.hasDOM:
		return domHit
	case cs.hasDOW:
		return dowHit
	default:
		return true
	}
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// String returns the original expression so log lines and the API
// can echo back what the operator submitted.
func (cs *CronSchedule) String() string {
	if cs == nil {
		return ""
	}
	return cs.expr
}
