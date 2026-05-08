// Package hours implements [api.WorkingHoursChecker] — the dialer's
// per-tenant + per-region permitted-dialing-window enforcement.
//
// # Why
//
// 152-ФЗ + § "rules of contact" require outbound calls to respondents
// to land inside locally-acceptable hours. The platform default is
// 09:00-21:00 weekdays / 10:00-18:00 weekends in the respondent's
// region-local time. Tenants may narrow (or, for a specific date,
// widen) the window via the "working_hours" entry in
// tenant_settings. Russian federal holidays (1-8 января, 23 февраля,
// 8 марта, 1 + 9 мая, 12 июня, 4 ноября for 2026) override every
// other rule — those days are always closed.
//
// # Decision order
//
// On every IsAllowed call the precedence chain is:
//
//  1. Region timezone — pkg/regions.TimezoneForRegion. Unknown region
//     → error (we refuse to guess).
//  2. Federal holiday — RUHolidays2026. Always-closed override.
//  3. Tenant exception (per-date entry in tenant_settings) — open=false
//     forces closed; open=true with a window overrides the per-day
//     default. Exception precedence is "today's date matches".
//  4. Default weekday/weekend window — either tenant-supplied or the
//     platform default if the tenant has no override.
//
// # Time math
//
// All time math goes through pkg/regions.TimezoneForRegion which loads
// the IANA-zone DB bundled into the binary via `_ "time/tzdata"`. This
// matters on FROM-scratch images that lack the OS tzdata package; the
// blank import in pkg/regions ensures Asia/Kamchatka, Asia/Vladivostok,
// Europe/Moscow and the rest of the 89 RU zones resolve at runtime.
//
// Window comparison is half-open: [start, end). 09:00:00.000 sharp is
// allowed; 21:00:00.000 sharp is end-of-day (denied). This matches the
// natural "the call must complete before 21:00" reading of the legal
// rule and keeps the math symmetric with the timer-based dispatch loop.
//
// # Settings shape
//
// The tenant override at tenant_settings.working_hours is a JSON object
// of the shape:
//
//	{
//	  "weekday": {"start": "09:00", "end": "21:00"},
//	  "weekend": {"start": "10:00", "end": "18:00"},
//	  "exceptions": [
//	    {"date": "2026-01-09", "open": false, "reason": "Plant closed"},
//	    {"date": "2026-12-31", "start": "09:00", "end": "16:00"}
//	  ]
//	}
//
// Every field is optional. Missing weekday / weekend → fall back to the
// platform default; missing exceptions → no per-day overrides. Invalid
// JSON returns an error from settings parsing — callers see it as
// IsAllowed errored, which is the safe default (the dispatch loop
// declines to call until the misconfiguration is fixed).
//
// # Plan 09 carry-forward
//
//   - *zap.Logger with typed fields. Tenant ID and region are logged;
//     phone numbers and respondent IDs never appear in this package.
//   - var _ api.WorkingHoursChecker = (*Checker)(nil) compile-time check.
//   - No init() MustRegister; metrics wire through RegisterMetrics(reg)
//     so two test imports don't collide on the default registerer.
//   - No time.After in loops; NextAllowed walks day-by-day with simple
//     time arithmetic.
package hours
