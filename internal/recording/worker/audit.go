package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// auditActorKindService is the audit_log.actor_kind value emitted by
// background workers. Mirrors the value used by service/service.go's
// writeAuditRow / writeAccessAudit helpers so the audit module's
// downstream filters (admin UI, BI exports) can treat every worker-
// emitted row uniformly.
const auditActorKindService = "service"

// writeAudit inserts one audit_log row inside tx.
//
// Tx-scope contract: the caller MUST have set the tenant scope via
// pool.WithTenant before invoking — writeAudit does NOT switch role
// itself. The retention worker bundles writeAudit with the status-flip
// UPDATE and (for delete) the outbox INSERT inside one WithTenant Tx so
// all rows commit atomically.
//
// audit_log schema (migrations/000001_init.up.sql):
//
//	id bigserial primary key  (db-assigned — omitted from INSERT)
//	tenant_id uuid not null
//	actor_kind text not null check (actor_kind in ('user','system','service'))
//	actor_user_id uuid        (nullable — nil for service actors)
//	action text not null
//	target_kind text not null
//	target_id text            (nullable text, not uuid)
//	payload jsonb
//	ts timestamptz not null default now()
//
// The ts column is supplied by the caller (rather than the SQL default
// now()) so workers can apply the same time.Now() to multiple paired
// rows (audit + outbox + UPDATE) inside one Tx and keep the chain-of-
// custody coherent.
func writeAudit(
	ctx context.Context,
	tx postgres.Tx,
	tenantID, recordingID uuid.UUID,
	action string,
	payload map[string]any,
	ts time.Time,
) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("recording.worker: marshal audit payload: %w", err)
	}

	const q = `
INSERT INTO audit_log (tenant_id, actor_kind, actor_user_id, action, target_kind, target_id, payload, ts)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
`
	if _, err := tx.Exec(ctx, q,
		tenantID,
		auditActorKindService,
		nil, // actor_user_id — service actor has no user uuid
		action,
		"recording",
		recordingID.String(),
		body,
		ts,
	); err != nil {
		return fmt.Errorf("recording.worker: insert audit row: %w", err)
	}
	return nil
}
