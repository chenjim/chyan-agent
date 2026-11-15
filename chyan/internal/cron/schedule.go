// Schedule parsing: 5-field cron expressions and "@every <duration>" intervals.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// schedule is a parsed job schedule. Either an interval (every > 0), a
// one-shot time (at non-zero, fires once), or a cron expression held as
// per-field bitmasks.
type schedule struct {
	every time.Duration
	at    time.Time

	min, hour       uint64
	dom, month, dow uint64
	domAny, dowAny  bool // field was "*"; both restricted match either (Vixie rule)
}

// ValidateSchedule reports whether expr is a supported schedule (5-field
// cron, "@every <duration>" or "@at <RFC3339>"). The daemon uses it before
// accepting a job from the Web form so a bad expression fails the request,
// not the silent skip in Load.
func ValidateSchedule(expr string) error {
	_, err := parseSchedule(expr)
	return err
}

// parseSchedule parses "* * * * *" (minute hour day month weekday, numbers
// only, weekday 0-6 with 7 = Sunday), "@every 30s" or "@at <RFC3339>"
// (one-shot). Supported field terms: "*", "*/n", "a", "a-b", "a-b/n", comma
// lists of those.
func parseSchedule(expr string) (schedule, error) {
	expr = strings.TrimSpace(expr)
	if rest, ok := strings.CutPrefix(expr, "@every "); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil || d <= 0 {
			return schedule{}, fmt.Errorf("cron: bad interval %q", expr)
		}
		return schedule{every: d}, nil
	}
	if rest, ok := strings.CutPrefix(expr, "@at "); ok {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(rest))
		if err != nil {
			return schedule{}, fmt.Errorf("cron: bad @at time %q", expr)
		}
		return schedule{at: t}, nil
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return schedule{}, fmt.Errorf("cron: want 5 fields, got %d in %q", len(fields), expr)
	}
	var s schedule
	var err error
	if s.min, err = parseField(fields[0], 0, 59); err != nil {
		return schedule{}, err
	}
	if s.hour, err = parseField(fields[1], 0, 23); err != nil {
		return schedule{}, err
	}
	if s.dom, err = parseField(fields[2], 1, 31); err != nil {
		return schedule{}, err
	}
	if s.month, err = parseField(fields[3], 1, 12); err != nil {
		return schedule{}, err
	}
	if s.dow, err = parseField(fields[4], 0, 7); err != nil {
		return schedule{}, err
	}
	if s.dow&(1<<7) != 0 { // 7 is an alias for Sunday (0)
		s.dow = s.dow&^(1<<7) | 1
	}
	s.domAny = strings.HasPrefix(fields[2], "*")
	s.dowAny = strings.HasPrefix(fields[4], "*")
	return s, nil
}

// parseField parses one cron field into a bitmask of allowed values.
func parseField(f string, lo, hi int) (uint64, error) {
	var mask uint64
	for _, tok := range strings.Split(f, ",") {
		step := 1
		if base, sp, ok := strings.Cut(tok, "/"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(sp))
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("cron: bad step in %q", tok)
			}
			step = n
			tok = base
		}
		lo2, hi2 := lo, hi
		if tok != "*" {
			if a, b, ok := strings.Cut(tok, "-"); ok {
				var err error
				if lo2, err = strconv.Atoi(strings.TrimSpace(a)); err != nil {
					return 0, fmt.Errorf("cron: bad range in %q", tok)
				}
				if hi2, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
					return 0, fmt.Errorf("cron: bad range in %q", tok)
				}
			} else {
				v, err := strconv.Atoi(strings.TrimSpace(tok))
				if err != nil {
					return 0, fmt.Errorf("cron: bad value %q", tok)
				}
				lo2, hi2 = v, v
			}
		}
		if lo2 < lo || hi2 > hi || lo2 > hi2 {
			return 0, fmt.Errorf("cron: value out of range in %q", tok)
		}
		for v := lo2; v <= hi2; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, nil
}

// dayMatches reports whether the date matches the day-of-month/weekday
// fields. When both are restricted, either may match (Vixie cron rule).
func (s schedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	if !s.domAny && !s.dowAny {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// next returns the next fire time strictly after after, or the zero time if
// none exists within five years. Intervals fire at after+every, one-shots
// only at their @at time (once), cron at the next matching minute boundary.
func (s schedule) next(after time.Time) time.Time {
	if s.every > 0 {
		return after.Add(s.every)
	}
	if !s.at.IsZero() {
		if after.Before(s.at) {
			return s.at
		}
		return time.Time{} // already fired; one-shot is done
	}
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case s.month&(1<<uint(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
		case s.hour&(1<<uint(t.Hour())) == 0:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Add(time.Hour)
		case s.min&(1<<uint(t.Minute())) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}
