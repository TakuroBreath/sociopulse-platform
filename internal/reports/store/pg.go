// Package store persists reports_jobs lifecycle in Postgres.
//
// PG.Create / Get / List open their own pool.WithTenant scope keyed on
// the caller-supplied tenantID — the project convention (recording's
// Search/GetByCallID, dialer's retry-row reads) is for store methods
// to manage their own RLS scope rather than leak postgres.Tx through
// the API. This keeps consumers' call sites simple: the gin handler
// already knows the tenantID from the auth middleware, and passes it
// in.
//
// SelectTenantByJobID is the one cross-tenant exception: it runs
// inside pool.BypassRLS to resolve a tenant from a bare job id (the
// asynq consumer's resolver pattern), then the consumer re-enters
// pool.WithTenant with that tenant for any subsequent reads/writes.
//
// The *Tx variants in tx.go take a caller-managed postgres.Tx for the
// atomic state-flip + audit + outbox commit (Task 5/6).
package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// reportJobColumns is the canonical SELECT projection — kept as a single
// constant so Get and List scan the same row shape and a column rename
// surfaces in both call sites at once.
const reportJobColumns = `
    id, tenant_id, kind, format, params, window_from, window_to,
    state, started_at, finished_at, bytes_size, filename, download_url,
    error, created_by, created_at
`

// PG persists reports_jobs rows via *postgres.Pool.
type PG struct {
	pool *postgres.Pool
}

// NewPG wires the store onto the supplied pool. The pool is held by
// reference; the caller owns its lifecycle.
func NewPG(pool *postgres.Pool) *PG { return &PG{pool: pool} }

// CreateInput is the data needed to insert a new reports_jobs row.
//
// TenantID is the authoritative tenant for the row; PG.Create opens
// pool.WithTenant(TenantID) before issuing the INSERT, so RLS WITH CHECK
// will reject any drift between the caller's claim and the row content.
type CreateInput struct {
	ID           string
	TenantID     uuid.UUID
	Kind         reportsapi.ReportKind
	Format       reportsapi.ExportFormat
	Params       map[string]any
	WindowFrom   time.Time
	WindowTo     time.Time
	CreatedBy    uuid.UUID
	NotifyUserID uuid.UUID
}

// Create inserts a new reports_jobs row inside a pool.WithTenant scope
// keyed on in.TenantID. State defaults to 'queued' (server-side);
// started_at and finished_at default to NULL. The *Tx variants in tx.go
// transition these as the job progresses.
func (s *PG) Create(ctx context.Context, in CreateInput) error {
	const q = `
INSERT INTO reports_jobs
    (id, tenant_id, kind, format, params, window_from, window_to,
     created_by, notify_user_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`
	paramsJSON, err := marshalParams(in.Params)
	if err != nil {
		return fmt.Errorf("reports.store.Create: marshal params: %w", err)
	}

	if err := s.pool.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		_, execErr := tx.Exec(ctx, q,
			in.ID, in.TenantID, string(in.Kind), string(in.Format),
			paramsJSON, in.WindowFrom, in.WindowTo,
			in.CreatedBy, in.NotifyUserID,
		)
		return execErr
	}); err != nil {
		return fmt.Errorf("reports.store.Create: %w", err)
	}
	return nil
}

// Get returns one job by id, scoped to tenantID via pool.WithTenant. A
// row that belongs to a different tenant is hidden by RLS and surfaces
// as reportsapi.ErrJobNotFound — the same shape as a genuinely missing
// row, which is the existence-probe defence (we never leak the
// distinction between "wrong tenant" and "no such id").
//
// Worker-bound callers that only know the job id resolve the tenant
// first via SelectTenantByJobID, then call Get with the resolved
// tenantID.
func (s *PG) Get(ctx context.Context, tenantID uuid.UUID, jobID string) (reportsapi.Job, error) {
	q := `SELECT` + reportJobColumns + `FROM reports_jobs WHERE id = $1`

	var job reportsapi.Job
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		row := tx.QueryRow(ctx, q, jobID)
		var scanErr error
		job, scanErr = scanJob(row)
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, postgres.ErrNoRows) {
			return reportsapi.Job{}, fmt.Errorf("%w: %s", reportsapi.ErrJobNotFound, jobID)
		}
		return reportsapi.Job{}, fmt.Errorf("reports.store.Get: %w", err)
	}
	return job, nil
}

// List returns up to f.Limit jobs (clamped to [1, 500]; defaults to 100
// on zero) in created_at DESC order with a stable id DESC tie-break.
// Cursor is opaque to clients (base64 of "unix_seconds:id"); an empty
// next-cursor signals end-of-results.
//
// Supported filters: State, Kind, From / To. From/To bound the
// created_at axis (when the job was queued); the report's data window
// lives in window_from/window_to and is not currently filterable.
//
// Tenant scoping is opened inside the method (pool.WithTenant(tenantID,
// …)); RLS enforces the predicate.
func (s *PG) List(ctx context.Context, tenantID uuid.UUID, f reportsapi.ListJobsFilter) ([]reportsapi.Job, string, error) {
	sql, args := buildListQuery(f)
	limit := clampLimit(f.Limit)

	out := make([]reportsapi.Job, 0, limit)
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		rows, qErr := tx.Query(ctx, sql, args...)
		if qErr != nil {
			return fmt.Errorf("query: %w", qErr)
		}
		defer rows.Close()

		for rows.Next() {
			job, scanErr := scanJob(rows)
			if scanErr != nil {
				return fmt.Errorf("scan: %w", scanErr)
			}
			out = append(out, job)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", fmt.Errorf("reports.store.List: %w", err)
	}

	var nextCursor string
	if len(out) == limit && limit > 0 {
		last := out[len(out)-1]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, nextCursor, nil
}

// SelectTenantByJobID returns the tenant that owns jobID. The lookup
// runs cross-tenant via pool.BypassRLS so the asynq consumer (which
// only knows the task id) can resolve the tenant before installing
// app.tenant_id. Used by Task 7's RequireSameTenant resolver.
//
// Returns reportsapi.ErrJobNotFound (wrapped) when the row is missing.
func (s *PG) SelectTenantByJobID(ctx context.Context, jobID string) (uuid.UUID, error) {
	const q = `SELECT tenant_id FROM reports_jobs WHERE id = $1`

	var tenantID uuid.UUID
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		switch scanErr := tx.QueryRow(ctx, q, jobID).Scan(&tenantID); {
		case errors.Is(scanErr, pgx.ErrNoRows):
			return reportsapi.ErrJobNotFound
		case scanErr != nil:
			return scanErr
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, reportsapi.ErrJobNotFound) {
			return uuid.Nil, fmt.Errorf("%w: %s", reportsapi.ErrJobNotFound, jobID)
		}
		return uuid.Nil, fmt.Errorf("reports.store.SelectTenantByJobID: %w", err)
	}
	return tenantID, nil
}

// rowScanner is the common subset of pgx.Row and pgx.Rows that scanJob
// needs. Using the interface lets scanJob serve both QueryRow (single
// row) and Query iteration (multi-row) call sites.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanJob reads one row into reportsapi.Job following the
// reportJobColumns projection.
func scanJob(row rowScanner) (reportsapi.Job, error) {
	var (
		job         reportsapi.Job
		kindStr     string
		formatStr   string
		stateStr    string
		paramsBytes []byte
		startedAt   *time.Time
		finishedAt  *time.Time
	)
	if err := row.Scan(
		&job.ID,
		&job.TenantID,
		&kindStr,
		&formatStr,
		&paramsBytes,
		&job.Window.From,
		&job.Window.To,
		&stateStr,
		&startedAt,
		&finishedAt,
		&job.BytesSize,
		&job.Filename,
		&job.DownloadURL,
		&job.Error,
		&job.CreatedBy,
		&job.CreatedAt,
	); err != nil {
		return reportsapi.Job{}, err
	}
	job.Kind = reportsapi.ReportKind(kindStr)
	job.Format = reportsapi.ExportFormat(formatStr)
	job.State = reportsapi.JobState(stateStr)
	job.StartedAt = startedAt
	job.FinishedAt = finishedAt
	params, err := unmarshalParams(paramsBytes)
	if err != nil {
		return reportsapi.Job{}, fmt.Errorf("unmarshal params: %w", err)
	}
	job.Params = params
	return job, nil
}

// marshalParams encodes a free-form params map as JSON bytes, treating
// nil as the empty object so the NOT NULL column constraint holds and
// the round-trip through unmarshalParams yields a stable shape.
func marshalParams(p map[string]any) ([]byte, error) {
	if p == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(p)
}

// unmarshalParams decodes the jsonb column back to map[string]any.
// json.Unmarshal of "{}" yields an empty non-nil map, which is the
// shape callers expect.
func unmarshalParams(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// clampLimit normalises a caller-supplied list limit to [1, 500] with
// 100 as the default when zero or negative is passed. The cap mirrors
// the dialer/recording sweep convention.
func clampLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 500 {
		return 500
	}
	return limit
}

// encodeCursor produces the opaque continuation token used by List.
// Format: base64-url("unix_seconds:id"). Unix seconds are sufficient
// because the keyset predicate tiebreaks on id within the same second.
func encodeCursor(t time.Time, id string) string {
	raw := strconv.FormatInt(t.UTC().Unix(), 10) + ":" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reverses encodeCursor. An empty input returns the
// no-cursor sentinel (zero time, empty id, nil error) so the caller can
// pass an empty filter through unchanged. Any malformed payload
// (non-base64, missing separator, empty halves, non-numeric unix half)
// returns a typed error — buildListQuery treats this as "first page".
func decodeCursor(s string) (time.Time, string, error) {
	if s == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor: base64 decode: %w", err)
	}
	idx := strings.IndexByte(string(raw), ':')
	if idx < 0 {
		return time.Time{}, "", errors.New("cursor: missing separator")
	}
	unixStr := string(raw[:idx])
	id := string(raw[idx+1:])
	if unixStr == "" {
		return time.Time{}, "", errors.New("cursor: empty unix half")
	}
	if id == "" {
		return time.Time{}, "", errors.New("cursor: empty id half")
	}
	unixSec, err := strconv.ParseInt(unixStr, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor: parse unix: %w", err)
	}
	return time.Unix(unixSec, 0).UTC(), id, nil
}

// buildListQuery assembles the SELECT-with-optional-filters SQL plus the
// matching args slice for PG.List. Kept pure (no DB access) so the
// predicate-assembly logic is unit-testable.
//
// SQL shape:
//
//	SELECT <cols>
//	FROM reports_jobs
//	WHERE 1=1
//	  [AND state = $N]
//	  [AND kind = $N]
//	  [AND created_at >= $N]
//	  [AND created_at < $N]
//	  [AND (created_at, id) < ($N, $N)]
//	ORDER BY created_at DESC, id DESC
//	LIMIT $N
//
// RLS does the tenant filter implicitly inside WithTenant — we do not
// emit `tenant_id = current_setting(...)` because the policy already
// has it. A malformed cursor is silently skipped (cursors are opaque,
// server-generated, and most malformed inputs are stale-tab scenarios
// — first page is a safer recovery than 400).
func buildListQuery(f reportsapi.ListJobsFilter) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, 8)

	addArg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	sb.WriteString("SELECT")
	sb.WriteString(reportJobColumns)
	sb.WriteString(" FROM reports_jobs WHERE 1=1")

	if f.State != nil {
		sb.WriteString(" AND state = ")
		sb.WriteString(addArg(string(*f.State)))
	}
	if f.Kind != nil {
		sb.WriteString(" AND kind = ")
		sb.WriteString(addArg(string(*f.Kind)))
	}
	if f.From != nil && !f.From.IsZero() {
		sb.WriteString(" AND created_at >= ")
		sb.WriteString(addArg(*f.From))
	}
	if f.To != nil && !f.To.IsZero() {
		sb.WriteString(" AND created_at < ")
		sb.WriteString(addArg(*f.To))
	}
	if f.Cursor != "" {
		if ts, id, err := decodeCursor(f.Cursor); err == nil && !ts.IsZero() {
			sb.WriteString(" AND (created_at, id) < (")
			sb.WriteString(addArg(ts))
			sb.WriteString(", ")
			sb.WriteString(addArg(id))
			sb.WriteString(")")
		}
	}

	sb.WriteString(" ORDER BY created_at DESC, id DESC LIMIT ")
	sb.WriteString(addArg(clampLimit(f.Limit)))

	return sb.String(), args
}
