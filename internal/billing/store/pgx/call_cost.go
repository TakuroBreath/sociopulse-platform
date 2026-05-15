package pgx

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// CallCostRow is the denormalised call_costs payload shared by InsertCallCost
// and InsertCallCostTx. All money fields are int64 minor units (kopecks) —
// the migration enforces CHECK (>= 0) on telecom/wages/storage/total.
//
// TariffVersion is *int because the migration column is nullable: a row
// inserted before billing.tariffs ever existed (legacy import, recompute
// of a pre-billing call) can persist with NULL, surfacing as a recomputable
// drift target later.
type CallCostRow struct {
	CallID        uuid.UUID
	TenantID      uuid.UUID
	ProjectID     uuid.UUID
	TrunkUsed     string
	DurationSec   int32
	Status        string
	TelecomMinor  int64
	WagesMinor    int64
	StorageMinor  int64
	TotalMinor    int64
	TariffVersion *int
	FinalizedAt   time.Time
}

// InsertCallCost upserts the call_costs row using ON CONFLICT (call_id) DO
// NOTHING for at-least-once redelivery safety (the NATS subject
// dialer.call.finalized is at-least-once per ADR-0010, and Step F's
// recompute job may also fire on the same call_id).
//
// Returns (true, nil) on real insert, (false, nil) on idempotent skip
// (call_id already present), and a wrapped error on storage failure. The
// pool's WithTenant scope is applied — RLS denies a forged TenantID.
//
// The Tx-variant (InsertCallCostTx) exists for callers that already hold
// a tenant-scoped transaction and need atomic state-flip alongside their
// own writes (e.g. future recompute jobs that also write an audit row).
func (s *PG) InsertCallCost(ctx context.Context, row CallCostRow) (bool, error) {
	var inserted bool
	err := s.pool.WithTenant(ctx, row.TenantID, func(tx postgres.Tx) error {
		ok, err := s.InsertCallCostTx(ctx, tx, row)
		inserted = ok
		return err
	})
	return inserted, err
}

// InsertCallCostTx is the Tx-variant of InsertCallCost — used by callers
// that already hold a tenant-scoped transaction and need atomic
// state-flip alongside their own writes.
//
// Returns (true, nil) on real insert, (false, nil) on idempotent skip
// (call_id already present). pgconn.CommandTag.RowsAffected reports 1 on
// insert, 0 on DO NOTHING — the canonical idiom mirrors
// internal/crm/store/project_store.go's UnassignOperator.
func (s *PG) InsertCallCostTx(ctx context.Context, tx postgres.Tx, row CallCostRow) (bool, error) {
	const q = `
insert into call_costs
  (call_id, tenant_id, project_id, trunk_used, duration_sec, status,
   telecom_minor, wages_minor, storage_minor, total_minor,
   tariff_version, finalized_at, computed_at)
values
  ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())
on conflict (call_id) do nothing`
	tag, err := tx.Exec(ctx, q,
		row.CallID, row.TenantID, row.ProjectID, row.TrunkUsed, row.DurationSec, row.Status,
		row.TelecomMinor, row.WagesMinor, row.StorageMinor, row.TotalMinor,
		row.TariffVersion, row.FinalizedAt,
	)
	if err != nil {
		return false, fmt.Errorf("billing/store: insert call_cost: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
