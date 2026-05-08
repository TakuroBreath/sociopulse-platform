package hours

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SettingsLookup is the small slice of [tenancy.SettingsCache] the
// hours package consumes. Defined here (rather than imported as the
// full tenancy interface) so:
//
//  1. Unit tests pass a tiny in-memory fake without dragging the
//     entire tenancy stack in;
//  2. The depguard module-boundaries rule stays satisfied — the
//     hours package depends only on the api shape, not on
//     internal/tenancy/service.
//
// Production wiring adapts internal/tenancy/api.SettingsCache to this
// shape (a one-method translator that maps tenancy.ErrNotFound to
// ok=false). See [Checker.lookup] for the contract — ok=false means
// "no override; use the default".
type SettingsLookup interface {
	Lookup(ctx context.Context, tenantID uuid.UUID, key string) (json.RawMessage, bool, error)
}

// settingsKey is the canonical tenant_settings entry name the hours
// package reads. Centralised so the production adapter and the unit
// tests use the same key string.
const settingsKey = "working_hours"

// WorkingHoursPolicy is the parsed shape of the tenant override
// (or the platform default when no override exists). All fields are
// optional in the JSON wire form; missing weekday / weekend fall
// back to the package-level default at parse time.
type WorkingHoursPolicy struct {
	// Weekday is the Mon-Fri window. Zero value is treated as
	// "unset" by mergeWithDefault, in which case the platform
	// default 09:00-21:00 fills in.
	Weekday window

	// Weekend is the Sat-Sun window. Zero value is "unset"; the
	// platform default 10:00-18:00 fills in.
	Weekend window

	// Exceptions is the list of per-date overrides. Order is
	// preserved from the JSON; lookups iterate the slice and stop
	// at the first match — duplicates are harmless because the
	// match is by date, not index.
	Exceptions []Exception
}

// Exception is one per-day override row in the tenant config. Either
// Open=false (closed all day, regardless of the default window) or
// Open=true with a Start/End window override.
type Exception struct {
	// Date is the local calendar date the exception applies to.
	// Stored zero-time — only year/month/day are significant.
	Date time.Time

	// Open=false → the day is closed regardless of any window.
	// Open=true with Start/End set → use the override window for
	// that date.
	Open bool

	// Start / End are the override window. Significant only when
	// Open is true; ignored when Open is false.
	Start timeOfDay
	End   timeOfDay

	// Reason is an audit / log string. Not consumed by the
	// allowed-or-not decision; included in error / log fields when
	// a request is denied so an operator can trace the reason.
	Reason string
}

// jsonPolicy is the on-the-wire shape of the tenant override. Fields
// are pointers so we can distinguish "absent" from "zero" — an
// explicit `{"weekday": {"start": "00:00", "end": "00:00"}}` is a
// misconfiguration we should reject, while a missing "weekday" key
// should fall back to the default.
type jsonPolicy struct {
	Weekday    *jsonWindow     `json:"weekday,omitempty"`
	Weekend    *jsonWindow     `json:"weekend,omitempty"`
	Exceptions []jsonException `json:"exceptions,omitempty"`
}

type jsonWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type jsonException struct {
	Date   string `json:"date"`
	Open   *bool  `json:"open,omitempty"`
	Start  string `json:"start,omitempty"`
	End    string `json:"end,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// parsePolicy decodes the tenant override JSON. Returns the parsed
// policy with whatever fields were present; the caller is
// responsible for filling missing windows with the package default
// via [mergeWithDefault].
//
// Empty / nil input is NOT an error here: callers use ok=false from
// SettingsLookup to detect "no override" and skip the parse entirely.
// If they did call parsePolicy with empty bytes, the result is the
// zero WorkingHoursPolicy — also valid; mergeWithDefault then fills
// every field.
func parsePolicy(raw []byte) (WorkingHoursPolicy, error) {
	if len(raw) == 0 {
		return WorkingHoursPolicy{}, nil
	}
	var jp jsonPolicy
	if err := json.Unmarshal(raw, &jp); err != nil {
		return WorkingHoursPolicy{}, fmt.Errorf("hours: parse working_hours: %w", err)
	}

	out := WorkingHoursPolicy{}
	if jp.Weekday != nil {
		w, err := parseJSONWindow(*jp.Weekday)
		if err != nil {
			return WorkingHoursPolicy{}, fmt.Errorf("hours: weekday: %w", err)
		}
		out.Weekday = w
	}
	if jp.Weekend != nil {
		w, err := parseJSONWindow(*jp.Weekend)
		if err != nil {
			return WorkingHoursPolicy{}, fmt.Errorf("hours: weekend: %w", err)
		}
		out.Weekend = w
	}
	if len(jp.Exceptions) > 0 {
		out.Exceptions = make([]Exception, 0, len(jp.Exceptions))
		for i, je := range jp.Exceptions {
			ex, err := parseJSONException(je)
			if err != nil {
				return WorkingHoursPolicy{}, fmt.Errorf("hours: exception %d: %w", i, err)
			}
			out.Exceptions = append(out.Exceptions, ex)
		}
	}
	return out, nil
}

func parseJSONWindow(jw jsonWindow) (window, error) {
	if jw.Start == "" || jw.End == "" {
		return window{}, errors.New("start/end required")
	}
	s, err := parseTimeOfDay(jw.Start)
	if err != nil {
		return window{}, err
	}
	e, err := parseTimeOfDay(jw.End)
	if err != nil {
		return window{}, err
	}
	return window{Start: s, End: e}, nil
}

func parseJSONException(je jsonException) (Exception, error) {
	if je.Date == "" {
		return Exception{}, errors.New("date required")
	}
	d, err := time.Parse("2006-01-02", je.Date)
	if err != nil {
		return Exception{}, fmt.Errorf("date %q: %w", je.Date, err)
	}
	ex := Exception{
		Date:   d,
		Reason: je.Reason,
	}
	if je.Open != nil {
		ex.Open = *je.Open
	} else {
		// Default behaviour for a date entry: open=true. The
		// caller can still mark a day closed by setting open=false
		// explicitly. open=true without a window is the
		// "tenant-supplied default applies, just use the day's
		// kind window" — but since the default already covers
		// that, an Open=true exception MUST also supply Start/End
		// to be useful. Tolerate the no-op case (it's a no-op,
		// not an error) so we don't reject configs that include
		// an exception entry purely as a Reason annotation for a
		// future date.
		ex.Open = true
	}
	if je.Start != "" || je.End != "" {
		s, err := parseTimeOfDay(je.Start)
		if err != nil {
			return Exception{}, fmt.Errorf("exception start: %w", err)
		}
		e, err := parseTimeOfDay(je.End)
		if err != nil {
			return Exception{}, fmt.Errorf("exception end: %w", err)
		}
		ex.Start = s
		ex.End = e
	}
	return ex, nil
}

// mergeWithDefault fills any zero-valued window in p with the
// corresponding window from def. Called once per IsAllowed (cheap —
// the policy struct is tiny) so the rest of the decision logic can
// rely on both Weekday and Weekend being populated.
func mergeWithDefault(p WorkingHoursPolicy, def WorkingHoursPolicy) WorkingHoursPolicy {
	if p.Weekday == (window{}) {
		p.Weekday = def.Weekday
	}
	if p.Weekend == (window{}) {
		p.Weekend = def.Weekend
	}
	return p
}

// findException returns the first exception whose Date matches the
// local calendar date of t in loc, or (zero, false) when no match.
// Iteration is linear because the typical exceptions list is tiny
// (a dozen rows at most — federal holidays + a handful of tenant-
// specific closures).
func findException(p WorkingHoursPolicy, t time.Time, loc *time.Location) (Exception, bool) {
	y, mo, d := t.In(loc).Date()
	for _, e := range p.Exceptions {
		ey, emo, ed := e.Date.Date()
		if y == ey && mo == emo && d == ed {
			return e, true
		}
	}
	return Exception{}, false
}

// MustParsePolicy parses a JSON working_hours blob into a
// WorkingHoursPolicy and panics on error. Convenience for tests and
// for the composition root that wires a hard-coded platform default;
// production tenant configs go through parsePolicy via the Checker's
// settings lookup, which surfaces parse failures as wrapped errors.
func MustParsePolicy(rawJSON string) WorkingHoursPolicy {
	p, err := parsePolicy([]byte(rawJSON))
	if err != nil {
		panic("hours.MustParsePolicy: " + err.Error())
	}
	return p
}
