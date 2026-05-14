package dialer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/capacity"
	"github.com/sociopulse/platform/internal/dialer/hours"
	"github.com/sociopulse/platform/internal/dialer/retry"
	dialertnats "github.com/sociopulse/platform/internal/dialer/transport/nats"
	"github.com/sociopulse/platform/internal/modules"
	telephonyapi "github.com/sociopulse/platform/internal/telephony/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Locator keys this module CONSUMES (registered by other modules).
const (
	locatorTenancy          = "tenancy.Tenancy"
	locatorKMSResolver      = "tenancy.KMSResolver"
	locatorCommandPublisher = telephonyLocatorPublisher
)

// telephonyLocatorPublisher mirrors telephony.LocatorCommandPublisher
// without forcing the dialer module to import the telephony package's
// non-api code. The string is stable across modules; if either side
// changes, locator.Lookup returns ok=false and the dialer falls back
// to a stub publisher (with a warn log).
const telephonyLocatorPublisher = "telephony.CommandPublisher"

// settingsLookupAdapter adapts tenancy.SettingsCache to the small
// hours.SettingsLookup surface. We do the json.RawMessage conversion
// inline so the hours package doesn't depend on the full
// tenancy.SettingValue type.
type settingsLookupAdapter struct {
	cache tenancyapi.SettingsCache
}

// Compile-time interface check.
var _ hours.SettingsLookup = (*settingsLookupAdapter)(nil)

// Lookup satisfies hours.SettingsLookup. tenancy.ErrNotFound translates
// to ok=false, which the hours package treats as "no override, use the
// default". Other errors propagate.
func (a *settingsLookupAdapter) Lookup(ctx context.Context, tenantID uuid.UUID, key string) (json.RawMessage, bool, error) {
	if a == nil || a.cache == nil {
		return nil, false, nil
	}
	v, err := a.cache.Lookup(ctx, tenantID, key)
	if err != nil {
		if errors.Is(err, tenancyapi.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return v.Raw(), true, nil
}

// noopSettingsLookup is the fallback when tenancy.Tenancy is missing
// from the locator (worker-only boot or dev test). It always returns
// ok=false so hours.Checker uses the platform default for every
// tenant.
type noopSettingsLookup struct{}

// Compile-time interface check.
var _ hours.SettingsLookup = noopSettingsLookup{}

// Lookup satisfies hours.SettingsLookup with a hard-coded "no
// override" answer. Operators see the package default 09-21 weekday /
// 10-18 weekend, which is conservative for the v1 rollout.
func (noopSettingsLookup) Lookup(_ context.Context, _ uuid.UUID, _ string) (json.RawMessage, bool, error) {
	return nil, false, nil
}

// kmsDecryptorAdapter adapts tenancy.KMSResolver to the small
// retry.Decryptor surface used by the orchestrator.
type kmsDecryptorAdapter struct {
	kms tenancyapi.KMSResolver
}

// dialerRespondentPhoneAADScope mirrors the crm.respondent.phone scope
// used by RespondentService at encrypt time. Both sides must agree on
// this string or the AEAD AAD bind fails (Plan 13.2.5 Task 6). Kept
// duplicated here rather than imported from crm/service to keep the
// dialer module's depguard surface clean.
const dialerRespondentPhoneAADScope = "crm.respondent.phone"

// Compile-time interface check.
var _ retry.Decryptor = (*kmsDecryptorAdapter)(nil)

// Decrypt satisfies retry.Decryptor. The orchestrator passes the
// per-row tenant ID + respondent ID + ciphertext; we forward to the
// KMS resolver after stamping the canonical respondent-phone scope.
func (a *kmsDecryptorAdapter) Decrypt(ctx context.Context, tenantID, respondentID uuid.UUID, ciphertext []byte) ([]byte, error) {
	if a == nil || a.kms == nil {
		return nil, errors.New("dialer: KMSResolver not wired (decryptor unavailable)")
	}
	return a.kms.Decrypt(ctx, tenantID, dialerRespondentPhoneAADScope, respondentID.String(), ciphertext)
}

// passthroughDecryptor is the fallback retry.Decryptor used when
// tenancy.KMSResolver is unavailable (worker-only smoke tests; pre-
// Plan 04 boot sequence). It returns the ciphertext bytes verbatim,
// matching the dev/test behaviour where phone columns are stored as
// plaintext. NOT for production: the retry orchestrator's logger
// surfaces the fallback choice on construction.
type passthroughDecryptor struct{}

// Compile-time interface check.
var _ retry.Decryptor = passthroughDecryptor{}

// Decrypt satisfies retry.Decryptor.
func (passthroughDecryptor) Decrypt(_ context.Context, _, _ uuid.UUID, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("dialer/retry: empty ciphertext")
	}
	out := make([]byte, len(ciphertext))
	copy(out, ciphertext)
	return out, nil
}

// stubCapacityPool is a Pool adapter that surfaces an empty healthy-node
// list. Used when telephony.LocatorCommandPublisher is the stub one
// (cmd/api boot without the bridge wired): the dialer's capacity
// tracker degrades to "no nodes available" on every Acquire call,
// which the dispatch loop translates into ErrAllNodesFull and backs
// off. Better than panicking on a missing Pool.
type stubCapacityPool struct{}

// Compile-time interface check.
var _ capacity.Pool = stubCapacityPool{}

// HealthyNodes satisfies capacity.Pool with a constant empty list.
func (stubCapacityPool) HealthyNodes() []string { return nil }

// stubBackpressure is a Bp adapter that always reports the node at
// cap. Pairs with stubCapacityPool so cmd/api can boot the dialer
// without the bridge — every Acquire call surfaces ErrAllNodesFull
// before reaching this stub, but the interface needs satisfying so
// the tracker construction doesn't fail.
type stubBackpressure struct{}

// Compile-time interface check.
var _ capacity.Bp = stubBackpressure{}

// TryAcquire satisfies capacity.Bp. Always returns (false, nil)
// — the upstream tracker treats this as "node at cap" and tries the
// next, eventually surfacing ErrAllNodesFull.
func (stubBackpressure) TryAcquire(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// Release satisfies capacity.Bp. No-op.
func (stubBackpressure) Release(_ context.Context, _ string) error { return nil }

// Get satisfies capacity.Bp. Returns a constant 0 — the tracker uses
// this to refresh per-node gauges on Acquire / Release; with no real
// telephony backend the gauges stay at 0.
func (stubBackpressure) Get(_ context.Context, _ string) (int, error) { return 0, nil }

// Cap satisfies capacity.Bp. The stub returns 0 so the tracker
// reports zero remaining capacity to anyone reading the metrics.
func (stubBackpressure) Cap() int { return 0 }

// stubEventConsumer satisfies telephony.api.EventConsumer with a
// no-op Subscribe. Returned when the locator has no real telephony
// EventConsumer — happens in cmd/api today (the bridge runs in
// cmd/telephony-bridge and Plan 11 will register the cluster
// consumer). The unsubscribe is also a no-op so the dialer's caller
// can defer it without nil-checking.
type stubEventConsumer struct {
	logger *zap.Logger
}

// Compile-time interface check.
var _ telephonyapi.EventConsumer = (*stubEventConsumer)(nil)

// Subscribe satisfies telephony.api.EventConsumer with a logged
// no-op. The subscription is "alive" until the unsubscribe is
// invoked; no events are ever delivered.
func (s *stubEventConsumer) Subscribe(_ context.Context, tenantID uuid.UUID, _ telephonyapi.EventHandler) (func(), error) {
	if s.logger != nil {
		s.logger.Debug("dialer: stub EventConsumer.Subscribe (no real bridge consumer wired)",
			zap.Stringer("tenant_id", tenantID),
		)
	}
	return func() {}, nil
}

// lookupTenancy pulls the aggregate tenancy.Tenancy interface from the
// locator. Returns nil (not error) when missing — the caller swaps in
// the noopSettingsLookup fallback.
func lookupTenancy(loc modules.ServiceLocator, log *zap.Logger) tenancyapi.Tenancy {
	if loc == nil {
		return nil
	}
	raw, ok := loc.Lookup(locatorTenancy)
	if !ok {
		return nil
	}
	t, ok := raw.(tenancyapi.Tenancy)
	if !ok {
		log.Error("tenancy.Tenancy registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil
	}
	return t
}

// lookupKMSResolver pulls tenancy.KMSResolver from the locator.
// Returns nil when missing or wrong-typed; the caller swaps in
// passthroughDecryptor as a fallback.
func lookupKMSResolver(loc modules.ServiceLocator, log *zap.Logger) tenancyapi.KMSResolver {
	if loc == nil {
		return nil
	}
	raw, ok := loc.Lookup(locatorKMSResolver)
	if !ok {
		return nil
	}
	r, ok := raw.(tenancyapi.KMSResolver)
	if !ok {
		log.Error("tenancy.KMSResolver registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil
	}
	return r
}

// lookupCommandPublisher pulls telephony.CommandPublisher from the
// locator. The dialer uses this as the destination for Originate /
// Hangup; cmd/api today registers a stub that returns
// ErrTelephonyBridgeOffline on every call. Plan 11 will register a
// real *nats.Conn-backed publisher.
//
// Returns ok=false when missing; the caller bails on dialer router
// construction (the router truly cannot operate without a publisher
// — even the stub is preferable to a nil deref).
func lookupCommandPublisher(loc modules.ServiceLocator, log *zap.Logger) (telephonyapi.CommandPublisher, bool) {
	if loc == nil {
		return nil, false
	}
	raw, ok := loc.Lookup(locatorCommandPublisher)
	if !ok {
		return nil, false
	}
	p, ok := raw.(telephonyapi.CommandPublisher)
	if !ok {
		log.Error("telephony.CommandPublisher registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil, false
	}
	return p, true
}

// Compile-time check that dialerapi.OperatorFSM is a strict subset of
// the interface the FSM Machine implements — keeps the locator-key
// type matching honest.
var _ dialerapi.OperatorFSM = (interface{ dialerapi.OperatorFSM })(nil)

// PgCallOperatorLookup resolves (tenant_id, call_id) → operator_id by
// reading the calls table under tenant RLS. Used by the
// dialer/transport/nats subscriber that routes telephony events into
// OperatorFSM transitions — the telephony event payload carries
// tenant_id + call_id but not operator_id, so the subscriber needs
// this projection to call the FSM.
//
// Runs inside pool.WithTenant — the RLS predicate (calls_iso) gates
// the row by tenant_id, defending against a poisoned event payload
// that names another tenant's call_id.
//
// Exported because cmd/worker constructs one directly (it does NOT go
// through Module.Register). cmd/api uses newPgCallOperatorLookup via
// Module.Register.
type PgCallOperatorLookup struct {
	pool *postgres.Pool
}

// Compile-time interface check.
var _ dialertnats.CallOperatorLookup = (*PgCallOperatorLookup)(nil)

// NewPgCallOperatorLookup constructs the adapter. pool MUST be non-nil
// — wiring without one is a programmer bug.
//
// Exported sibling of newPgCallOperatorLookup; cmd/worker calls this
// directly (it bypasses Module.Register).
func NewPgCallOperatorLookup(pool *postgres.Pool) *PgCallOperatorLookup {
	if pool == nil {
		panic("dialer.NewPgCallOperatorLookup: pool must be non-nil")
	}
	return &PgCallOperatorLookup{pool: pool}
}

// newPgCallOperatorLookup is the package-private constructor used by
// Module.Register. Kept thin so the exported variant remains the
// canonical entry point.
func newPgCallOperatorLookup(pool *postgres.Pool) *PgCallOperatorLookup {
	return NewPgCallOperatorLookup(pool)
}

// LookupOperator satisfies dialertnats.CallOperatorLookup. Returns
// dialertnats.ErrOperatorNotFound when no row matches OR when the
// matched row has a NULL operator_id (a call placed without operator
// binding is not actionable by the FSM). Anything else propagates as a
// transient error so the subscriber NAKs for redelivery.
func (a *PgCallOperatorLookup) LookupOperator(ctx context.Context, tenantID, callID uuid.UUID) (uuid.UUID, error) {
	if tenantID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("dialer/lookup_operator: nil tenantID")
	}
	if callID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("dialer/lookup_operator: nil callID")
	}

	// operator_id is nullable on the calls table so we scan into a
	// pointer and translate the NULL into ErrOperatorNotFound.
	var opPtr *uuid.UUID
	err := a.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		const q = `SELECT operator_id FROM calls WHERE id = $1 LIMIT 1`
		row := tx.QueryRow(ctx, q, callID)
		switch err := row.Scan(&opPtr); {
		case errors.Is(err, pgx.ErrNoRows):
			return dialertnats.ErrOperatorNotFound
		case err != nil:
			return fmt.Errorf("dialer/lookup_operator: scan: %w", err)
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	if opPtr == nil {
		return uuid.Nil, dialertnats.ErrOperatorNotFound
	}
	return *opPtr, nil
}
