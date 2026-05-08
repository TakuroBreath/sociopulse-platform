package retry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// MatureRetryRow is one respondent row that the orchestrator's sweep
// loop is about to re-enqueue. The rows are produced by ListMatureRetries;
// the orchestrator decrypts Phone via the configured Decryptor and
// invokes EnqueueRespondent on the dialer queue.
type MatureRetryRow struct {
	// ID is respondents.id.
	ID uuid.UUID
	// TenantID scopes the row; required for Decryptor + queue routing.
	TenantID uuid.UUID
	// ProjectID is the project this respondent belongs to.
	ProjectID uuid.UUID
	// PhoneCiphertext is respondents.phone_encrypted, ready for
	// Decryptor.Decrypt(tenantID, ciphertext).
	PhoneCiphertext []byte
	// Region is respondents.region_code (ISO 3166-2:RU style).
	Region string
	// Attempts is the count BEFORE this re-enqueue. Used by the
	// orchestrator to compute the new queue priority.
	Attempts int
	// NextAttemptAt is the materialised retry timestamp from the row.
	// Mainly diagnostic — the SQL filter is what selected this row.
	NextAttemptAt time.Time
}

// RespondentReader is the persistence surface the orchestrator
// consumes. The dialer/retry package depends on this interface (NOT on
// crm.RespondentStorePort) for two reasons:
//
//  1. The orchestrator runs as a privileged sweep — it must see EVERY
//     pending row across tenants, which RLS-scoped reads can't deliver.
//     The default Postgres-backed implementation in this package uses
//     postgres.Pool.BypassRLS to issue a single multi-tenant SELECT.
//
//  2. Tests substitute an in-memory fake without dragging crm/store in.
//
// Implementations MUST be safe to call from a single goroutine at a
// time; the orchestrator never calls them concurrently per instance.
type RespondentReader interface {
	// ListMatureRetries returns up to `limit` rows whose
	// next_attempt_at <= NOW() and status='pending'. Rows are locked
	// via FOR UPDATE SKIP LOCKED so concurrent leaders (during a
	// failover window) don't double-process; the lock is released when
	// the surrounding transaction commits.
	ListMatureRetries(ctx context.Context, limit int) ([]MatureRetryRow, error)

	// MarkExhausted sets respondents.status='exhausted' and
	// next_attempt_at=NULL for the supplied id. Used when attempts hit
	// max_attempts on a retry-eligible disposition.
	MarkExhausted(ctx context.Context, id uuid.UUID) error

	// MarkScheduled sets respondents.status='dialing' and
	// next_attempt_at=NULL for the supplied id. The orchestrator calls
	// this AFTER a successful queue.EnqueueRespondent so the respondent
	// is not re-picked on the next sweep tick.
	MarkScheduled(ctx context.Context, id uuid.UUID) error
}

// PgReader is the production RespondentReader. It runs every operation
// inside postgres.Pool.BypassRLS so the cross-tenant sweep sees rows
// regardless of the per-request app.tenant_id setting.
type PgReader struct {
	pool *postgres.Pool
}

// NewPgReader constructs a PgReader.
func NewPgReader(pool *postgres.Pool) (*PgReader, error) {
	if pool == nil {
		return nil, errors.New("retry: PgReader requires a postgres pool")
	}
	return &PgReader{pool: pool}, nil
}

// Compile-time check that PgReader satisfies RespondentReader.
var _ RespondentReader = (*PgReader)(nil)

// ListMatureRetries implements RespondentReader. The query uses FOR
// UPDATE SKIP LOCKED inside a BypassRLS transaction so a hand-off
// between leaders never double-issues the same row.
//
// Limit is sanitised to [1, 1000]; out-of-band values clamp.
func (r *PgReader) ListMatureRetries(ctx context.Context, limit int) ([]MatureRetryRow, error) {
	limit = clampLimit(limit)

	var out []MatureRetryRow
	err := r.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, `
			select id, tenant_id, project_id, phone_encrypted, region_code,
			       attempts, next_attempt_at
			  from respondents
			 where status = 'pending'
			   and next_attempt_at is not null
			   and next_attempt_at <= now()
			 order by next_attempt_at asc
			 limit $1
			   for update skip locked
		`, limit)
		if err != nil {
			return fmt.Errorf("retry/store: list mature retries: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var row MatureRetryRow
			if err := rows.Scan(
				&row.ID,
				&row.TenantID,
				&row.ProjectID,
				&row.PhoneCiphertext,
				&row.Region,
				&row.Attempts,
				&row.NextAttemptAt,
			); err != nil {
				return fmt.Errorf("retry/store: scan mature row: %w", err)
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkExhausted implements RespondentReader.
func (r *PgReader) MarkExhausted(ctx context.Context, id uuid.UUID) error {
	return r.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, `
			update respondents
			   set status = 'exhausted',
			       next_attempt_at = null
			 where id = $1
		`, id)
		if err != nil {
			return fmt.Errorf("retry/store: mark exhausted %s: %w", id, err)
		}
		return nil
	})
}

// MarkScheduled implements RespondentReader. The row's next_attempt_at
// is cleared so a future sweep does not re-pick it; the row's status
// transitions to 'dialing'. The dialer worker that pops the queue
// item finalises the row's status when the call concludes.
func (r *PgReader) MarkScheduled(ctx context.Context, id uuid.UUID) error {
	return r.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, `
			update respondents
			   set status = 'dialing',
			       next_attempt_at = null
			 where id = $1
		`, id)
		if err != nil {
			return fmt.Errorf("retry/store: mark scheduled %s: %w", id, err)
		}
		return nil
	})
}

// clampLimit normalises the batch size to a sane bracket. The default
// [1, 1000] mirrors the sweep budget — anything over 1000 rows per tick
// risks lock-table pressure during the FOR UPDATE.
func clampLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}
