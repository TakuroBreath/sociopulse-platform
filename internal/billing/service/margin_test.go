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

// fakeMarginBackend is a pure in-memory service.MarginBackend stand-in.
// Unit tests in this file exercise sort order, margin arithmetic, and
// divide-by-zero safety — the SQL itself is integration-tested in
// store/pgx/revenue_pg_test.go (ListProjectsForPeriod).
type fakeMarginBackend struct {
	rows []billingpgx.ProjectAggregate
	err  error
}

func (f *fakeMarginBackend) ListProjectsForPeriod(_ context.Context, _ uuid.UUID, _, _ time.Time) ([]billingpgx.ProjectAggregate, error) {
	return f.rows, f.err
}

// fakeRevenue is a deterministic billingapi.RevenueCalculator stand-in.
// Keyed by project id so tests can verify the margin layer wires each row
// to the correct project's revenue. The zero-value (no entry in the map)
// returns 0 — matches the production "no contract attached" behaviour.
type fakeRevenue struct {
	fees map[uuid.UUID]int64
}

func (f fakeRevenue) MonthRevenue(_ context.Context, _, projectID uuid.UUID, _ billingapi.Period) (int64, error) {
	return f.fees[projectID], nil
}

// fakeRevenueErr is a RevenueCalculator stand-in that always returns the
// provided error — used to verify error propagation through the margin
// layer.
type fakeRevenueErr struct{ err error }

func (f fakeRevenueErr) MonthRevenue(_ context.Context, _, _ uuid.UUID, _ billingapi.Period) (int64, error) {
	return 0, f.err
}

// TestMargin_SortsByTotalDesc verifies the canonical "top projects" sort:
// the biggest-spend project lands first regardless of input order. Also
// pins the per-row arithmetic: RevenueMin from the injected calculator,
// MarginMin = Revenue − Total, CostPerSrvMn = Total / Surveys.
func TestMargin_SortsByTotalDesc(t *testing.T) {
	t.Parallel()
	bigP, smallP := uuid.New(), uuid.New()
	pg := &fakeMarginBackend{rows: []billingpgx.ProjectAggregate{
		{ProjectID: smallP, ProjectCode: "SML", ProjectName: "Small", Surveys: 2, TotalMinor: 20000},
		{ProjectID: bigP, ProjectCode: "BIG", ProjectName: "Big", Surveys: 4, TotalMinor: 50000},
	}}
	fr := fakeRevenue{fees: map[uuid.UUID]int64{bigP: 100000, smallP: 30000}}
	m := service.NewMarginReport(pg, fr)

	rows, err := m.Margin(t.Context(), uuid.New(), billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "BIG", rows[0].ProjectCode)
	require.Equal(t, int64(100000), rows[0].RevenueMin)
	require.Equal(t, int64(100000-50000), rows[0].MarginMin)
	require.Equal(t, int64(50000/4), rows[0].CostPerSrvMn)
	require.Equal(t, "SML", rows[1].ProjectCode)
}

// TestMargin_NegativeMargin pins a deliberately money-losing project: the
// MarginMin field MUST be allowed to go negative (a project losing money
// is a legitimate state the finance UI needs to surface — sneakily
// clamping to zero would mask the very signal the operator looks for).
func TestMargin_NegativeMargin(t *testing.T) {
	t.Parallel()
	pid := uuid.New()
	pg := &fakeMarginBackend{rows: []billingpgx.ProjectAggregate{
		{ProjectID: pid, ProjectCode: "LOSS", ProjectName: "Loser", Surveys: 1, TotalMinor: 100000},
	}}
	fr := fakeRevenue{fees: map[uuid.UUID]int64{pid: 50000}}
	m := service.NewMarginReport(pg, fr)
	rows, err := m.Margin(t.Context(), uuid.New(), billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(-50000), rows[0].MarginMin)
}

// TestMargin_NoSurveysZeroCostPerSrv verifies the divide-by-zero guard:
// a project with non-zero spend but zero successful surveys (e.g. a
// project that only ran "refused" calls) reports CostPerSrvMn = 0 rather
// than panicking on the int64 divide.
func TestMargin_NoSurveysZeroCostPerSrv(t *testing.T) {
	t.Parallel()
	pid := uuid.New()
	pg := &fakeMarginBackend{rows: []billingpgx.ProjectAggregate{
		{ProjectID: pid, ProjectCode: "EMPTY", ProjectName: "Empty", Surveys: 0, TotalMinor: 1000},
	}}
	fr := fakeRevenue{fees: map[uuid.UUID]int64{}}
	m := service.NewMarginReport(pg, fr)
	rows, err := m.Margin(t.Context(), uuid.New(), billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(0), rows[0].CostPerSrvMn)
}

// TestMargin_BasesMinAlwaysZero pins the design decision documented in
// margin.go: per-project RespondentBasesMin is intentionally zero because
// bases are billed at the tenant grain. A future v2 may decide to prorate;
// this test catches an accidental change.
func TestMargin_BasesMinAlwaysZero(t *testing.T) {
	t.Parallel()
	pid := uuid.New()
	pg := &fakeMarginBackend{rows: []billingpgx.ProjectAggregate{
		{ProjectID: pid, ProjectCode: "X", ProjectName: "X", Surveys: 1, TotalMinor: 1000},
	}}
	m := service.NewMarginReport(pg, fakeRevenue{})
	rows, err := m.Margin(t.Context(), uuid.New(), billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(0), rows[0].RespondentBasesMin)
}

// TestMargin_InvalidPeriod verifies the period guard mirrors the canonical
// validation rule (zero From → 400 at the HTTP boundary).
func TestMargin_InvalidPeriod(t *testing.T) {
	t.Parallel()
	m := service.NewMarginReport(&fakeMarginBackend{}, fakeRevenue{})
	_, err := m.Margin(t.Context(), uuid.New(), billingapi.Period{})
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)
}

// TestMargin_EmptyList verifies the zero-projects path: an empty aggregate
// slice returns an empty (not nil-with-error) result so JSON-encoding
// produces `[]` rather than `null`.
func TestMargin_EmptyList(t *testing.T) {
	t.Parallel()
	m := service.NewMarginReport(&fakeMarginBackend{}, fakeRevenue{})
	rows, err := m.Margin(t.Context(), uuid.New(), billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestMargin_BackendError_Wrapped verifies a store failure on the project
// list is surfaced via %w wrapping (boundary can errors.Is the original
// sentinel).
func TestMargin_BackendError_Wrapped(t *testing.T) {
	t.Parallel()
	listErr := errors.New("simulated db failure")
	m := service.NewMarginReport(&fakeMarginBackend{err: listErr}, fakeRevenue{})
	_, err := m.Margin(t.Context(), uuid.New(), billingapi.Month(2026, time.May))
	require.Error(t, err)
	require.ErrorIs(t, err, listErr)
}

// TestMargin_RevenueError_Wrapped verifies a RevenueCalculator failure
// short-circuits the loop and propagates the wrapped error.
func TestMargin_RevenueError_Wrapped(t *testing.T) {
	t.Parallel()
	pid := uuid.New()
	revErr := errors.New("simulated revenue failure")
	pg := &fakeMarginBackend{rows: []billingpgx.ProjectAggregate{
		{ProjectID: pid, ProjectCode: "X", ProjectName: "X", Surveys: 1, TotalMinor: 1000},
	}}
	m := service.NewMarginReport(pg, fakeRevenueErr{err: revErr})
	_, err := m.Margin(t.Context(), uuid.New(), billingapi.Month(2026, time.May))
	require.Error(t, err)
	require.ErrorIs(t, err, revErr)
}
