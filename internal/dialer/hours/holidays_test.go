package hours_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/dialer/hours"
)

// TestRUHolidays2026_AllDatesPresent — every documented federal
// holiday for 2026 is in the set. This locks the seed against an
// accidental rename / typo in NewRUHolidays2026.
func TestRUHolidays2026_AllDatesPresent(t *testing.T) {
	t.Parallel()
	moscow, err := time.LoadLocation("Europe/Moscow")
	require.NoError(t, err)
	set := hours.NewRUHolidays2026()

	expected := []struct {
		month time.Month
		day   int
		label string
	}{
		{time.January, 1, "New Year"},
		{time.January, 2, "New Year holidays"},
		{time.January, 3, "New Year holidays"},
		{time.January, 4, "New Year holidays"},
		{time.January, 5, "New Year holidays"},
		{time.January, 6, "New Year holidays"},
		{time.January, 7, "Orthodox Christmas"},
		{time.January, 8, "New Year holidays"},
		{time.February, 23, "Defender of the Fatherland Day"},
		{time.March, 8, "International Women's Day"},
		{time.May, 1, "Labour Day"},
		{time.May, 9, "Victory Day"},
		{time.June, 12, "Russia Day"},
		{time.November, 4, "Unity Day"},
	}
	for _, e := range expected {
		instant := time.Date(2026, e.month, e.day, 12, 0, 0, 0, moscow)
		require.True(t, set.IsHoliday(instant, moscow),
			"%s (%s %d) should be a holiday", e.label, e.month, e.day)
	}
}

// TestRUHolidays2026_NonHolidaysAbsent — common non-holiday dates
// must NOT be in the set. Catches a regression that, e.g., adds Feb
// 22 instead of Feb 23.
func TestRUHolidays2026_NonHolidaysAbsent(t *testing.T) {
	t.Parallel()
	moscow, err := time.LoadLocation("Europe/Moscow")
	require.NoError(t, err)
	set := hours.NewRUHolidays2026()

	nonHolidays := []struct {
		month time.Month
		day   int
		label string
	}{
		{time.January, 9, "first work day after NY"},
		{time.January, 15, "ordinary January workday"},
		{time.February, 22, "day before Defender's"},
		{time.February, 24, "day after Defender's"},
		{time.March, 7, "day before March 8"},
		{time.March, 9, "day after March 8"},
		{time.April, 30, "day before May 1"},
		{time.May, 2, "between May 1 and 9"},
		{time.May, 8, "between May 1 and 9"},
		{time.May, 10, "day after May 9"},
		{time.June, 11, "day before Russia Day"},
		{time.November, 3, "day before Unity Day"},
		{time.November, 5, "day after Unity Day"},
		{time.December, 31, "New Year's Eve"},
	}
	for _, e := range nonHolidays {
		instant := time.Date(2026, e.month, e.day, 12, 0, 0, 0, moscow)
		require.False(t, set.IsHoliday(instant, moscow),
			"%s (%s %d) must NOT be a holiday", e.label, e.month, e.day)
	}
}

// TestIsHoliday_ZoneAgnostic — Jan 1 12:00 LOCAL in any RU zone is
// still a holiday. The set is keyed by Europe/Moscow midnights but
// the comparison is by local calendar date — so a call from
// Asia/Kamchatka on Jan 1 must still hit the holiday branch.
func TestIsHoliday_ZoneAgnostic(t *testing.T) {
	t.Parallel()
	set := hours.NewRUHolidays2026()
	zones := []string{
		"Europe/Moscow",
		"Asia/Yekaterinburg",
		"Asia/Novosibirsk",
		"Asia/Vladivostok",
		"Asia/Kamchatka",
		"Europe/Kaliningrad",
	}
	for _, name := range zones {
		loc, err := time.LoadLocation(name)
		require.NoError(t, err, name)
		instant := time.Date(2026, time.January, 1, 12, 0, 0, 0, loc)
		require.True(t, set.IsHoliday(instant, loc),
			"%s: Jan 1 12:00 LOCAL must be a holiday", name)
	}
}

// TestIsHoliday_NilSafe — a nil HolidaySet returns false rather
// than panic. Defence against an accidental zero-value Config.
func TestIsHoliday_NilSafe(t *testing.T) {
	t.Parallel()
	moscow, err := time.LoadLocation("Europe/Moscow")
	require.NoError(t, err)
	var nilSet hours.HolidaySet
	require.False(t, nilSet.IsHoliday(time.Date(2026, time.January, 1, 12, 0, 0, 0, moscow), moscow))
}

// TestIsHoliday_NilLoc — passing a nil location returns false. Same
// reason as the nil set: the package must never panic on a missing
// dependency, only return false / surface an error at the call
// site.
func TestIsHoliday_NilLoc(t *testing.T) {
	t.Parallel()
	set := hours.NewRUHolidays2026()
	require.False(t, set.IsHoliday(time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC), nil))
}

// TestDayKey_NormalisesToZero — DayKey returns a zero-time-of-day
// time at the local date. Two calls with different sub-day fields
// on the same calendar date in the same zone yield the same key.
func TestDayKey_NormalisesToZero(t *testing.T) {
	t.Parallel()
	moscow, err := time.LoadLocation("Europe/Moscow")
	require.NoError(t, err)

	a := hours.DayKey(time.Date(2026, time.January, 1, 5, 30, 0, 0, moscow), moscow)
	b := hours.DayKey(time.Date(2026, time.January, 1, 23, 59, 59, 0, moscow), moscow)
	require.Equal(t, a, b, "same local date → same key regardless of time-of-day")
	require.Equal(t, 0, a.Hour())
	require.Equal(t, 0, a.Minute())
	require.Equal(t, 0, a.Second())
}
