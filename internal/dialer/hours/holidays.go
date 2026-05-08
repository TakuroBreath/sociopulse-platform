package hours

import "time"

// HolidaySet is an immutable set of date-only days that are always
// closed. The map key is the local date with all sub-day fields
// (hour/min/sec/nsec) zero — see [DayKey] for the canonical
// constructor that callers use to look up "is THIS instant on a
// holiday day in THIS zone?".
//
// The zero value is a usable empty set; callers should treat instances
// returned from [NewRUHolidays2026] as read-only — mutating the map
// from outside the package would race with concurrent IsAllowed calls.
type HolidaySet map[time.Time]bool

// DayKey returns the canonical date-only key for t in loc. Used both
// by [NewRUHolidays2026] (when seeding) and by [Checker.IsAllowed]
// (when looking up). The "all sub-day fields zeroed" rule is what
// makes the set comparable across an instant in any zone — every UTC
// instant maps to exactly one local date.
func DayKey(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// NewRUHolidays2026 returns the canonical set of Russian Federation
// 2026 federal non-working days, keyed by Europe/Moscow midnights.
// The keys are deliberately stored in Europe/Moscow rather than UTC
// because the IsHoliday check compares against the LOCAL date in the
// caller's region — and Russian holidays are calendar dates, not
// instants. A call from RU-KAM (UTC+12) on Jan 1 LOCAL is still on
// the holiday (because the LOCAL date matches), regardless of what
// UTC instant that maps to.
//
// Source: ст. 112 ТК РФ + Постановление Правительства РФ № 1648 от
// 29.10.2025 (production calendar 2026):
//
//   - Jan 1-8: Новогодние каникулы + Рождество Христово
//   - Feb 23: День защитника Отечества
//   - Mar 8: Международный женский день
//   - May 1:  Праздник весны и труда
//   - May 9:  День Победы
//   - Jun 12: День России
//   - Nov 4:  День народного единства
//
// Working-day TRANSFERS (e.g. Saturday → Monday compensations) are
// NOT modelled — the dialer enforces "call within local working
// hours", and a transfer day is still a working day at the local
// 09:00-21:00 cadence. Tenant-side per-day exceptions can override
// when a specific tenant treats a transfer day differently.
func NewRUHolidays2026() HolidaySet {
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		// time/tzdata is bundled into the binary via pkg/regions —
		// LoadLocation cannot fail at runtime. A panic here surfaces
		// the impossible-but-detected misconfiguration loudly at boot
		// rather than at first IsAllowed call.
		panic("hours.NewRUHolidays2026: load Europe/Moscow: " + err.Error())
	}
	dates := []struct {
		month time.Month
		day   int
	}{
		{time.January, 1}, {time.January, 2}, {time.January, 3}, {time.January, 4},
		{time.January, 5}, {time.January, 6}, {time.January, 7}, {time.January, 8},
		{time.February, 23},
		{time.March, 8},
		{time.May, 1}, {time.May, 9},
		{time.June, 12},
		{time.November, 4},
	}
	out := make(HolidaySet, len(dates))
	for _, d := range dates {
		out[time.Date(2026, d.month, d.day, 0, 0, 0, 0, moscow)] = true
	}
	return out
}

// IsHoliday reports whether t (UTC or any zone) falls on a holiday
// day in the local zone loc. Comparison is by year-month-day; the
// local-date semantics match the way Russian holidays are
// conceptualised (a calendar date, not a UTC instant).
func (h HolidaySet) IsHoliday(t time.Time, loc *time.Location) bool {
	if h == nil || loc == nil {
		return false
	}
	local := t.In(loc)
	// Compare on (year, month, day) rather than equating two
	// time.Time values directly — the seed is in Europe/Moscow, but
	// any zone's midnight on the same calendar date is what we want
	// to match. Iterating is fine: the set is at most ~14 entries.
	y, mo, d := local.Date()
	for k := range h {
		ky, kmo, kd := k.Date()
		if y == ky && mo == kmo && d == kd {
			return true
		}
	}
	return false
}
