package hours

import (
	"fmt"
	"time"
)

// dayKind discriminates between weekday and weekend windows. Sunday
// and Saturday are weekend; Mon-Fri are weekday. Russian federal
// holidays sit OUTSIDE this dichotomy and are handled by the holiday
// set check before the kind switch ever runs.
type dayKind int

const (
	kindWeekday dayKind = iota
	kindWeekend
)

func (k dayKind) String() string {
	switch k {
	case kindWeekday:
		return "weekday"
	case kindWeekend:
		return "weekend"
	default:
		return "unknown"
	}
}

// kindFor returns the dayKind for the local date. Sunday + Saturday →
// weekend; everything else → weekday.
func kindFor(t time.Time) dayKind {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return kindWeekend
	default:
		return kindWeekday
	}
}

// timeOfDay is a (hour, minute) pair with no date component. Used as
// the per-window start/end. We could store time.Duration since
// midnight, but the explicit struct documents the units at the type
// level and keeps round-tripping to JSON ("HH:MM") trivial.
type timeOfDay struct {
	Hour   int
	Minute int
}

// String renders timeOfDay back as "HH:MM" — convenient for log
// fields and structured tests.
func (t timeOfDay) String() string {
	return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute)
}

// parseTimeOfDay parses an "HH:MM" string into a timeOfDay. Strict on
// the format (must be exactly five characters, two-digit hour + colon
// + two-digit minute) so a typo in tenant config surfaces as an
// explicit error rather than silently parsing as a different time.
//
// Allows 24:00 as the upper-bound terminator for an end window —
// "01:00 to 24:00" is a valid 23-hour stretch. Internally 24:00 is
// stored as Hour=24 Minute=0 and compared correctly by combineDate
// (which uses time.Date with Hour=24 to roll into the next day's
// 00:00 — Go normalises this automatically).
func parseTimeOfDay(s string) (timeOfDay, error) {
	if len(s) != 5 || s[2] != ':' {
		return timeOfDay{}, fmt.Errorf("hours: %q: want HH:MM", s)
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		// Special case: 24:00 fails time.Parse but is a valid
		// half-open end. Detect it before deciding the input was
		// malformed.
		if s == "24:00" {
			return timeOfDay{Hour: 24, Minute: 0}, nil
		}
		return timeOfDay{}, fmt.Errorf("hours: %q: %w", s, err)
	}
	return timeOfDay{Hour: t.Hour(), Minute: t.Minute()}, nil
}

// combineDate splices a timeOfDay onto the local date of base in loc.
// time.Date normalises out-of-range values (e.g. Hour=24 → next day
// 00:00) which is exactly what we want for the half-open end of the
// allowed window.
func combineDate(base time.Time, loc *time.Location, t timeOfDay) time.Time {
	y, mo, d := base.In(loc).Date()
	return time.Date(y, mo, d, t.Hour, t.Minute, 0, 0, loc)
}

// window is a [start, end) interval of clock-time-of-day. The zero
// value is invalid (an "empty" window is a misconfiguration) and the
// constructor declines to build one.
type window struct {
	Start timeOfDay
	End   timeOfDay
}

// contains reports whether the local instant local lies inside the
// half-open [Start, End) window for that local date.
func (w window) contains(local time.Time, loc *time.Location) bool {
	start := combineDate(local, loc, w.Start)
	end := combineDate(local, loc, w.End)
	// Half-open: tStart inclusive, tEnd exclusive.
	if local.Before(start) {
		return false
	}
	if !local.Before(end) {
		return false
	}
	return true
}
