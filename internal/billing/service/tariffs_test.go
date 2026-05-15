package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
)

// fakeSettingsBackend is a pure in-memory tenant_settings stand-in. It
// implements service.SettingsBackend without touching Postgres so the unit
// tests in this file are pure-function and run without testcontainers.
type fakeSettingsBackend struct {
	kv map[string]map[string][]byte
}

func newFakeBackend() *fakeSettingsBackend {
	return &fakeSettingsBackend{kv: map[string]map[string][]byte{}}
}

// GetSetting mirrors pkg/postgres semantics: missing (tenant_id, key)
// returns pgx.ErrNoRows so TariffStore can errors.Is-discriminate
// "absent" from "broken".
func (f *fakeSettingsBackend) GetSetting(_ context.Context, tid uuid.UUID, key string) ([]byte, error) {
	m, ok := f.kv[tid.String()]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	v, ok := m[key]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return v, nil
}

// UpsertSettings batches writes for a single tenant — the real PG adapter
// runs the loop inside a Tx; in the fake we just overwrite the map.
func (f *fakeSettingsBackend) UpsertSettings(_ context.Context, tid uuid.UUID, kv map[string][]byte) error {
	if _, ok := f.kv[tid.String()]; !ok {
		f.kv[tid.String()] = map[string][]byte{}
	}
	for k, v := range kv {
		f.kv[tid.String()][k] = v
	}
	return nil
}

// TestTariffStore_Get_NoTariffs_ReturnsErrNoTariffs verifies the "tenant
// has set zero keys" branch returns the canonical sentinel — the boundary
// (HTTP / RBAC) maps this to 409.
func TestTariffStore_Get_NoTariffs_ReturnsErrNoTariffs(t *testing.T) {
	t.Parallel()
	s := service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{})
	_, err := s.Get(t.Context(), uuid.New())
	require.ErrorIs(t, err, billingapi.ErrNoTariffs)
}

// TestTariffStore_Update_RoundTrip writes a tariff snapshot, then reads it
// back. Asserts version bump (0→1), tenant id stamping, and UpdatedAt set.
func TestTariffStore_Update_RoundTrip(t *testing.T) {
	t.Parallel()
	fake := newFakeBackend()
	s := service.NewTariffStore(fake, billingapi.Tariffs{})
	tid := uuid.New()

	in := billingapi.Tariffs{
		TrunkCostsMinor:    map[string]int64{"mtt-msk-1": 342},
		WagePerSurveyMinor: 12000,
	}
	updated, err := s.Update(t.Context(), tid, in)
	require.NoError(t, err)
	require.Equal(t, 1, updated.Version) // bumped from 0
	require.Equal(t, tid, updated.TenantID)
	require.False(t, updated.UpdatedAt.IsZero())

	got, err := s.Get(t.Context(), tid)
	require.NoError(t, err)
	require.Equal(t, int64(342), got.TrunkCostsMinor["mtt-msk-1"])
	require.Equal(t, int64(12000), got.WagePerSurveyMinor)
	require.Equal(t, updated.Version, got.Version)
	require.Equal(t, tid, got.TenantID)
}

// TestTariffStore_Update_BumpsVersionMonotonically verifies version is
// always (current+1) — the audit trail invariant. Two consecutive updates
// produce 1 then 2; Get returns the most recent.
func TestTariffStore_Update_BumpsVersionMonotonically(t *testing.T) {
	t.Parallel()
	fake := newFakeBackend()
	s := service.NewTariffStore(fake, billingapi.Tariffs{})
	tid := uuid.New()

	first, err := s.Update(t.Context(), tid, billingapi.Tariffs{WagePerSurveyMinor: 10000})
	require.NoError(t, err)
	require.Equal(t, 1, first.Version)

	second, err := s.Update(t.Context(), tid, billingapi.Tariffs{WagePerSurveyMinor: 11000})
	require.NoError(t, err)
	require.Equal(t, 2, second.Version)

	got, err := s.Get(t.Context(), tid)
	require.NoError(t, err)
	require.Equal(t, 2, got.Version)
	require.Equal(t, int64(11000), got.WagePerSurveyMinor)
}

// TestTariffStore_Update_InvalidRejected_WrapsErrInvalidTariff guards the
// validation chain: Tariffs.Validate wraps ErrInvalidTariff for negative
// scalars, and Update must propagate the wrapped sentinel unchanged so
// callers can errors.Is it.
func TestTariffStore_Update_InvalidRejected_WrapsErrInvalidTariff(t *testing.T) {
	t.Parallel()
	s := service.NewTariffStore(newFakeBackend(), billingapi.Tariffs{})
	_, err := s.Update(t.Context(), uuid.New(), billingapi.Tariffs{
		WagePerSurveyMinor: -1,
	})
	require.ErrorIs(t, err, billingapi.ErrInvalidTariff)
}

// TestTariffStore_Get_PartialKeysFallBackToDefault models the "tenant set
// only some keys" scenario. Unset keys must fall back to the injected
// default snapshot rather than zero — that's how a new tenant inherits
// the platform defaults from BillingConfig.
func TestTariffStore_Get_PartialKeysFallBackToDefault(t *testing.T) {
	t.Parallel()
	fake := newFakeBackend()
	def := billingapi.Tariffs{FixedFeesMinor: 5_000_00, RespondentBasesMinor: 50}
	s := service.NewTariffStore(fake, def)
	tid := uuid.New()

	// Tenant set only the wage_per_survey key.
	require.NoError(t, fake.UpsertSettings(t.Context(), tid, map[string][]byte{
		"billing.wage_per_survey": mustJSON(t, struct {
			Value int64 `json:"value"`
		}{13_500}),
	}))

	got, err := s.Get(t.Context(), tid)
	require.NoError(t, err)
	require.Equal(t, int64(13_500), got.WagePerSurveyMinor)
	// Falls back to default for unset keys:
	require.Equal(t, int64(5_000_00), got.FixedFeesMinor)
	require.Equal(t, int64(50), got.RespondentBasesMinor)
}

// TestTariffStore_Get_CorruptedJSONReturnsError ensures unparseable JSON in
// tenant_settings does not crash the calculator silently — Get must
// surface the parse error so the operator can fix it.
func TestTariffStore_Get_CorruptedJSONReturnsError(t *testing.T) {
	t.Parallel()
	fake := newFakeBackend()
	s := service.NewTariffStore(fake, billingapi.Tariffs{})
	tid := uuid.New()
	require.NoError(t, fake.UpsertSettings(t.Context(), tid, map[string][]byte{
		"billing.wage_per_survey": []byte("not-json"),
	}))
	_, err := s.Get(t.Context(), tid)
	require.Error(t, err)
}

// TestTariffStore_Update_PreservesTrunkCostsMap verifies the map-shaped
// trunk costs round-trip correctly (the key/value pair is the largest
// JSON payload in tenant_settings.billing.*).
func TestTariffStore_Update_PreservesTrunkCostsMap(t *testing.T) {
	t.Parallel()
	fake := newFakeBackend()
	s := service.NewTariffStore(fake, billingapi.Tariffs{})
	tid := uuid.New()

	in := billingapi.Tariffs{
		TrunkCostsMinor: map[string]int64{
			"mtt-msk-1":   342,
			"mango-fed":   378,
			"beeline-srf": 412,
		},
	}
	_, err := s.Update(t.Context(), tid, in)
	require.NoError(t, err)

	got, err := s.Get(t.Context(), tid)
	require.NoError(t, err)
	require.Equal(t, int64(342), got.TrunkCostsMinor["mtt-msk-1"])
	require.Equal(t, int64(378), got.TrunkCostsMinor["mango-fed"])
	require.Equal(t, int64(412), got.TrunkCostsMinor["beeline-srf"])
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
