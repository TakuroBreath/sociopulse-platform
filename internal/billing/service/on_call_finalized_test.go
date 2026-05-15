package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
)

// fakeSink is an in-memory service.CostSink stand-in for unit tests. It
// replays the production store's idempotency contract (a second insert of
// the same call_id returns inserted=false) without touching Postgres.
type fakeSink struct {
	rows     []billingpgx.CallCostRow
	inserted []bool
	err      error
}

// InsertCallCost mirrors the production store: returns (true, nil) on a
// fresh call_id, (false, nil) on a duplicate, and the configured err
// (with inserted=false) for the simulated-failure tests.
func (f *fakeSink) InsertCallCost(_ context.Context, row billingpgx.CallCostRow) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	// Idempotency: if a row with this call_id was previously recorded,
	// return (false, nil) — same shape as ON CONFLICT (call_id) DO NOTHING.
	for _, r := range f.rows {
		if r.CallID == row.CallID {
			f.rows = append(f.rows, row)
			f.inserted = append(f.inserted, false)
			return false, nil
		}
	}
	f.rows = append(f.rows, row)
	f.inserted = append(f.inserted, true)
	return true, nil
}

// baseInput returns a "success / 60s" CallCostInput with fresh UUIDs. Tests
// override fields as needed to keep table-driven cases focused.
func baseInput() billingapi.CallCostInput {
	return billingapi.CallCostInput{
		CallID:      uuid.New(),
		TenantID:    uuid.New(),
		ProjectID:   uuid.New(),
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 60,
		Status:      "success",
		FinalizedAt: time.Now().UTC(),
	}
}

// TestOnCallFinalized_HappyPath exercises the canonical wage+telecom path:
// the calculator's three line items land in the persisted row and
// TotalMinor is the exact sum.
func TestOnCallFinalized_HappyPath(t *testing.T) {
	t.Parallel()
	def := billingapi.Tariffs{
		WagePerSurveyMinor: 12000,
		TrunkCostsMinor:    map[string]int64{"mtt-msk-1": 342},
	}
	store := service.NewTariffStore(newFakeBackend(), def)
	sink := &fakeSink{}
	h := service.NewCallFinalizedHandler(
		service.NewCostCalculator(), store, sink, def, zap.NewNop(),
	)

	in := baseInput()
	require.NoError(t, h.OnCallFinalized(t.Context(), in))
	require.Len(t, sink.rows, 1)
	got := sink.rows[0]
	require.Equal(t, in.CallID, got.CallID)
	require.Equal(t, in.TenantID, got.TenantID)
	require.Equal(t, in.ProjectID, got.ProjectID)
	require.Equal(t, in.TrunkUsed, got.TrunkUsed)
	require.Equal(t, in.DurationSec, got.DurationSec)
	require.Equal(t, in.Status, got.Status)
	require.Equal(t, int64(342), got.TelecomMinor)
	require.Equal(t, int64(12000), got.WagesMinor)
	require.Equal(t, int64(0), got.StorageMinor)
	require.Equal(t, int64(342+12000), got.TotalMinor)
	require.Equal(t, in.FinalizedAt, got.FinalizedAt)
}

// TestOnCallFinalized_NoTariffs_FallsBackToDefaults verifies the
// ErrNoTariffs branch: a tenant that has never PATCHed its tariffs uses
// the injected BillingConfig.Defaults, and the persisted TariffVersion
// stays nil to signal "no admin-edit yet" to future audit / drift jobs.
func TestOnCallFinalized_NoTariffs_FallsBackToDefaults(t *testing.T) {
	t.Parallel()
	def := billingapi.Tariffs{
		WagePerSurveyMinor: 11000,
		TrunkCostsMinor:    map[string]int64{"mango-fed": 378},
	}
	store := service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{})
	sink := &fakeSink{}
	h := service.NewCallFinalizedHandler(
		service.NewCostCalculator(), store, sink, def, zap.NewNop(),
	)

	in := baseInput()
	in.TrunkUsed = "mango-fed"
	require.NoError(t, h.OnCallFinalized(t.Context(), in))
	require.Len(t, sink.rows, 1)
	got := sink.rows[0]
	require.Equal(t, int64(11000), got.WagesMinor)
	require.Equal(t, int64(378), got.TelecomMinor)
	require.Nil(t, got.TariffVersion, "no admin edit yet → tariff_version NULL")
}

// TestOnCallFinalized_IdempotentRedelivery exercises the NATS at-least-once
// invariant: publishing the same dialer.call.finalized event twice yields
// exactly one persisted call_cost row. The hook must NOT return an error
// on the duplicate — that would trigger another redelivery loop.
func TestOnCallFinalized_IdempotentRedelivery(t *testing.T) {
	t.Parallel()
	def := billingapi.Tariffs{WagePerSurveyMinor: 12000}
	store := service.NewTariffStore(newFakeBackend(), def)
	sink := &fakeSink{}
	h := service.NewCallFinalizedHandler(
		service.NewCostCalculator(), store, sink, def, zap.NewNop(),
	)

	in := baseInput()
	require.NoError(t, h.OnCallFinalized(t.Context(), in))
	require.NoError(t, h.OnCallFinalized(t.Context(), in), "redelivery must succeed without error")
	require.Equal(t, []bool{true, false}, sink.inserted)
}

// TestOnCallFinalized_SinkError_ReturnsWrappedError ensures a storage
// failure surfaces as a wrapped error so the NATS handler can NACK and
// trigger redelivery. errors.Is must work through the wrap.
func TestOnCallFinalized_SinkError_ReturnsWrappedError(t *testing.T) {
	t.Parallel()
	def := billingapi.Tariffs{WagePerSurveyMinor: 12000}
	store := service.NewTariffStore(newFakeBackend(), def)
	sinkErr := errors.New("simulated db error")
	sink := &fakeSink{err: sinkErr}
	h := service.NewCallFinalizedHandler(
		service.NewCostCalculator(), store, sink, def, zap.NewNop(),
	)

	err := h.OnCallFinalized(t.Context(), baseInput())
	require.Error(t, err)
	require.ErrorIs(t, err, sinkErr)
}

// TestOnCallFinalized_NegativeDuration_ReturnsError guards the
// calculator's input-validation contract: negative DurationSec wraps
// ErrInvalidPeriod, and the hook must propagate the wrapped sentinel so
// callers can errors.Is-discriminate "calculation broken" from
// "storage broken".
func TestOnCallFinalized_NegativeDuration_ReturnsError(t *testing.T) {
	t.Parallel()
	def := billingapi.Tariffs{WagePerSurveyMinor: 12000}
	store := service.NewTariffStore(newFakeBackend(), def)
	sink := &fakeSink{}
	h := service.NewCallFinalizedHandler(
		service.NewCostCalculator(), store, sink, def, zap.NewNop(),
	)

	in := baseInput()
	in.DurationSec = -1
	err := h.OnCallFinalized(t.Context(), in)
	require.Error(t, err)
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod)
	require.Empty(t, sink.rows, "calculator failure must short-circuit the insert")
}

// TestOnCallFinalized_StoresTariffVersionWhenSet verifies the
// tariff_version column is populated whenever the tenant has explicitly
// PATCHed at least one tariff key (Version >= 1). This is the audit
// trail anchor: a future Step F recompute job can join call_costs
// against the tariff_history snapshot at version N.
func TestOnCallFinalized_StoresTariffVersionWhenSet(t *testing.T) {
	t.Parallel()
	store := service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{})
	sink := &fakeSink{}
	def := billingapi.Tariffs{WagePerSurveyMinor: 12000}
	h := service.NewCallFinalizedHandler(
		service.NewCostCalculator(), store, sink, def, zap.NewNop(),
	)

	tid := uuid.New()
	_, err := store.Update(t.Context(), tid, billingapi.Tariffs{WagePerSurveyMinor: 13000})
	require.NoError(t, err)

	in := baseInput()
	in.TenantID = tid
	require.NoError(t, h.OnCallFinalized(t.Context(), in))
	require.NotNil(t, sink.rows[0].TariffVersion)
	require.Equal(t, 1, *sink.rows[0].TariffVersion)
}

// TestOnCallFinalized_TariffLoadError_NonSentinel_Propagates verifies the
// non-ErrNoTariffs branch — a corrupted JSON in tenant_settings or a
// genuine pg failure must surface as a wrapped error rather than silently
// falling back to defaults (which would mask data corruption).
func TestOnCallFinalized_TariffLoadError_NonSentinel_Propagates(t *testing.T) {
	t.Parallel()
	fake := newFakeBackend()
	tid := uuid.New()
	require.NoError(t, fake.UpsertSettings(context.Background(), tid, map[string][]byte{
		"billing.wage_per_survey": []byte("not-json"),
	}))
	store := service.NewTariffStore(fake, billingapi.Tariffs{})
	sink := &fakeSink{}
	h := service.NewCallFinalizedHandler(
		service.NewCostCalculator(), store, sink, billingapi.Tariffs{}, zap.NewNop(),
	)

	in := baseInput()
	in.TenantID = tid
	err := h.OnCallFinalized(t.Context(), in)
	require.Error(t, err)
	require.Empty(t, sink.rows, "tariff load failure must short-circuit the insert")
}

// TestNewCallFinalizedHandler_PanicsOnNilDeps documents the construction
// contract: nil calc/tariffs/sink is a wiring bug, not a runtime state to
// recover from.
func TestNewCallFinalizedHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	def := billingapi.Tariffs{WagePerSurveyMinor: 12000}
	store := service.NewTariffStore(newFakeBackend(), def)
	sink := &fakeSink{}

	require.Panics(t, func() {
		service.NewCallFinalizedHandler(nil, store, sink, def, zap.NewNop())
	})
	require.Panics(t, func() {
		service.NewCallFinalizedHandler(service.NewCostCalculator(), nil, sink, def, zap.NewNop())
	})
	require.Panics(t, func() {
		service.NewCallFinalizedHandler(service.NewCostCalculator(), store, nil, def, zap.NewNop())
	})
}
