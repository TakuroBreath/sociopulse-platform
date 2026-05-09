package worker

import (
	"context"
	"time"

	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// LifecycleStore is the worker-package narrow contract on the store. It
// bundles the cross-tenant LIST methods (which run on BypassRLS Tx
// inside the store impl) with the in-Tx mutation methods (which the
// caller MUST run inside an already-tenant-scoped Tx).
//
// Production wiring passes *store.PostgresStore directly; this
// interface exists primarily as a documentation surface and a seam for
// future fakes. Plan 12.4 tests use the real store + real Postgres
// because the audit-row + outbox INSERT paths need a real Tx anyway.
//
// The interface is shared between the retention worker (Plan 12.4
// Task 2) and the integrity worker (Plan 12.4 Task 3). Both consumers
// import every method even when they only call a subset — extending the
// interface in place keeps the wiring uniform across cmd/worker.
type LifecycleStore interface {
	// ListDueColdMoves runs cross-tenant on a BypassRLS Tx (retention worker).
	ListDueColdMoves(ctx context.Context, now time.Time, limit int) ([]rapi.LifecycleRow, error)
	// ListDueDeletes runs cross-tenant on a BypassRLS Tx (retention worker).
	ListDueDeletes(ctx context.Context, now time.Time, limit int) ([]rapi.LifecycleRow, error)
	// SampleForVerify runs cross-tenant on a BypassRLS Tx (integrity worker).
	SampleForVerify(ctx context.Context, samplePct float64, limit int) ([]rapi.LifecycleRow, error)

	// MarkColdTx runs in the caller's already-tenant-scoped Tx.
	MarkColdTx(ctx context.Context, tx postgres.Tx, id uuid.UUID) (int64, error)
	// MarkDeletedTx runs in the caller's already-tenant-scoped Tx.
	MarkDeletedTx(ctx context.Context, tx postgres.Tx, id uuid.UUID) (int64, error)
	// UpdateVerifyResultTx runs in the caller's already-tenant-scoped Tx.
	// Returns rowsAffected — caller treats 0 as a benign skip (the row was
	// concurrently deleted between SampleForVerify and this Tx).
	UpdateVerifyResultTx(
		ctx context.Context,
		tx postgres.Tx,
		id uuid.UUID,
		verifiedAt time.Time,
		integrityOK bool,
	) (int64, error)
}
