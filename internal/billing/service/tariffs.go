package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// SettingsBackend is the narrow adapter interface TariffStore needs from
// the persistence layer. Production is *store/pgx.PG (pgx-backed,
// tenant-scoped via *postgres.Pool.WithTenant); tests use an in-memory
// fake. The interface lives here, in the consumer package, per the
// "accept interfaces, return structs" project convention.
type SettingsBackend interface {
	// GetSetting fetches one tenant_settings row by (tenant_id, key).
	// Returns pgx.ErrNoRows when the row is absent so callers can
	// errors.Is-discriminate "missing" from "broken".
	GetSetting(ctx context.Context, tenantID uuid.UUID, key string) ([]byte, error)
	// UpsertSettings writes multiple keys for a single tenant atomically
	// (single Tx in the PG adapter). Partial-write semantics on error.
	UpsertSettings(ctx context.Context, tenantID uuid.UUID, kv map[string][]byte) error
}

// tenant_settings.key values owned by the billing module. Each scalar
// field is wrapped as {"value": <number>} for JSON-shape consistency
// (see plan-14 §2.4 — keeping value as an object rather than a bare
// number plays nicer with tenant_settings.value::jsonb queries).
const (
	keyTrunks          = "billing.trunks"
	keyWagePerSurvey   = "billing.wage_per_survey"
	keyRespondentBases = "billing.respondent_bases"
	keyStorage         = "billing.storage"
	keyFixed           = "billing.fixed"
	keyVersion         = "billing.version"
)

// tariffStore persists per-tenant Tariffs into the canonical
// tenant_settings table. Reads merge stored keys onto a default snapshot
// (BillingConfig.Defaults) so a new tenant inherits the platform defaults
// without writes. Writes bump Version monotonically.
type tariffStore struct {
	backend SettingsBackend
	def     billingapi.Tariffs
}

// NewTariffStore wires a backend and a default Tariffs snapshot. The
// default fills any keys the tenant hasn't set yet. Complete absence of
// every key returns ErrNoTariffs (so the boundary can map it to HTTP
// 409 / "tenant not yet configured").
func NewTariffStore(b SettingsBackend, def billingapi.Tariffs) billingapi.TariffStore {
	return &tariffStore{backend: b, def: def}
}

// Compile-time interface guard: any signature drift in billingapi.TariffStore
// breaks the build here rather than at the call site.
var _ billingapi.TariffStore = (*tariffStore)(nil)

// Get merges per-key reads onto the injected default. Returns
// ErrNoTariffs when the tenant has set zero keys. Any read or parse
// failure other than pgx.ErrNoRows propagates wrapped with %w so the
// caller can errors.Is-discriminate.
func (s *tariffStore) Get(ctx context.Context, tid uuid.UUID) (billingapi.Tariffs, error) {
	// Start from the configured default so unset keys fall back cleanly.
	// Defensively clone the map so callers can't mutate our default by
	// holding the returned snapshot.
	t := s.def
	t.TenantID = tid
	t.TrunkCostsMinor = cloneMap(s.def.TrunkCostsMinor)

	have := false

	// billing.trunks — map shape, NOT the scalar {"value": x} wrapper.
	if raw, err := s.backend.GetSetting(ctx, tid, keyTrunks); err == nil {
		var v map[string]int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return billingapi.Tariffs{}, fmt.Errorf("billing: parse %s: %w", keyTrunks, err)
		}
		t.TrunkCostsMinor = v
		have = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return billingapi.Tariffs{}, fmt.Errorf("billing: load %s: %w", keyTrunks, err)
	}

	if v, ok, err := readInt64Field(ctx, s.backend, tid, keyWagePerSurvey); err != nil {
		return billingapi.Tariffs{}, err
	} else if ok {
		t.WagePerSurveyMinor = v
		have = true
	}
	if v, ok, err := readInt64Field(ctx, s.backend, tid, keyRespondentBases); err != nil {
		return billingapi.Tariffs{}, err
	} else if ok {
		t.RespondentBasesMinor = v
		have = true
	}
	if v, ok, err := readInt64Field(ctx, s.backend, tid, keyStorage); err != nil {
		return billingapi.Tariffs{}, err
	} else if ok {
		t.StorageMinorPerGBMo = v
		have = true
	}
	if v, ok, err := readInt64Field(ctx, s.backend, tid, keyFixed); err != nil {
		return billingapi.Tariffs{}, err
	} else if ok {
		t.FixedFeesMinor = v
		have = true
	}

	if v, ok, err := readIntField(ctx, s.backend, tid, keyVersion); err != nil {
		return billingapi.Tariffs{}, err
	} else if ok {
		t.Version = v
		have = true
	}

	if !have {
		return billingapi.Tariffs{}, billingapi.ErrNoTariffs
	}
	return t, nil
}

// Update validates the incoming snapshot, bumps Version monotonically, and
// persists every key in a single batch. The previous-version read is
// tolerated to be ErrNoTariffs (first write for a tenant).
//
// Tariffs.Validate already wraps ErrInvalidTariff for negative scalars; we
// propagate that wrapped error unchanged so callers can errors.Is the
// sentinel.
func (s *tariffStore) Update(ctx context.Context, tid uuid.UUID, in billingapi.Tariffs) (billingapi.Tariffs, error) {
	if err := in.Validate(); err != nil {
		return billingapi.Tariffs{}, err // already wraps ErrInvalidTariff
	}
	// Load current version so the next write is current+1. A fresh tenant
	// (ErrNoTariffs) starts at version 1 — curr.Version is the zero value.
	curr, err := s.Get(ctx, tid)
	if err != nil && !errors.Is(err, billingapi.ErrNoTariffs) {
		return billingapi.Tariffs{}, err
	}
	in.TenantID = tid
	in.Version = curr.Version + 1
	in.UpdatedAt = time.Now().UTC()

	kv := map[string][]byte{}
	if in.TrunkCostsMinor != nil {
		b, err := json.Marshal(in.TrunkCostsMinor)
		if err != nil {
			return billingapi.Tariffs{}, fmt.Errorf("billing: marshal trunks: %w", err)
		}
		kv[keyTrunks] = b
	}
	kv[keyWagePerSurvey] = mustMarshalInt64(in.WagePerSurveyMinor)
	kv[keyRespondentBases] = mustMarshalInt64(in.RespondentBasesMinor)
	kv[keyStorage] = mustMarshalInt64(in.StorageMinorPerGBMo)
	kv[keyFixed] = mustMarshalInt64(in.FixedFeesMinor)
	kv[keyVersion] = mustMarshalInt(in.Version)

	if err := s.backend.UpsertSettings(ctx, tid, kv); err != nil {
		return billingapi.Tariffs{}, fmt.Errorf("billing: upsert settings: %w", err)
	}
	return in, nil
}

// readInt64Field reads a scalar wrapped as {"value": int64}. ok==false
// means the key was absent (pgx.ErrNoRows); err is set for parse / IO
// failure.
func readInt64Field(ctx context.Context, b SettingsBackend, tid uuid.UUID, key string) (int64, bool, error) {
	raw, err := b.GetSetting(ctx, tid, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("billing: load %s: %w", key, err)
	}
	var v struct {
		Value int64 `json:"value"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false, fmt.Errorf("billing: parse %s: %w", key, err)
	}
	return v.Value, true, nil
}

// readIntField is the int-variant convenience wrapper around
// readInt64Field. The wire shape is still {"value": <number>}.
func readIntField(ctx context.Context, b SettingsBackend, tid uuid.UUID, key string) (int, bool, error) {
	v, ok, err := readInt64Field(ctx, b, tid, key)
	if err != nil {
		return 0, false, err
	}
	return int(v), ok, nil
}

// cloneMap returns a shallow copy of m (or nil for a nil input). Used to
// prevent callers from mutating the injected default tariffs map by
// holding a returned snapshot.
func cloneMap(m map[string]int64) map[string]int64 {
	if m == nil {
		return nil
	}
	out := make(map[string]int64, len(m))
	maps.Copy(out, m)
	return out
}

// mustMarshalInt64 encodes a scalar as {"value": <n>}. Marshaling int64
// into json.Marshal cannot fail; the defensive panic catches a future
// drift in stdlib semantics rather than a runtime bug.
func mustMarshalInt64(v int64) []byte {
	b, err := json.Marshal(struct {
		V int64 `json:"value"`
	}{v})
	if err != nil {
		panic(err) // marshaling int64 cannot fail; defensive
	}
	return b
}

func mustMarshalInt(v int) []byte {
	return mustMarshalInt64(int64(v))
}
