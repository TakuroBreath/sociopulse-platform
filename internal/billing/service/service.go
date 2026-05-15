package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Service is the composed billing root used by cmd/api and the HTTP
// transport layer. All dependency fields are interfaces so handler tests
// can swap in fakes.
//
// Wiring lives in internal/billing/module.go (Step I — Plan 14 Task 10).
// Until that step lands, this struct is consumed only by the transport
// layer's tests and by future module.go code.
type Service struct {
	// SpendReport returns per-tenant monthly spend rollups.
	SpendReport billingapi.SpendReport
	// MarginReport returns per-project margin rows.
	MarginReport billingapi.MarginReport
	// Revenue computes platform revenue per tenant×project×month.
	Revenue billingapi.RevenueCalculator
	// Tariffs persists per-tenant Tariffs snapshots.
	Tariffs billingapi.TariffStore
	// DefaultTariffs is the BillingConfig.Defaults fallback used when a
	// tenant has not yet PATCHed its own tariffs.
	DefaultTariffs billingapi.Tariffs
	// Logger is the structured logger used by handlers for error/audit.
	// Production passes the module-named logger; tests pass zap.NewNop.
	Logger *zap.Logger
}

// AuditWriter is the narrow outbox-writer surface AuditEmitter needs.
// Production uses *outbox.PostgresWriter; tests use a recording fake.
//
// Mirrors internal/reports/service.AuditWriter — kept billing-local so a
// future change in the reports module's interface does not ripple here.
type AuditWriter interface {
	Append(ctx context.Context, tx postgres.Tx, ev outbox.Event) error
}

// tenantTxRunner is the narrow pool surface AuditEmitter needs. *postgres.Pool
// implements it; tests pass a recording fake to exercise the emit-path
// without a testcontainer Postgres.
type tenantTxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
}

// AuditEmitter publishes billing-related audit events to the outbox.
// Mirrors internal/reports/service.AuditEmitter. The audit module is still
// a stub (Plan 03 Task 7); events sit durably in event_outbox until that
// consumer ships.
//
// Atomicity posture (per docs/references/plan-14-billing.md Step G
// architectural decisions): tariff updates and audit emit are NOT in the
// same Tx in v1 because tariffStore.Update opens its own Tx via
// UpsertSettings — no way to inject an external Tx without a larger
// refactor. EmitTariffUpdated therefore runs in its own pool.WithTenant
// Tx after the tariff update has committed; failure is logged at WARN but
// does NOT roll back the tariff change. At-most-once audit on tariff
// changes is the explicit trade-off.
type AuditEmitter struct {
	pool tenantTxRunner
	ob   AuditWriter
	log  *zap.Logger
}

// NewAuditEmitter wires an outbox writer + a pool (for the tenant-scoped
// Tx). Panics on nil pool/writer: every caller is constructed at module-
// register time and a nil here is a wiring bug we want to surface loudly
// rather than degrade silently. A nil logger is permitted (defaults to
// zap.NewNop) so tests don't have to construct one explicitly.
func NewAuditEmitter(pool *postgres.Pool, ob AuditWriter, log *zap.Logger) *AuditEmitter {
	if pool == nil {
		panic("billing.NewAuditEmitter: pool must be non-nil")
	}
	if ob == nil {
		panic("billing.NewAuditEmitter: writer must be non-nil")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &AuditEmitter{pool: pool, ob: ob, log: log}
}

// EmitTariffUpdated appends a tenant.<t>.audit.event row to the outbox
// describing a tariff change. The event runs in its own Tx — best-effort:
// audit emit failure is logged at WARN but does NOT roll back the prior
// TariffStore.Update. See AuditEmitter type-comment for rationale.
//
// changedKeys carries the dotted field names that changed (e.g.
// "wage_per_survey_minor"). Per docs/references/plan-14-billing.md §4.11
// we do not store the numeric values in the audit payload — only the
// changed-keys list. The actual values live in tenant_settings.billing.*
// and can be queried out-of-band by an auditor.
//
// Never returns an error — failure is logged for the operator. The caller
// (the HTTP PATCH handler) already returned 200 to the client.
func (e *AuditEmitter) EmitTariffUpdated(
	ctx context.Context,
	tenantID, actorID uuid.UUID,
	versionBefore, versionAfter int,
	changedKeys []string,
) {
	if tenantID == uuid.Nil {
		// Defensive — every legitimate call has a real tenant. We log
		// rather than panic so a future regression doesn't take down a
		// successful tariff write.
		e.log.Warn("billing/audit: emit tariff_updated with zero tenant — skipping")
		return
	}

	// ActorID is *uuid.UUID with omitempty; only set the pointer when the
	// caller provided a real actor.
	var actorIDPtr *uuid.UUID
	if actorID != uuid.Nil {
		a := actorID
		actorIDPtr = &a
	}
	ev := auditapi.Event{
		ID:        uuid.New(),
		TenantID:  tenantID,
		ActorID:   actorIDPtr,
		ActorKind: auditapi.ActorUser,
		Action:    billingapi.AuditActionTariffUpdated,
		Target:    fmt.Sprintf("tariff:%s", tenantID),
		Payload: map[string]any{
			"version_before": versionBefore,
			"version_after":  versionAfter,
			"changed_keys":   changedKeys,
		},
		Timestamp: time.Now().UTC(),
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		// Marshaling a well-formed auditapi.Event cannot fail in practice
		// (every field is JSON-friendly); the defensive branch surfaces a
		// future regression rather than hiding it.
		e.log.Warn("billing/audit: marshal event failed",
			zap.String("tenant_id", tenantID.String()),
			zap.Error(err),
		)
		return
	}
	subject := auditapi.SubjectAuditEventFor(tenantID)
	emitErr := e.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		tid := tenantID
		return e.ob.Append(ctx, tx, outbox.Event{
			TenantID: &tid,
			Subject:  subject,
			Payload:  payload,
		})
	})
	if emitErr != nil {
		// Audit emit failure does NOT fail the API request — the tariff
		// update is already persisted. We log loudly so an operator can
		// notice repeated misses in metrics.
		e.log.Warn("billing/audit: emit tariff_updated failed",
			zap.String("tenant_id", tenantID.String()),
			zap.Error(emitErr),
		)
	}
}
