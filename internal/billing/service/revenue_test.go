package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
)

// fakeRevenueBackend is a pure in-memory service.RevenueBackend stand-in.
// Unit tests in this file exercise period validation, error wrapping, and
// arithmetic — the SQL itself is integration-tested in
// store/pgx/revenue_pg_test.go.
type fakeRevenueBackend struct {
	fee        int64
	feeErr     error
	successN   int64
	successErr error
}

func (f *fakeRevenueBackend) ProjectFeePerCompleted(_ context.Context, _, _ uuid.UUID) (int64, error) {
	return f.fee, f.feeErr
}

func (f *fakeRevenueBackend) CountSuccessfulCalls(_ context.Context, _, _ uuid.UUID, _, _ time.Time) (int64, error) {
	return f.successN, f.successErr
}

// TestRevenue_NoContract_ZeroRevenue covers the "project exists but no
// contract attached" branch — fee == 0 returns 0 revenue without querying
// the COUNT path (hot-path optimisation).
func TestRevenue_NoContract_ZeroRevenue(t *testing.T) {
	t.Parallel()
	rc := service.NewRevenueCalculator(&fakeRevenueBackend{fee: 0, successN: 99})
	got, err := rc.MonthRevenue(t.Context(), uuid.New(), uuid.New(), billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}

// TestRevenue_WithContract covers the canonical arithmetic path:
// fee × successful_calls in int64 minor units.
func TestRevenue_WithContract(t *testing.T) {
	t.Parallel()
	rc := service.NewRevenueCalculator(&fakeRevenueBackend{fee: 38100, successN: 4})
	got, err := rc.MonthRevenue(t.Context(), uuid.New(), uuid.New(), billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(38100*4), got)
}

// TestRevenue_DeletedProject_ZeroRevenue covers the missing-project branch.
// pgx.ErrNoRows from the fee lookup is treated as "project deleted/archived
// mid-period" and yields zero revenue rather than failing the entire
// margin report.
func TestRevenue_DeletedProject_ZeroRevenue(t *testing.T) {
	t.Parallel()
	rc := service.NewRevenueCalculator(&fakeRevenueBackend{feeErr: pgxv5.ErrNoRows})
	got, err := rc.MonthRevenue(t.Context(), uuid.New(), uuid.New(), billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}

// TestRevenue_InvalidPeriod verifies the period guard: a zero From maps to
// ErrInvalidPeriod, mirroring the canonical validation rule in
// SpendReport.MonthSpend. The HTTP boundary maps it to 400.
func TestRevenue_InvalidPeriod(t *testing.T) {
	t.Parallel()
	rc := service.NewRevenueCalculator(&fakeRevenueBackend{fee: 1000})
	_, err := rc.MonthRevenue(t.Context(), uuid.New(), uuid.New(), billingapi.Period{})
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)
}

// TestRevenue_InvalidPeriod_FromEqualsTo covers the From == To edge: a
// zero-width period is invalid even though From is non-zero.
func TestRevenue_InvalidPeriod_FromEqualsTo(t *testing.T) {
	t.Parallel()
	rc := service.NewRevenueCalculator(&fakeRevenueBackend{fee: 1000})
	now := time.Now().UTC()
	_, err := rc.MonthRevenue(t.Context(), uuid.New(), uuid.New(), billingapi.Period{From: now, To: now})
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)
}

// TestRevenue_FeeError_Wrapped verifies a backend failure on the fee load
// is surfaced unchanged via %w wrapping so the boundary can errors.Is on
// the original sentinel.
func TestRevenue_FeeError_Wrapped(t *testing.T) {
	t.Parallel()
	feeErr := errors.New("simulated db failure")
	rc := service.NewRevenueCalculator(&fakeRevenueBackend{feeErr: feeErr})
	_, err := rc.MonthRevenue(t.Context(), uuid.New(), uuid.New(), billingapi.Month(2026, time.May))
	require.Error(t, err)
	require.ErrorIs(t, err, feeErr)
}

// TestRevenue_CountError_Wrapped verifies a backend failure on the COUNT
// path is also surfaced wrapped, not swallowed.
func TestRevenue_CountError_Wrapped(t *testing.T) {
	t.Parallel()
	countErr := errors.New("simulated count error")
	rc := service.NewRevenueCalculator(&fakeRevenueBackend{fee: 1000, successErr: countErr})
	_, err := rc.MonthRevenue(t.Context(), uuid.New(), uuid.New(), billingapi.Month(2026, time.May))
	require.Error(t, err)
	require.ErrorIs(t, err, countErr)
}
