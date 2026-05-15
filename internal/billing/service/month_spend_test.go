package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
)

// fakeAggregator is an in-memory service.AggregatorBackend stand-in for
// unit-testing SpendReport without touching Postgres. The service-layer
// tests here exercise period validation, tariff load, and arithmetic; the
// SQL itself is integration-tested in store/pgx/spend_pg_test.go.
type fakeAggregator struct {
	agg      billingpgx.CallCostsAggregate
	imported int64
	err      error
}

// SumCallCosts mirrors the production aggregator return shape.
func (f *fakeAggregator) SumCallCosts(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _, _ time.Time) (billingpgx.CallCostsAggregate, error) {
	if f.err != nil {
		return billingpgx.CallCostsAggregate{}, f.err
	}
	return f.agg, nil
}

// CountImportedRecords mirrors the production count return shape.
func (f *fakeAggregator) CountImportedRecords(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _, _ time.Time) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.imported, nil
}

// TestMonthSpend_HappyPath verifies the canonical arithmetic: SumCallCosts +
// CountImportedRecords + Tariffs.FixedFeesMinor + Tariffs.RespondentBasesMinor
// roll up into a MonthBreakdown whose TotalMin is the sum of every line item
// and whose CostPerSurveyMinor convenience method divides by Surveys.
func TestMonthSpend_HappyPath(t *testing.T) {
	t.Parallel()
	def := billingapi.Tariffs{
		FixedFeesMinor:       500000, // 5,000 ₽
		RespondentBasesMinor: 50,     // 0.50 ₽ / row
	}
	tariffs := service.NewTariffStore(newFakeBackend(), def)
	agg := billingpgx.CallCostsAggregate{
		TelecomMinor: 1000,
		WagesMinor:   24000,
		StorageMinor: 150,
		Surveys:      2,
		TotalSeconds: 180,
	}
	pg := &fakeAggregator{agg: agg, imported: 100}
	spender := service.NewSpendReport(pg, tariffs, def)

	bd, err := spender.MonthSpend(t.Context(), uuid.New(), nil, billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(1000), bd.TelecomMin)
	require.Equal(t, int64(24000), bd.WagesMin)
	require.Equal(t, int64(150), bd.StorageMin)
	require.Equal(t, int64(50*100), bd.RespondentBasesMin) // 5000
	require.Equal(t, int64(500000), bd.FixedFeeMin)
	require.Equal(t, int64(2), bd.CompletedSurveys)
	require.Equal(t, int64(180), bd.TotalCallSeconds)
	require.Equal(t, int64(1000+24000+150+5000+500000), bd.TotalMin)
	// CostPerSurveyMinor = TotalMin / CompletedSurveys.
	require.Equal(t, bd.TotalMin/2, bd.CostPerSurveyMinor())
}

// TestMonthSpend_InvalidPeriod verifies the three invalid-period branches:
// zero From, From == To, and From > To. All must map to ErrInvalidPeriod
// so the HTTP boundary returns 400.
func TestMonthSpend_InvalidPeriod(t *testing.T) {
	t.Parallel()
	spender := service.NewSpendReport(&fakeAggregator{},
		service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{}),
		billingapi.Tariffs{})

	_, err := spender.MonthSpend(t.Context(), uuid.New(), nil, billingapi.Period{})
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)

	now := time.Now().UTC()
	_, err = spender.MonthSpend(t.Context(), uuid.New(), nil, billingapi.Period{From: now, To: now}) // From == To
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)

	_, err = spender.MonthSpend(t.Context(), uuid.New(), nil, billingapi.Period{From: now.Add(time.Hour), To: now}) // From > To
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)
}

// TestMonthSpend_NoTariffs_FallsBack covers the ErrNoTariffs fallback path:
// a tenant with no configured tariffs uses BillingConfig.Defaults. The
// breakdown must reflect the default FixedFeesMinor and RespondentBasesMinor
// rather than zero.
func TestMonthSpend_NoTariffs_FallsBack(t *testing.T) {
	t.Parallel()
	def := billingapi.Tariffs{FixedFeesMinor: 100000, RespondentBasesMinor: 10}
	tariffs := service.NewTariffStore(newFakeBackend(), def)
	pg := &fakeAggregator{imported: 50}
	spender := service.NewSpendReport(pg, tariffs, def)

	bd, err := spender.MonthSpend(t.Context(), uuid.New(), nil, billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(100000), bd.FixedFeeMin)
	require.Equal(t, int64(500), bd.RespondentBasesMin) // 10 * 50
}

// TestMonthSpend_AggregatorError_Wrapped verifies a backend failure is
// surfaced unchanged via %w wrapping so the boundary can errors.Is on the
// original sentinel.
func TestMonthSpend_AggregatorError_Wrapped(t *testing.T) {
	t.Parallel()
	aggErr := errors.New("simulated db failure")
	pg := &fakeAggregator{err: aggErr}
	tariffs := service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{})
	spender := service.NewSpendReport(pg, tariffs, billingapi.Tariffs{})

	_, err := spender.MonthSpend(t.Context(), uuid.New(), nil, billingapi.Month(2026, time.May))
	require.Error(t, err)
	require.ErrorIs(t, err, aggErr)
}

// TestSpendByMonth_TrailingCount verifies that SpendByMonth returns the
// trailing N months in oldest-first order ending with the clock's current
// month. The fixed clock makes the assertion deterministic.
func TestSpendByMonth_TrailingCount(t *testing.T) {
	t.Parallel()
	pg := &fakeAggregator{agg: billingpgx.CallCostsAggregate{TelecomMinor: 100, WagesMinor: 200}}
	tariffs := service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{})
	// Pin clock to 2026-05-15 so the trailing months are deterministic.
	fixed := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	spender := service.NewSpendReportWithClock(pg, tariffs, billingapi.Tariffs{}, func() time.Time { return fixed })

	series, err := spender.SpendByMonth(t.Context(), uuid.New(), 3)
	require.NoError(t, err)
	require.Len(t, series, 3)
	// Oldest first: March, April, May.
	require.Equal(t, time.March, series[0].Period.From.Month())
	require.Equal(t, time.April, series[1].Period.From.Month())
	require.Equal(t, time.May, series[2].Period.From.Month())
}

// TestSpendByMonth_Day31_NoMonthSkip is a regression test for the calendar-
// normalisation gotcha caught in Step E review: time.AddDate(0,-1,0) on
// March 31 normalises to March 3 (since Feb only has 28 days), which would
// have skipped February and double-counted March. The implementation snaps
// the anchor to day-1-of-month before iterating, so this test asserts
// trailing 3 months ending January 31 == Nov + Dec + Jan and trailing 3
// months ending March 31 == Jan + Feb + Mar.
func TestSpendByMonth_Day31_NoMonthSkip(t *testing.T) {
	t.Parallel()
	pg := &fakeAggregator{}
	tariffs := service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{})

	// Day-31 in January (Feb has 28/29 days — naive AddDate would land in
	// March, skipping February).
	jan31 := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	s := service.NewSpendReportWithClock(pg, tariffs, billingapi.Tariffs{}, func() time.Time { return jan31 })
	series, err := s.SpendByMonth(t.Context(), uuid.New(), 3)
	require.NoError(t, err)
	require.Len(t, series, 3)
	require.Equal(t, time.November, series[0].Period.From.Month())
	require.Equal(t, time.December, series[1].Period.From.Month())
	require.Equal(t, time.January, series[2].Period.From.Month())

	// Day-31 in March — naive AddDate would skip February.
	mar31 := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	s = service.NewSpendReportWithClock(pg, tariffs, billingapi.Tariffs{}, func() time.Time { return mar31 })
	series, err = s.SpendByMonth(t.Context(), uuid.New(), 3)
	require.NoError(t, err)
	require.Equal(t, time.January, series[0].Period.From.Month())
	require.Equal(t, time.February, series[1].Period.From.Month())
	require.Equal(t, time.March, series[2].Period.From.Month())
}

// TestSpendByMonth_InvalidCount verifies the count-range guard: 0, negative,
// and > 24 all map to ErrInvalidPeriod (the HTTP boundary returns 400).
func TestSpendByMonth_InvalidCount(t *testing.T) {
	t.Parallel()
	spender := service.NewSpendReport(&fakeAggregator{},
		service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{}),
		billingapi.Tariffs{})

	_, err := spender.SpendByMonth(t.Context(), uuid.New(), 0)
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)
	_, err = spender.SpendByMonth(t.Context(), uuid.New(), 25)
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)
	_, err = spender.SpendByMonth(t.Context(), uuid.New(), -1)
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)
}

// TestSpendByMonth_BoundaryCount24 verifies the upper boundary count=24 is
// allowed (just below the rejection threshold) and yields 24 rows.
func TestSpendByMonth_BoundaryCount24(t *testing.T) {
	t.Parallel()
	pg := &fakeAggregator{}
	tariffs := service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{})
	fixed := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	spender := service.NewSpendReportWithClock(pg, tariffs, billingapi.Tariffs{}, func() time.Time { return fixed })

	series, err := spender.SpendByMonth(t.Context(), uuid.New(), 24)
	require.NoError(t, err)
	require.Len(t, series, 24)
}

// TestNewSpendReportWithClock_NilClockFallsBack ensures the test-only
// constructor accepts a nil clock and falls back to time.Now.
func TestNewSpendReportWithClock_NilClockFallsBack(t *testing.T) {
	t.Parallel()
	pg := &fakeAggregator{}
	tariffs := service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{})
	spender := service.NewSpendReportWithClock(pg, tariffs, billingapi.Tariffs{}, nil)
	// Compute trailing 1 month — should not panic, should return 1 row.
	series, err := spender.SpendByMonth(t.Context(), uuid.New(), 1)
	require.NoError(t, err)
	require.Len(t, series, 1)
}
