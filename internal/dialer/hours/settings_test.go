package hours

// settings_test.go uses the package-private parsePolicy +
// mergeWithDefault helpers and therefore lives INSIDE the hours
// package (no _test suffix). Other test files use the _test variant
// because they only exercise the public Checker surface.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestParsePolicy_FullValid — every JSON field present, every value
// parses cleanly. Locks the happy-path schema.
func TestParsePolicy_FullValid(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"weekday": {"start": "08:00", "end": "22:00"},
		"weekend": {"start": "11:00", "end": "17:00"},
		"exceptions": [
			{"date": "2026-01-09", "open": false, "reason": "Plant closed"},
			{"date": "2026-12-31", "open": true, "start": "09:00", "end": "16:00", "reason": "NYE early close"}
		]
	}`)

	p, err := parsePolicy(raw)
	require.NoError(t, err)
	require.Equal(t, timeOfDay{Hour: 8, Minute: 0}, p.Weekday.Start)
	require.Equal(t, timeOfDay{Hour: 22, Minute: 0}, p.Weekday.End)
	require.Equal(t, timeOfDay{Hour: 11, Minute: 0}, p.Weekend.Start)
	require.Equal(t, timeOfDay{Hour: 17, Minute: 0}, p.Weekend.End)
	require.Len(t, p.Exceptions, 2)

	// Exception 0 — closed all day.
	require.False(t, p.Exceptions[0].Open)
	require.Equal(t, "Plant closed", p.Exceptions[0].Reason)
	require.Equal(t, 2026, p.Exceptions[0].Date.Year())
	require.Equal(t, time.January, p.Exceptions[0].Date.Month())
	require.Equal(t, 9, p.Exceptions[0].Date.Day())

	// Exception 1 — open with custom window.
	require.True(t, p.Exceptions[1].Open)
	require.Equal(t, timeOfDay{Hour: 9, Minute: 0}, p.Exceptions[1].Start)
	require.Equal(t, timeOfDay{Hour: 16, Minute: 0}, p.Exceptions[1].End)
}

// TestParsePolicy_MissingFieldsAreOK — partial config (e.g. only
// weekday) parses cleanly. mergeWithDefault is responsible for
// filling the gaps; parsePolicy itself just leaves the fields zero.
func TestParsePolicy_MissingFieldsAreOK(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"weekday": {"start": "08:00", "end": "22:00"}}`)

	p, err := parsePolicy(raw)
	require.NoError(t, err)
	require.Equal(t, timeOfDay{Hour: 8, Minute: 0}, p.Weekday.Start)
	require.Equal(t, window{}, p.Weekend, "weekend left zero when not in JSON")
	require.Empty(t, p.Exceptions)
}

// TestParsePolicy_EmptyInputZeroPolicy — empty bytes parse to the
// zero policy without an error. Callers normally use ok=false from
// SettingsLookup to skip the parse, but tolerate empty input as
// "no override".
func TestParsePolicy_EmptyInputZeroPolicy(t *testing.T) {
	t.Parallel()
	p, err := parsePolicy(nil)
	require.NoError(t, err)
	require.Equal(t, WorkingHoursPolicy{}, p)

	p, err = parsePolicy([]byte{})
	require.NoError(t, err)
	require.Equal(t, WorkingHoursPolicy{}, p)
}

// TestParsePolicy_InvalidJSON — malformed JSON surfaces an error.
func TestParsePolicy_InvalidJSON(t *testing.T) {
	t.Parallel()
	_, err := parsePolicy([]byte(`{"weekday": {`))
	require.Error(t, err)
}

// TestParsePolicy_InvalidTimeFormat — "9:00" instead of "09:00" is
// rejected so a tenant typo doesn't silently parse to a different
// time.
func TestParsePolicy_InvalidTimeFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{"single-digit hour", `{"weekday": {"start": "9:00", "end": "21:00"}}`},
		{"missing minute", `{"weekday": {"start": "09", "end": "21:00"}}`},
		{"weekend bad", `{"weekend": {"start": "10:00", "end": "18-00"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parsePolicy([]byte(tc.raw))
			require.Error(t, err)
		})
	}
}

// TestParsePolicy_InvalidExceptionDate — a malformed YYYY-MM-DD is
// rejected (vs silently treated as "today").
func TestParsePolicy_InvalidExceptionDate(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"exceptions":[{"date":"2026-13-99","open":false}]}`)
	_, err := parsePolicy(raw)
	require.Error(t, err)
}

// TestParsePolicy_24HourEnd — "24:00" is allowed as the half-open
// upper-bound terminator (so a tenant can configure a 21:00 → 24:00
// late-evening window).
func TestParsePolicy_24HourEnd(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"weekday":{"start":"21:00","end":"24:00"}}`)
	p, err := parsePolicy(raw)
	require.NoError(t, err)
	require.Equal(t, timeOfDay{Hour: 24, Minute: 0}, p.Weekday.End)
}

// TestMergeWithDefault — zero windows in the parsed policy are
// filled with the platform default; non-zero windows are left
// untouched.
func TestMergeWithDefault(t *testing.T) {
	t.Parallel()

	def := WorkingHoursPolicy{
		Weekday: window{Start: timeOfDay{Hour: 9, Minute: 0}, End: timeOfDay{Hour: 21, Minute: 0}},
		Weekend: window{Start: timeOfDay{Hour: 10, Minute: 0}, End: timeOfDay{Hour: 18, Minute: 0}},
	}

	// Override only weekday.
	override := WorkingHoursPolicy{
		Weekday: window{Start: timeOfDay{Hour: 8, Minute: 0}, End: timeOfDay{Hour: 22, Minute: 0}},
	}
	merged := mergeWithDefault(override, def)
	require.Equal(t, override.Weekday, merged.Weekday, "explicit weekday survives merge")
	require.Equal(t, def.Weekend, merged.Weekend, "missing weekend filled by default")

	// Both zero → both filled.
	merged = mergeWithDefault(WorkingHoursPolicy{}, def)
	require.Equal(t, def.Weekday, merged.Weekday)
	require.Equal(t, def.Weekend, merged.Weekend)
}

// TestFindException_MatchesByLocalDate — findException matches on
// the local calendar date, not the raw time.Time. So an exception
// stored as "2026-01-09 00:00 UTC" matches an instant
// "2026-01-09 12:00 LOCAL" in any RU zone.
func TestFindException_MatchesByLocalDate(t *testing.T) {
	t.Parallel()
	moscow, err := time.LoadLocation("Europe/Moscow")
	require.NoError(t, err)

	p := WorkingHoursPolicy{
		Exceptions: []Exception{
			{Date: time.Date(2026, time.January, 9, 0, 0, 0, 0, time.UTC), Open: false},
		},
	}
	// 2026-01-09 12:00 Europe/Moscow.
	t1 := time.Date(2026, time.January, 9, 12, 0, 0, 0, moscow)
	ex, ok := findException(p, t1, moscow)
	require.True(t, ok)
	require.False(t, ex.Open)

	// Different date — no match.
	t2 := time.Date(2026, time.January, 10, 12, 0, 0, 0, moscow)
	_, ok = findException(p, t2, moscow)
	require.False(t, ok)
}

// TestFindException_EmptyPolicy — no exceptions → no match.
func TestFindException_EmptyPolicy(t *testing.T) {
	t.Parallel()
	moscow, err := time.LoadLocation("Europe/Moscow")
	require.NoError(t, err)

	_, ok := findException(WorkingHoursPolicy{}, time.Date(2026, time.January, 1, 12, 0, 0, 0, moscow), moscow)
	require.False(t, ok)
}

// TestParseTimeOfDay_Valid — sanity: representative valid inputs.
func TestParseTimeOfDay_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want timeOfDay
	}{
		{"00:00", timeOfDay{0, 0}},
		{"09:00", timeOfDay{9, 0}},
		{"09:30", timeOfDay{9, 30}},
		{"23:59", timeOfDay{23, 59}},
		{"24:00", timeOfDay{24, 0}},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseTimeOfDay(c.in)
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}

// TestParseTimeOfDay_Invalid — representative malformed inputs are
// rejected. timeOfDay's String renders back to "HH:MM".
func TestParseTimeOfDay_Invalid(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",       // empty
		"9:00",   // single-digit hour
		"24:01",  // beyond half-open terminator
		"25:00",  // hour out of range
		"12:60",  // minute out of range
		"AB:CD",  // not numeric
		"12-34",  // wrong separator
		"123:00", // too long
	}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := parseTimeOfDay(in)
			require.Error(t, err, "input %q should fail to parse", in)
		})
	}
}

// TestTimeOfDayString — round-trip render.
func TestTimeOfDayString(t *testing.T) {
	t.Parallel()
	require.Equal(t, "09:00", timeOfDay{Hour: 9, Minute: 0}.String())
	require.Equal(t, "21:30", timeOfDay{Hour: 21, Minute: 30}.String())
}

// TestKindFor_Days — Mon-Fri = weekday; Sat/Sun = weekend.
func TestKindFor_Days(t *testing.T) {
	t.Parallel()
	moscow, err := time.LoadLocation("Europe/Moscow")
	require.NoError(t, err)

	// 2026-05-04 is a Monday (per Russian calendar; verified via Go).
	monday := time.Date(2026, time.May, 4, 12, 0, 0, 0, moscow)
	require.Equal(t, time.Monday, monday.Weekday())
	require.Equal(t, kindWeekday, kindFor(monday))

	saturday := time.Date(2026, time.May, 9, 12, 0, 0, 0, moscow)
	// May 9 is also Victory Day (a holiday) but kindFor only
	// classifies day-of-week; the holiday check happens in IsAllowed.
	require.Equal(t, time.Saturday, saturday.Weekday())
	require.Equal(t, kindWeekend, kindFor(saturday))

	sunday := time.Date(2026, time.May, 10, 12, 0, 0, 0, moscow)
	require.Equal(t, time.Sunday, sunday.Weekday())
	require.Equal(t, kindWeekend, kindFor(sunday))
}

// TestDayKindString — String() returns the canonical token used in
// log fields and error messages.
func TestDayKindString(t *testing.T) {
	t.Parallel()
	require.Equal(t, "weekday", kindWeekday.String())
	require.Equal(t, "weekend", kindWeekend.String())
	require.Equal(t, "unknown", dayKind(99).String())
}

// TestParsePolicy_ExceptionWithBadStart — the start in an exception
// override is malformed → error path of parseJSONException.
func TestParsePolicy_ExceptionWithBadStart(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"exceptions":[{"date":"2026-04-13","open":true,"start":"9:00","end":"21:00"}]}`)
	_, err := parsePolicy(raw)
	require.Error(t, err)
}

// TestParsePolicy_ExceptionWithBadEnd — the end in an exception
// override is malformed → error path of parseJSONException.
func TestParsePolicy_ExceptionWithBadEnd(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"exceptions":[{"date":"2026-04-13","open":true,"start":"09:00","end":"BAD"}]}`)
	_, err := parsePolicy(raw)
	require.Error(t, err)
}

// TestParsePolicy_ExceptionMissingDate — date is required.
func TestParsePolicy_ExceptionMissingDate(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"exceptions":[{"open":false}]}`)
	_, err := parsePolicy(raw)
	require.Error(t, err)
}

// TestParsePolicy_WindowMissingFields — start without end (or vice
// versa) is rejected.
func TestParsePolicy_WindowMissingFields(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"weekday":{"start":"09:00"}}`)
	_, err := parsePolicy(raw)
	require.Error(t, err)
}

// TestMustParsePolicy_PanicsOnBadJSON — convenience helper panics
// rather than returning an error; matches the rest of the platform's
// MustX helpers.
func TestMustParsePolicy_PanicsOnBadJSON(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = MustParsePolicy(`{"weekday":{`)
	})
}

// TestMustParsePolicy_HappyPath — the helper round-trips a valid
// blob into the same struct parsePolicy would produce.
func TestMustParsePolicy_HappyPath(t *testing.T) {
	t.Parallel()
	p := MustParsePolicy(`{"weekday":{"start":"08:00","end":"22:00"}}`)
	require.Equal(t, timeOfDay{Hour: 8, Minute: 0}, p.Weekday.Start)
	require.Equal(t, timeOfDay{Hour: 22, Minute: 0}, p.Weekday.End)
}

// TestWindowContains_HalfOpen — sanity: [start, end) — start
// inclusive, end exclusive.
func TestWindowContains_HalfOpen(t *testing.T) {
	t.Parallel()
	moscow, err := time.LoadLocation("Europe/Moscow")
	require.NoError(t, err)

	w := window{Start: timeOfDay{Hour: 9, Minute: 0}, End: timeOfDay{Hour: 21, Minute: 0}}

	cases := []struct {
		hour, min int
		want      bool
		label     string
	}{
		{8, 59, false, "minute before start"},
		{9, 0, true, "start exactly inclusive"},
		{12, 0, true, "midday inside"},
		{20, 59, true, "minute before end inside"},
		{21, 0, false, "end exactly exclusive"},
		{22, 0, false, "after end"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			t.Parallel()
			ts := time.Date(2026, time.May, 4, c.hour, c.min, 0, 0, moscow)
			require.Equal(t, c.want, w.contains(ts, moscow), "%s @ %02d:%02d", c.label, c.hour, c.min)
		})
	}
}
