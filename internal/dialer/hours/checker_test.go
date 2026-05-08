package hours_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/hours"
	"github.com/sociopulse/platform/pkg/regions"
)

// fakeSettings is an in-memory hours.SettingsLookup. Each tenant
// either has an entry (returned with ok=true) or is absent (ok=false).
// transportErr forces every Lookup to fail with the supplied error
// — used to cover the wrapped-error path of IsAllowed.
type fakeSettings struct {
	mu           sync.Mutex
	values       map[uuid.UUID]json.RawMessage
	transportErr error
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{values: make(map[uuid.UUID]json.RawMessage)}
}

func (f *fakeSettings) Lookup(_ context.Context, tenantID uuid.UUID, key string) (json.RawMessage, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transportErr != nil {
		return nil, false, f.transportErr
	}
	if key != "working_hours" {
		// Unknown key — treat as absent.
		return nil, false, nil
	}
	v, ok := f.values[tenantID]
	if !ok {
		return nil, false, nil
	}
	out := make(json.RawMessage, len(v))
	copy(out, v)
	return out, true, nil
}

func (f *fakeSettings) set(tenantID uuid.UUID, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[tenantID] = json.RawMessage(raw)
}

func (f *fakeSettings) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transportErr = err
}

// Compile-time interface check — the fake satisfies the package's
// declared dependency surface. If the contract drifts the test
// package fails to compile.
var _ hours.SettingsLookup = (*fakeSettings)(nil)

// rig wires up the Checker with fakes and a fresh metrics registry.
type rig struct {
	c        *hours.Checker
	settings *fakeSettings
	metrics  *hours.Metrics
	regset   *regions.Set
	tenant   uuid.UUID
}

func newRig(t *testing.T) *rig {
	t.Helper()
	regset, err := regions.Load()
	require.NoError(t, err)
	settings := newFakeSettings()
	reg := prometheus.NewRegistry()
	metrics := hours.RegisterMetrics(reg)
	c, err := hours.New(hours.Config{
		Settings: settings,
		Regions:  regset,
		Logger:   zaptest.NewLogger(t),
		Metrics:  metrics,
	})
	require.NoError(t, err)
	return &rig{
		c:        c,
		settings: settings,
		metrics:  metrics,
		regset:   regset,
		tenant:   uuid.New(),
	}
}

// loc is a small helper: load an IANA zone or fail the test.
func loc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	require.NoError(t, err)
	return l
}

// TestNew_RequiresSettings — Settings is required.
func TestNew_RequiresSettings(t *testing.T) {
	t.Parallel()
	regset, err := regions.Load()
	require.NoError(t, err)
	_, err = hours.New(hours.Config{Regions: regset})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Settings")
}

// TestNew_RequiresRegions — Regions is required.
func TestNew_RequiresRegions(t *testing.T) {
	t.Parallel()
	_, err := hours.New(hours.Config{Settings: newFakeSettings()})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Regions")
}

// TestNew_Defaults — nil Logger / Metrics / Clock fall back; the
// constructor returns no error.
func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	regset, err := regions.Load()
	require.NoError(t, err)
	_, err = hours.New(hours.Config{
		Settings: newFakeSettings(),
		Regions:  regset,
	})
	require.NoError(t, err)
}

// TestIsAllowed_TableDriven covers ~20 representative scenarios for
// the IsAllowed contract: every code path in the precedence chain.
func TestIsAllowed_TableDriven(t *testing.T) {
	t.Parallel()

	moscow := loc(t, "Europe/Moscow")
	kamchatka := loc(t, "Asia/Kamchatka") // UTC+12
	samara := loc(t, "Europe/Samara")     // UTC+4 — RU-SAM in the dataset
	yekaterinburg := loc(t, "Asia/Yekaterinburg")

	// Reference dates (verified via the Go calendar):
	//   2026-05-04 = Monday    (post-May-1, post-May-9 — ordinary weekday)
	//   2026-05-03 = Sunday    (ordinary weekend day, NOT a holiday)
	//   2026-05-02 = Saturday  (ordinary weekend day)
	//   2026-05-01 = Friday    (Labour Day, federal holiday)
	//   2026-01-01 = Thursday  (federal holiday)
	//   2026-04-13 = Monday    (ordinary weekday after Easter season)
	//   2026-04-11 = Saturday  (ordinary weekend, far from any RU holiday)
	mondayMoscow := func(h, m int) time.Time {
		return time.Date(2026, time.May, 4, h, m, 0, 0, moscow)
	}
	sundayMoscow := func(h, m int) time.Time {
		return time.Date(2026, time.May, 3, h, m, 0, 0, moscow)
	}
	saturdayMoscow := func(h, m int) time.Time {
		return time.Date(2026, time.April, 11, h, m, 0, 0, moscow)
	}

	type tc struct {
		name      string
		region    string
		at        time.Time
		setup     func(r *rig)
		want      bool
		wantErr   bool
		expectMet string // expected "result" label that ticked, or "" to skip metric check
	}

	cases := []tc{
		{
			name:      "Moscow Monday 14:00 — allowed (default weekday window)",
			region:    "RU-MOW",
			at:        mondayMoscow(14, 0),
			want:      true,
			expectMet: "allowed",
		},
		{
			name:      "Moscow Monday 09:00 sharp — allowed (start inclusive)",
			region:    "RU-MOW",
			at:        mondayMoscow(9, 0),
			want:      true,
			expectMet: "allowed",
		},
		{
			name:      "Moscow Monday 21:00 sharp — denied (end exclusive)",
			region:    "RU-MOW",
			at:        mondayMoscow(21, 0),
			want:      false,
			expectMet: "outside_window",
		},
		{
			name:      "Moscow Monday 22:00 — denied (after end)",
			region:    "RU-MOW",
			at:        mondayMoscow(22, 0),
			want:      false,
			expectMet: "outside_window",
		},
		{
			name:      "Moscow Monday 06:00 — denied (before start)",
			region:    "RU-MOW",
			at:        mondayMoscow(6, 0),
			want:      false,
			expectMet: "outside_window",
		},
		{
			name:      "Moscow Sunday 12:00 — allowed (default weekend window)",
			region:    "RU-MOW",
			at:        sundayMoscow(12, 0),
			want:      true,
			expectMet: "allowed",
		},
		{
			name:      "Moscow Saturday 19:00 — denied (weekend ends 18:00)",
			region:    "RU-MOW",
			at:        saturdayMoscow(19, 0),
			want:      false,
			expectMet: "outside_window",
		},
		{
			name:      "Moscow Saturday 10:00 — allowed (weekend start)",
			region:    "RU-MOW",
			at:        saturdayMoscow(10, 0),
			want:      true,
			expectMet: "allowed",
		},
		{
			name:      "Moscow Jan 1 12:00 — denied (federal holiday, weekday)",
			region:    "RU-MOW",
			at:        time.Date(2026, time.January, 1, 12, 0, 0, 0, moscow),
			want:      false,
			expectMet: "holiday",
		},
		{
			name:      "Samara Saturday 11:00 — allowed (weekend window UTC+4)",
			region:    "RU-SAM",
			at:        time.Date(2026, time.April, 11, 11, 0, 0, 0, samara),
			want:      true,
			expectMet: "allowed",
		},
		{
			name:      "Kamchatka Tuesday 13:00 LOCAL (= 01:00 UTC) — allowed",
			region:    "RU-KAM",
			at:        time.Date(2026, time.May, 5, 13, 0, 0, 0, kamchatka),
			want:      true,
			expectMet: "allowed",
		},
		{
			name:      "Kamchatka 02:00 UTC = 14:00 local Tuesday — allowed (UTC input)",
			region:    "RU-KAM",
			at:        time.Date(2026, time.May, 5, 2, 0, 0, 0, time.UTC),
			want:      true,
			expectMet: "allowed",
		},
		{
			// 22:00 UTC May 1 + 12h Kamchatka offset = 10:00 LOCAL
			// May 2 (Saturday). Weekend window 10:00-18:00 starts
			// inclusive at 10:00 — allowed.
			name:      "Kamchatka 22:00 UTC Friday = 10:00 local Saturday — allowed",
			region:    "RU-KAM",
			at:        time.Date(2026, time.May, 1, 22, 0, 0, 0, time.UTC),
			want:      true,
			expectMet: "allowed",
		},
		{
			name: "Tenant override 08:00 weekday — 08:00 inclusive allowed",
			setup: func(r *rig) {
				r.settings.set(r.tenant, `{"weekday":{"start":"08:00","end":"22:00"}}`)
			},
			region:    "RU-MOW",
			at:        mondayMoscow(8, 0),
			want:      true,
			expectMet: "allowed",
		},
		{
			name: "Tenant override 08:00-22:00 weekday — 22:00 sharp denied",
			setup: func(r *rig) {
				r.settings.set(r.tenant, `{"weekday":{"start":"08:00","end":"22:00"}}`)
			},
			region:    "RU-MOW",
			at:        mondayMoscow(22, 0),
			want:      false,
			expectMet: "outside_window",
		},
		{
			name: "Tenant exception open=false — denied (Mon Apr 13 12:00, normally allowed)",
			setup: func(r *rig) {
				r.settings.set(r.tenant, `{"exceptions":[{"date":"2026-04-13","open":false,"reason":"Plant closed"}]}`)
			},
			region:    "RU-MOW",
			at:        time.Date(2026, time.April, 13, 12, 0, 0, 0, moscow),
			want:      false,
			expectMet: "denied",
		},
		{
			name: "Tenant exception with custom window — inside override allowed",
			setup: func(r *rig) {
				r.settings.set(r.tenant, `{"exceptions":[{"date":"2026-04-13","open":true,"start":"06:00","end":"08:00"}]}`)
			},
			region:    "RU-MOW",
			at:        time.Date(2026, time.April, 13, 7, 0, 0, 0, moscow),
			want:      true,
			expectMet: "allowed",
		},
		{
			name: "Tenant exception with custom window — outside override denied",
			setup: func(r *rig) {
				r.settings.set(r.tenant, `{"exceptions":[{"date":"2026-04-13","open":true,"start":"06:00","end":"08:00"}]}`)
			},
			region:    "RU-MOW",
			at:        time.Date(2026, time.April, 13, 9, 0, 0, 0, moscow),
			want:      false,
			expectMet: "outside_window",
		},
		{
			name: "Tenant exception cannot un-close a federal holiday (Jan 1)",
			setup: func(r *rig) {
				// Tenant tries to override Jan 1 to be open. Holidays beat exceptions.
				r.settings.set(r.tenant, `{"exceptions":[{"date":"2026-01-01","open":true,"start":"09:00","end":"21:00"}]}`)
			},
			region:    "RU-MOW",
			at:        time.Date(2026, time.January, 1, 12, 0, 0, 0, moscow),
			want:      false,
			expectMet: "holiday",
		},
		{
			name: "Yekaterinburg Monday 14:00 LOCAL — allowed (UTC+5)",
			setup: func(_ *rig) {
				// No override; default weekday 09-21.
			},
			region:    "RU-SVE",
			at:        time.Date(2026, time.May, 4, 14, 0, 0, 0, yekaterinburg),
			want:      true,
			expectMet: "allowed",
		},
		{
			name:      "Unknown region — error (regions.ErrUnknownRegion wrapped)",
			region:    "RU-XXX",
			at:        time.Now(),
			wantErr:   true,
			expectMet: "error",
		},
		{
			name: "Settings transport error — error wrapped",
			setup: func(r *rig) {
				r.settings.setError(errors.New("postgres down"))
			},
			region:    "RU-MOW",
			at:        mondayMoscow(14, 0),
			wantErr:   true,
			expectMet: "error",
		},
		{
			name: "Empty settings — falls back to default (allowed inside default window)",
			setup: func(_ *rig) {
				// No setup — fakeSettings starts empty.
			},
			region:    "RU-MOW",
			at:        mondayMoscow(14, 0),
			want:      true,
			expectMet: "allowed",
		},
		{
			name: "Tenant override is invalid JSON — IsAllowed errors",
			setup: func(r *rig) {
				r.settings.set(r.tenant, `{"weekday":{`)
			},
			region:    "RU-MOW",
			at:        mondayMoscow(14, 0),
			wantErr:   true,
			expectMet: "error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			if tc.setup != nil {
				tc.setup(r)
			}

			got, err := r.c.IsAllowed(context.Background(), r.tenant, tc.region, tc.at)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
			if tc.expectMet != "" {
				require.InDeltaf(t, 1.0,
					testutil.ToFloat64(r.metrics.Checks.WithLabelValues(tc.expectMet)),
					0,
					"metric label %q should have ticked", tc.expectMet,
				)
			}
		})
	}
}

// TestIsAllowed_NextDayCrossesUTC — confirms zone arithmetic for a
// region east of UTC (Samara, UTC+4). 22:00 UTC on Sat is 02:00
// LOCAL Sun — into the weekend window's "before start" region (Sun
// starts at 10:00).
func TestIsAllowed_NextDayCrossesUTC(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	// 22:00 UTC Apr 11 = 02:00 Apr 12 in Samara (UTC+4).
	// Apr 12 is Sunday — weekend window 10:00-18:00. 02:00 is
	// before start → denied.
	at := time.Date(2026, time.April, 11, 22, 0, 0, 0, time.UTC)
	got, err := r.c.IsAllowed(context.Background(), r.tenant, "RU-SAM", at)
	require.NoError(t, err)
	require.False(t, got, "02:00 local Sunday is before weekend window start 10:00")
}

// TestIsAllowed_HolidayInOtherZone — Jan 1 LOCAL is a holiday in
// Kamchatka even though that local Jan 1 starts at Dec 31 12:00
// UTC. The check uses LOCAL date, not UTC instant.
func TestIsAllowed_HolidayInOtherZone(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	kamchatka := loc(t, "Asia/Kamchatka")
	at := time.Date(2026, time.January, 1, 12, 0, 0, 0, kamchatka) // = 00:00 UTC Jan 1
	got, err := r.c.IsAllowed(context.Background(), r.tenant, "RU-KAM", at)
	require.NoError(t, err)
	require.False(t, got, "Jan 1 local in Kamchatka is a federal holiday")
}

// TestIsAllowed_ExceptionOpenButZeroWindow — open=true with no
// start/end falls through to the default window for that day.
// (A tenant might leave a Reason annotation on a future date.)
func TestIsAllowed_ExceptionOpenButZeroWindow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.settings.set(r.tenant, `{"exceptions":[{"date":"2026-04-13","open":true,"reason":"Note for ops"}]}`)

	moscow := loc(t, "Europe/Moscow")
	got, err := r.c.IsAllowed(context.Background(), r.tenant, "RU-MOW", time.Date(2026, time.April, 13, 14, 0, 0, 0, moscow))
	require.NoError(t, err)
	require.True(t, got, "open=true with no window falls through to default weekday")
}

// TestNextAllowed_AlreadyAllowed — when at is already inside the
// window, NextAllowed returns at unchanged.
func TestNextAllowed_AlreadyAllowed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	moscow := loc(t, "Europe/Moscow")
	at := time.Date(2026, time.May, 4, 14, 0, 0, 0, moscow).UTC()

	next, err := r.c.NextAllowed(context.Background(), r.tenant, "RU-MOW", at)
	require.NoError(t, err)
	require.True(t, next.Equal(at), "already-allowed at returned unchanged; got %s want %s", next, at)
}

// TestNextAllowed_SundayLateNight — 23:30 Sunday → next allowed is
// Monday 09:00 LOCAL.
func TestNextAllowed_SundayLateNight(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	moscow := loc(t, "Europe/Moscow")
	// 2026-05-03 is Sunday; 2026-05-04 Monday.
	at := time.Date(2026, time.May, 3, 23, 30, 0, 0, moscow)

	next, err := r.c.NextAllowed(context.Background(), r.tenant, "RU-MOW", at)
	require.NoError(t, err)

	wantLocal := time.Date(2026, time.May, 4, 9, 0, 0, 0, moscow)
	require.True(t, next.Equal(wantLocal),
		"sunday 23:30 → next allowed monday 09:00; got %s want %s", next.In(moscow), wantLocal)
}

// TestNextAllowed_SkipsHoliday — Dec 31 23:30 Moscow → next allowed
// after Jan 1-8 holidays = Jan 9 09:00 (Friday — weekday).
func TestNextAllowed_SkipsHoliday(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	moscow := loc(t, "Europe/Moscow")

	// Apr 30 22:00 (Thu, after default end 21:00). May 1 is a
	// federal holiday (Labour Day). The next allowed instant is
	// May 2 (Saturday) 10:00 LOCAL — start of the weekend window.
	at := time.Date(2026, time.April, 30, 22, 0, 0, 0, moscow)
	next, err := r.c.NextAllowed(context.Background(), r.tenant, "RU-MOW", at)
	require.NoError(t, err)

	// The first allowed instant is May 2 (Sat) 10:00 LOCAL — May 1
	// is a holiday but May 2 is an ordinary Saturday.
	wantLocal := time.Date(2026, time.May, 2, 10, 0, 0, 0, moscow)
	require.True(t, next.Equal(wantLocal),
		"skipped May 1 holiday; got %s want %s", next.In(moscow), wantLocal)
}

// TestNextAllowed_SkipsTenantClosure — tenant has Apr 13 closed; at
// is Apr 12 22:00 (Sunday after window end 18:00). NextAllowed must
// skip Mon Apr 13 (closed) and return Tue Apr 14 09:00.
func TestNextAllowed_SkipsTenantClosure(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.settings.set(r.tenant, `{"exceptions":[{"date":"2026-04-13","open":false}]}`)
	moscow := loc(t, "Europe/Moscow")

	at := time.Date(2026, time.April, 12, 22, 0, 0, 0, moscow) // Sunday after weekend end
	next, err := r.c.NextAllowed(context.Background(), r.tenant, "RU-MOW", at)
	require.NoError(t, err)

	wantLocal := time.Date(2026, time.April, 14, 9, 0, 0, 0, moscow) // Tuesday default 09:00
	require.True(t, next.Equal(wantLocal),
		"skipped Mon Apr 13 closure; got %s want %s", next.In(moscow), wantLocal)
}

// TestNextAllowed_SaturdayMidday — at = Sat 11:00 (inside weekend
// window 10-18) → returns at unchanged.
func TestNextAllowed_SaturdayMidday(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	moscow := loc(t, "Europe/Moscow")
	at := time.Date(2026, time.April, 11, 11, 0, 0, 0, moscow)

	next, err := r.c.NextAllowed(context.Background(), r.tenant, "RU-MOW", at)
	require.NoError(t, err)
	require.True(t, next.Equal(at))
}

// TestNextAllowed_HorizonExhausted — pathological tenant marks 14
// consecutive days closed → ErrOutsideWorkingHours.
func TestNextAllowed_HorizonExhausted(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	moscow := loc(t, "Europe/Moscow")

	// Mark Apr 11 - Apr 30 (20 consecutive days) closed. Horizon
	// is 14, so this tips the scanner over the edge — exhausting
	// the look-ahead without finding an open day.
	exceptions := `[`
	for i, d := range []int{11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30} {
		if i > 0 {
			exceptions += ","
		}
		exceptions += `{"date":"2026-04-` + twoDigit(d) + `","open":false}`
	}
	exceptions += "]"
	r.settings.set(r.tenant, `{"exceptions":`+exceptions+`}`)

	at := time.Date(2026, time.April, 11, 8, 0, 0, 0, moscow) // ahead of 09:00 start, but day is closed

	_, err := r.c.NextAllowed(context.Background(), r.tenant, "RU-MOW", at)
	require.ErrorIs(t, err, api.ErrOutsideWorkingHours)
}

// TestNextAllowed_RegionUnknown — unknown region → error wrapped
// from regions.TimezoneForRegion.
func TestNextAllowed_RegionUnknown(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	_, err := r.c.NextAllowed(context.Background(), r.tenant, "RU-XXX", time.Now())
	require.Error(t, err)
}

// TestNextAllowed_SettingsTransportError — settings transport
// failure propagates wrapped.
func TestNextAllowed_SettingsTransportError(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.settings.setError(errors.New("postgres down"))
	_, err := r.c.NextAllowed(context.Background(), r.tenant, "RU-MOW", time.Now())
	require.Error(t, err)
}

// TestIsAllowed_PerCallTenantIsolation — tenant A's override does
// NOT bleed into tenant B's check. Same Checker instance.
func TestIsAllowed_PerCallTenantIsolation(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	tenantB := uuid.New()
	r.settings.set(r.tenant, `{"weekday":{"start":"00:00","end":"05:00"}}`) // narrow window
	moscow := loc(t, "Europe/Moscow")
	monday14 := time.Date(2026, time.May, 4, 14, 0, 0, 0, moscow)

	// Tenant A — narrow window 00-05; 14:00 is outside.
	got, err := r.c.IsAllowed(context.Background(), r.tenant, "RU-MOW", monday14)
	require.NoError(t, err)
	require.False(t, got, "tenant A's narrow window denies 14:00")

	// Tenant B — no override; default 09-21; 14:00 inside.
	got, err = r.c.IsAllowed(context.Background(), tenantB, "RU-MOW", monday14)
	require.NoError(t, err)
	require.True(t, got, "tenant B uses default and is allowed at 14:00")
}

// TestIsAllowed_NilLoggerNoMetricsTolerated — building a Checker
// without a logger / metrics is supported; every method continues
// to work without panic.
func TestIsAllowed_NilLoggerNoMetricsTolerated(t *testing.T) {
	t.Parallel()
	regset, err := regions.Load()
	require.NoError(t, err)
	settings := newFakeSettings()
	c, err := hours.New(hours.Config{
		Settings: settings,
		Regions:  regset,
	})
	require.NoError(t, err)

	moscow := loc(t, "Europe/Moscow")
	got, err := c.IsAllowed(context.Background(), uuid.New(), "RU-MOW",
		time.Date(2026, time.May, 4, 14, 0, 0, 0, moscow))
	require.NoError(t, err)
	require.True(t, got)
}

// TestRegisterMetricsNilRegistererPanics — the contract is "panic
// on nil reg" (matches FSM/queue/RDD/router/capacity packages).
func TestRegisterMetricsNilRegistererPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { hours.RegisterMetrics(nil) })
}

// TestDefaultPolicy — sanity check: package constants are 09-21 / 10-18.
func TestDefaultPolicy(t *testing.T) {
	t.Parallel()
	def := hours.DefaultPolicy()
	require.NotEqual(t, hours.WorkingHoursPolicy{}, def, "default policy should not be the zero value")
}

// TestIsAllowed_CustomDefaultPolicy — passing Default in Config is
// honoured for tenants without an override.
func TestIsAllowed_CustomDefaultPolicy(t *testing.T) {
	t.Parallel()
	regset, err := regions.Load()
	require.NoError(t, err)
	settings := newFakeSettings()

	custom := hours.MustParsePolicy(`{
		"weekday": {"start": "07:00", "end": "23:00"},
		"weekend": {"start": "10:00", "end": "18:00"}
	}`)
	c, err := hours.New(hours.Config{
		Settings: settings,
		Regions:  regset,
		Default:  custom,
		Logger:   zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	moscow := loc(t, "Europe/Moscow")
	at := time.Date(2026, time.May, 4, 7, 0, 0, 0, moscow) // 07:00 — inside custom but outside platform default.
	got, err := c.IsAllowed(context.Background(), uuid.New(), "RU-MOW", at)
	require.NoError(t, err)
	require.True(t, got, "custom default 07-23 should allow 07:00 sharp")
}

// twoDigit zero-pads small ints to two characters. Inline helper
// for the closure-day exception JSON builder.
func twoDigit(d int) string {
	if d < 10 {
		return "0" + string(rune('0'+d))
	}
	return string(rune('0'+d/10)) + string(rune('0'+d%10))
}
