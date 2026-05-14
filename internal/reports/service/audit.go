package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// AuditWriter is the outbox writer surface AuditEmitter needs. Defined
// here (consumer-side) rather than imported from pkg/outbox so tests can
// use a tiny fake without bringing in the real outbox table. The
// production implementation is *outbox.PostgresWriter, which satisfies
// this interface implicitly via the Append method.
type AuditWriter interface {
	Append(ctx context.Context, tx postgres.Tx, ev outbox.Event) error
}

// AuditEmitter publishes export-related audit events to the outbox.
// The audit module is currently a no-op stub awaiting Plan 03 Task 7;
// events sit durably in event_outbox until that consumer ships. The
// emitter's atomic-with-state-flip guarantee (caller wraps EmitTx +
// MarkSucceededTx in one Tx) preserves chain-of-custody.
type AuditEmitter struct {
	ob AuditWriter
}

// NewAuditEmitter builds an AuditEmitter on top of the given writer.
// Production passes outbox.NewPostgresWriter(); tests pass a recording
// fake that records appended events.
func NewAuditEmitter(ob AuditWriter) *AuditEmitter {
	return &AuditEmitter{ob: ob}
}

// AuditExport is the payload for an export-related audit event. Caller
// fills the fields; EmitTx marshals to auditapi.Event JSON, builds the
// tenant-scoped subject, and appends to the outbox tx.
type AuditExport struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	ActorKind auditapi.ActorKind
	JobID     string
	Kind      string
	Format    string
	BytesSize int64
	Window    AuditWindow
	Params    map[string]any
	Timestamp time.Time
}

// AuditWindow holds the [From, To) tuple in the audit payload. We don't
// reuse analyticsapi.Window here because it would force every test
// fixture to import analytics — a layering wart. The two types are
// isomorphic.
type AuditWindow struct {
	From time.Time
	To   time.Time
}

// EmitTx appends a tenant.<t>.audit.event to the outbox tx.
//
// Subject layout: tenant.<tenant_uuid>.audit.event (project convention).
// Payload: JSON of auditapi.Event with Action="reports.export",
// Target="report:<JobID>", Payload carrying
// kind/format/window/bytes_size/params.
func (e *AuditEmitter) EmitTx(ctx context.Context, tx postgres.Tx, in AuditExport) error {
	actorID := in.ActorID
	ev := auditapi.Event{
		ID:        uuid.New(),
		TenantID:  in.TenantID,
		ActorID:   &actorID,
		ActorKind: in.ActorKind,
		Action:    "reports.export",
		Target:    fmt.Sprintf("report:%s", in.JobID),
		Payload: map[string]any{
			"job_id":      in.JobID,
			"kind":        in.Kind,
			"format":      in.Format,
			"bytes_size":  in.BytesSize,
			"window_from": in.Window.From,
			"window_to":   in.Window.To,
			"params":      in.Params,
		},
		Timestamp: in.Timestamp,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("reports.service.AuditEmitter.EmitTx: marshal: %w", err)
	}
	tenantID := in.TenantID
	return e.ob.Append(ctx, tx, outbox.Event{
		TenantID: &tenantID,
		Subject:  fmt.Sprintf("tenant.%s.audit.event", in.TenantID),
		Payload:  payload,
	})
}
