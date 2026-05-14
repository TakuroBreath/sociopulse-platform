// Package events publishes outbox events on behalf of the reports
// module. Used by both the synchronous Runner (Task 5) and the
// asynchronous Consumer (Task 6) — sync emits only audit, async emits
// audit AND reports.report.ready when the artifact is ready for
// download.
package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// OutboxWriter is the consumer-side surface the publisher needs;
// satisfied by *outbox.PostgresWriter in production. Defined here so
// tests use a small in-memory fake without bringing in the real outbox
// table.
type OutboxWriter interface {
	Append(ctx context.Context, tx postgres.Tx, ev outbox.Event) error
}

// ReportReadyPublisher emits the report-ready event after a job lifecycle
// completes (Task 6 wires this). The sync path does NOT emit this event
// — the response body IS the artifact.
type ReportReadyPublisher struct{ ob OutboxWriter }

// NewReportReadyPublisher builds a publisher on top of the given writer.
// Production passes outbox.NewPostgresWriter(); tests pass a recording
// fake that records appended events.
func NewReportReadyPublisher(ob OutboxWriter) *ReportReadyPublisher {
	return &ReportReadyPublisher{ob: ob}
}

// PublishReadyTx appends a tenant.<t>.reports.report.ready event to the
// outbox tx. The caller wraps this in pool.WithTenant alongside
// MarkSucceededTx and AuditEmitter.EmitTx for atomicity (Task 6).
//
// jobID is currently informational — the canonical id travels inside
// the marshalled ReportReadyEvent.JobID — but the parameter is kept on
// the signature so future callers can elevate it to the outbox row's
// AggregateID without an API churn.
func (p *ReportReadyPublisher) PublishReadyTx(
	ctx context.Context, tx postgres.Tx,
	tenantID uuid.UUID, jobID string, ready reportsapi.ReportReadyEvent,
) error {
	_ = jobID // reserved for future AggregateID hookup
	payload, err := json.Marshal(ready)
	if err != nil {
		return fmt.Errorf("reports.events.PublishReadyTx: marshal: %w", err)
	}
	tid := tenantID
	return p.ob.Append(ctx, tx, outbox.Event{
		TenantID: &tid,
		Subject:  reportsapi.SubjectReportReadyFor(tenantID),
		Payload:  payload,
	})
}
