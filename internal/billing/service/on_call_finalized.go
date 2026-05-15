package service

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
)

// CostSink is the narrow store interface OnCallFinalized needs to persist a
// call_cost row. Production is *internal/billing/store/pgx.PG; unit tests
// substitute an in-memory fake.
//
// The interface lives here, in the consumer package, per the
// "accept interfaces, return structs" convention also used by
// service.SettingsBackend. Intra-module imports of store/pgx are
// permitted by the depguard module-boundaries rule (the deny list scopes
// CROSS-module access only; see .golangci.yml lines 200-207).
//
// Idempotency contract:
//   - (true, nil)  → fresh INSERT, call_id was absent.
//   - (false, nil) → ON CONFLICT (call_id) DO NOTHING — redelivery.
//   - (_,    err)  → storage failure, caller should NACK and retry.
type CostSink interface {
	InsertCallCost(ctx context.Context, row billingpgx.CallCostRow) (bool, error)
}

// onCallFinalizedHandler implements billingapi.CallFinalizedHook. It glues
// CostCalculator, TariffStore, and CostSink into the single per-event
// pipeline the NATS subscriber drives. The struct holds only injected
// dependencies; it carries no per-call state and is safe to share across
// goroutines (the NATS subscriber routes one message per goroutine via
// queue group, but that is incidental — sharing is still safe).
type onCallFinalizedHandler struct {
	calc       billingapi.CostCalculator
	tariffs    billingapi.TariffStore
	sink       CostSink
	defTariffs billingapi.Tariffs
	log        *zap.Logger
}

// NewCallFinalizedHandler wires the three dependencies. defTariffs is the
// BillingConfig.Defaults snapshot used when a tenant has not yet
// configured tariffs (ErrNoTariffs fallback path).
//
// Panics on a nil calc/tariffs/sink: every caller is constructed at
// module-register time when dependencies are mandatory — a nil here is a
// wiring bug we want to fail loudly rather than degrade silently.
// A nil logger is permitted (defaults to zap.NewNop) so tests don't have
// to construct one explicitly.
func NewCallFinalizedHandler(
	calc billingapi.CostCalculator,
	tariffs billingapi.TariffStore,
	sink CostSink,
	defTariffs billingapi.Tariffs,
	log *zap.Logger,
) billingapi.CallFinalizedHook {
	if calc == nil {
		panic("billing/service.NewCallFinalizedHandler: calc must be non-nil")
	}
	if tariffs == nil {
		panic("billing/service.NewCallFinalizedHandler: tariffs must be non-nil")
	}
	if sink == nil {
		panic("billing/service.NewCallFinalizedHandler: sink must be non-nil")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &onCallFinalizedHandler{
		calc:       calc,
		tariffs:    tariffs,
		sink:       sink,
		defTariffs: defTariffs,
		log:        log,
	}
}

// Compile-time interface check: any signature drift in
// billingapi.CallFinalizedHook breaks the build here.
var _ billingapi.CallFinalizedHook = (*onCallFinalizedHandler)(nil)

// OnCallFinalized is the per-event entry point. It runs CallCost (pure
// arithmetic), then persists the call_costs row idempotently. The hook
// returns nil for both fresh inserts and idempotent skips so the NATS
// handler ACKs in both cases — only a real failure (calc broken, db
// down) returns an error and triggers redelivery.
//
// Per docs/references/plan-14-billing.md §4.11: NO audit row per
// InsertCallCost — only human-driven tariff changes earn audit entries.
func (h *onCallFinalizedHandler) OnCallFinalized(ctx context.Context, in billingapi.CallCostInput) error {
	tariffs, err := h.tariffs.Get(ctx, in.TenantID)
	switch {
	case errors.Is(err, billingapi.ErrNoTariffs):
		// Tenant has not configured tariffs — use the platform defaults.
		// Version stays at the zero value (0) → TariffVersion column is
		// persisted as NULL (signals "no admin-edit yet" to future audit).
		tariffs = h.defTariffs
		tariffs.TenantID = in.TenantID
	case err != nil:
		return fmt.Errorf("billing/oncallfinalized: load tariffs: %w", err)
	}

	out, err := h.calc.CallCost(ctx, in, tariffs)
	if err != nil {
		return fmt.Errorf("billing/oncallfinalized: calc: %w", err)
	}

	// tariff_version: nullable in DB. nil iff Version == 0 (default
	// fallback or fresh-tenant path); a real pointer otherwise so the
	// recompute job in Step F can join against tariff_history.
	var verPtr *int
	if tariffs.Version > 0 {
		v := tariffs.Version
		verPtr = &v
	}

	inserted, err := h.sink.InsertCallCost(ctx, billingpgx.CallCostRow{
		CallID:        in.CallID,
		TenantID:      in.TenantID,
		ProjectID:     in.ProjectID,
		TrunkUsed:     in.TrunkUsed,
		DurationSec:   in.DurationSec,
		Status:        in.Status,
		TelecomMinor:  out.TelecomMinor,
		WagesMinor:    out.WagesMinor,
		StorageMinor:  out.StorageMinor,
		TotalMinor:    out.TotalMinor,
		TariffVersion: verPtr,
		FinalizedAt:   in.FinalizedAt,
	})
	if err != nil {
		return fmt.Errorf("billing/oncallfinalized: insert: %w", err)
	}
	if !inserted {
		// Idempotent skip — log at debug, NOT error. The NATS handler
		// ACKs this just like a fresh insert.
		h.log.Debug("billing: call_cost already present, skipping",
			zap.String("call_id", in.CallID.String()),
			zap.String("tenant_id", in.TenantID.String()),
		)
	}
	return nil
}
