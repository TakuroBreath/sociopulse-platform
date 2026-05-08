package hours

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/pkg/regions"
)

// Default platform windows. Used when the tenant has no override.
// Source: Plan 10 §"Working hours" — 09-21 weekday / 10-18 weekend.
var (
	defaultWeekday = window{Start: timeOfDay{Hour: 9, Minute: 0}, End: timeOfDay{Hour: 21, Minute: 0}}
	defaultWeekend = window{Start: timeOfDay{Hour: 10, Minute: 0}, End: timeOfDay{Hour: 18, Minute: 0}}
)

// nextAllowedHorizon caps the day-by-day scan in NextAllowed. The
// largest contiguous closed stretch in the RU calendar is the New
// Year holidays (Jan 1-8) — 8 days. Add a safety buffer for tenants
// that mark Sat-Sun closed via per-day exceptions adjacent to
// holidays and you get ~10 days. We round up to 14 so a wildly
// misconfigured tenant (e.g. a closed-for-two-weeks override)
// surfaces the misconfig as ErrOutsideWorkingHours rather than
// looping forever.
const nextAllowedHorizon = 14

// DefaultPolicy returns the platform default — 09-21 weekday and
// 10-18 weekend — that the Checker uses when a tenant has no
// `working_hours` override in tenant_settings. Exposed so callers
// can inspect / log the active default without poking package
// internals.
func DefaultPolicy() WorkingHoursPolicy {
	return WorkingHoursPolicy{
		Weekday: defaultWeekday,
		Weekend: defaultWeekend,
	}
}

// Config bundles the dependencies and tunable values for a Checker.
// Required fields are documented per-field; nil-tolerated fields
// fall back to safe defaults so the constructor stays trivially
// wireable from tests.
type Config struct {
	// Settings is the tenant-keyed override lookup. Required.
	// Production wires an adapter over tenancy.SettingsCache;
	// tests pass an in-memory fake.
	Settings SettingsLookup

	// Regions is the loaded RU regions snapshot (driving timezone
	// resolution). Required — every IsAllowed call goes through
	// regions.TimezoneForRegion.
	Regions *regions.Set

	// Default is the platform default policy used when the tenant
	// has no override. Zero value → DefaultPolicy() (09-21 weekday
	// / 10-18 weekend). Production normally relies on the package
	// default; this field exists so tests can pin a different
	// platform default.
	Default WorkingHoursPolicy

	// Holidays is the immutable set of always-closed days. Zero
	// value → NewRUHolidays2026(). Tests that want to stress the
	// non-holiday paths pass an empty HolidaySet{} explicitly.
	Holidays HolidaySet

	// Logger receives per-method diagnostics. nil → zap.NewNop().
	// Per Plan 09 carry-forward, fields are typed and never carry
	// PII — this package only logs tenant ID + region code +
	// decision label.
	Logger *zap.Logger

	// Clock returns the current time. nil → time.Now. Tests pass a
	// frozen clock; the Checker uses it only as a default for
	// IsAllowed when the caller didn't supply an `at` (callers in
	// the hot dispatch loop always pass at; this is for symmetry
	// with the rest of the dialer packages).
	Clock func() time.Time

	// Metrics is the per-package collector group. nil → no metrics
	// (the Checker is fully functional without it).
	Metrics *Metrics
}

// Checker implements [api.WorkingHoursChecker]. Stateless beyond the
// configured dependencies — concurrent IsAllowed / NextAllowed calls
// share the same SettingsLookup and Regions snapshot but do not
// share per-call state.
type Checker struct {
	settings SettingsLookup
	regions  *regions.Set
	def      WorkingHoursPolicy
	holidays HolidaySet
	log      *zap.Logger
	clock    func() time.Time
	metrics  *Metrics
}

// Compile-time interface check. Surfaces api.WorkingHoursChecker
// signature drift the moment it happens (per Plan 09 lessons §8).
var _ api.WorkingHoursChecker = (*Checker)(nil)

// New constructs a Checker. Returns an error when a required
// dependency is missing; nil-tolerated fields are filled with
// defaults so callers can pass a minimal Config{Settings: ...,
// Regions: ...} for the simplest wiring.
//
// Holidays defaults to NewRUHolidays2026 — the canonical 2026 RU
// federal calendar. Tests that want zero-holidays semantics must
// pass an explicitly-empty HolidaySet{} (a non-nil empty map). Nil
// means "use the default", consistent with how the other Config
// fields treat zero values.
func New(cfg Config) (*Checker, error) {
	if cfg.Settings == nil {
		return nil, errors.New("hours.New: Settings is required")
	}
	if cfg.Regions == nil {
		return nil, errors.New("hours.New: Regions is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	def := cfg.Default
	if def.Weekday == (window{}) {
		def.Weekday = defaultWeekday
	}
	if def.Weekend == (window{}) {
		def.Weekend = defaultWeekend
	}
	holidays := cfg.Holidays
	if holidays == nil {
		holidays = NewRUHolidays2026()
	}
	return &Checker{
		settings: cfg.Settings,
		regions:  cfg.Regions,
		def:      def,
		holidays: holidays,
		log:      logger,
		clock:    clock,
		metrics:  cfg.Metrics,
	}, nil
}

// IsAllowed reports whether dialing is permitted for the named
// region at instant at. The decision precedence is:
//
//  1. Region timezone resolution. Unknown region → error.
//  2. Federal holiday (RUHolidays2026) in the LOCAL date → false.
//  3. Tenant exception matching the LOCAL date → either Open=false
//     (forces closed) or override window for the day.
//  4. Default weekday/weekend window (tenant-supplied or platform
//     default).
//
// The local time is half-open against the active window: [start,
// end). 09:00 sharp is allowed; 21:00 sharp is end-of-day (denied).
//
// at is interpreted as UTC by convention but the comparison is
// zone-correct via t.In(loc) — passing a time.Time already in the
// local zone is equivalent.
func (c *Checker) IsAllowed(ctx context.Context, tenantID uuid.UUID, region string, at time.Time) (bool, error) {
	loc, err := c.regions.TimezoneForRegion(region)
	if err != nil {
		c.metrics.observeCheck(resultError)
		return false, fmt.Errorf("hours.IsAllowed: tz lookup: %w", err)
	}

	local := at.In(loc)

	// Federal holidays — always closed; short-circuits before the
	// tenant exception lookup.
	if c.holidays.IsHoliday(local, loc) {
		c.metrics.observeCheck(resultHoliday)
		c.log.Debug("dialing denied: federal holiday",
			zap.String("tenant_id", tenantID.String()),
			zap.String("region", region),
			zap.String("local", local.Format(time.RFC3339)),
		)
		return false, nil
	}

	policy, err := c.policyFor(ctx, tenantID)
	if err != nil {
		c.metrics.observeCheck(resultError)
		return false, fmt.Errorf("hours.IsAllowed: settings: %w", err)
	}

	if ex, ok := findException(policy, local, loc); ok {
		// Open=false dominates regardless of any window the
		// tenant might also have included on the row.
		if !ex.Open {
			c.metrics.observeCheck(resultDenied)
			c.log.Debug("dialing denied: tenant exception (closed)",
				zap.String("tenant_id", tenantID.String()),
				zap.String("region", region),
				zap.String("local", local.Format(time.RFC3339)),
				zap.String("reason", ex.Reason),
			)
			return false, nil
		}
		// Open=true with a non-empty window → use override window
		// for that day; Open=true with zero window → treat as if
		// no exception existed (fall through to default).
		exWin := window{Start: ex.Start, End: ex.End}
		if exWin != (window{}) {
			if exWin.contains(local, loc) {
				c.metrics.observeCheck(resultAllowed)
				return true, nil
			}
			c.metrics.observeCheck(resultOutsideWindow)
			return false, nil
		}
	}

	// Default day-kind window.
	var win window
	switch kindFor(local) {
	case kindWeekday:
		win = policy.Weekday
	case kindWeekend:
		win = policy.Weekend
	}
	if win.contains(local, loc) {
		c.metrics.observeCheck(resultAllowed)
		return true, nil
	}
	c.metrics.observeCheck(resultOutsideWindow)
	return false, nil
}

// NextAllowed returns the next instant ≥ at when dialing becomes
// permitted. If at itself is allowed → returns at unchanged (in UTC).
//
// Algorithm:
//
//  1. Check at; if allowed, return.
//  2. For the current local day, if it's not a holiday and the
//     active window (default or exception) starts after the
//     current local time, return that local start in UTC.
//  3. Otherwise advance to the start of the next local day and
//     repeat. Cap the scan at nextAllowedHorizon days; if we
//     exhaust without finding an open window, return
//     ErrOutsideWorkingHours (defensive — this would only fire on
//     a misconfigured tenant marking 14+ consecutive days closed).
//
// The returned time is in UTC, matching the api.WorkingHoursChecker
// contract.
func (c *Checker) NextAllowed(ctx context.Context, tenantID uuid.UUID, region string, at time.Time) (time.Time, error) {
	loc, err := c.regions.TimezoneForRegion(region)
	if err != nil {
		return time.Time{}, fmt.Errorf("hours.NextAllowed: tz lookup: %w", err)
	}
	policy, err := c.policyFor(ctx, tenantID)
	if err != nil {
		return time.Time{}, fmt.Errorf("hours.NextAllowed: settings: %w", err)
	}

	// Day-by-day scan starting from the local instant of at.
	cursor := at.In(loc)
	for day := 0; day <= nextAllowedHorizon; day++ {
		// Resolve the active window for this day. Holiday or
		// exception with Open=false → no window; advance the
		// cursor to the next day.
		win, hasWindow := c.activeWindow(policy, cursor, loc)
		if hasWindow {
			start := combineDate(cursor, loc, win.Start)
			end := combineDate(cursor, loc, win.End)
			// If the cursor is before the window's start, the
			// next allowed instant is the window's start. If
			// it's INSIDE the window, the next allowed instant
			// is now (== cursor, but only on day 0). If it's
			// past the window's end, fall through to the next
			// day.
			switch {
			case cursor.Before(start):
				return start.UTC(), nil
			case cursor.Before(end):
				// Inside the window — allowed right now.
				return cursor.UTC(), nil
			}
		}
		// Advance to start-of-next-local-day and re-check.
		nextDay := time.Date(cursor.Year(), cursor.Month(), cursor.Day()+1, 0, 0, 0, 0, loc)
		cursor = nextDay
	}
	c.log.Warn("hours.NextAllowed: horizon exhausted",
		zap.String("tenant_id", tenantID.String()),
		zap.String("region", region),
		zap.Int("horizon_days", nextAllowedHorizon),
	)
	return time.Time{}, api.ErrOutsideWorkingHours
}

// activeWindow returns the window that applies to the local date of
// t in loc, plus a bool indicating whether ANY window applies (false
// when the day is a federal holiday or a tenant Open=false
// exception). Centralised so IsAllowed and NextAllowed share the
// precedence chain.
func (c *Checker) activeWindow(p WorkingHoursPolicy, t time.Time, loc *time.Location) (window, bool) {
	if c.holidays.IsHoliday(t, loc) {
		return window{}, false
	}
	if ex, ok := findException(p, t, loc); ok {
		if !ex.Open {
			return window{}, false
		}
		exWin := window{Start: ex.Start, End: ex.End}
		if exWin != (window{}) {
			return exWin, true
		}
		// Open=true with zero window — fall through to the
		// day-kind default below.
	}
	switch kindFor(t) {
	case kindWeekday:
		return p.Weekday, true
	case kindWeekend:
		return p.Weekend, true
	}
	// Unreachable: kindFor only returns Weekday/Weekend.
	return window{}, false
}

// policyFor loads the tenant's working_hours override (if any) and
// merges the platform default into any unset fields. Settings
// transport errors propagate; ok=false from SettingsLookup is the
// "no override; use default" signal and is NOT an error.
func (c *Checker) policyFor(ctx context.Context, tenantID uuid.UUID) (WorkingHoursPolicy, error) {
	raw, ok, err := c.settings.Lookup(ctx, tenantID, settingsKey)
	if err != nil {
		return WorkingHoursPolicy{}, err
	}
	if !ok {
		return c.def, nil
	}
	parsed, err := parsePolicy(raw)
	if err != nil {
		return WorkingHoursPolicy{}, err
	}
	return mergeWithDefault(parsed, c.def), nil
}
