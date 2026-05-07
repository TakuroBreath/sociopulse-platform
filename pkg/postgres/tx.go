package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Tx is the transaction handle exposed to store-layer code. It wraps pgx.Tx
// without exposing it directly, so callers can't accidentally Commit/Rollback
// (the wrapper owns lifecycle).
//
// Methods delegate to the embedded pgx.Tx so existing call sites that already
// use tx.Exec / tx.Query / tx.QueryRow keep compiling unchanged.
type Tx struct {
	tx pgx.Tx
}

// Exec executes a SQL statement that returns no rows.
func (t Tx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}

// Query runs a multi-row query. Callers MUST close the returned Rows.
func (t Tx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.tx.Query(ctx, sql, args...)
}

// QueryRow runs a single-row query.
func (t Tx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

// CopyFrom delegates to pgx.Tx.CopyFrom for bulk inserts (used by import flows).
func (t Tx) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, rows pgx.CopyFromSource) (int64, error) {
	return t.tx.CopyFrom(ctx, table, columns, rows)
}

// Conn returns the underlying connection — needed by some pgx helpers
// (e.g. listening for notifications). Use sparingly.
func (t Tx) Conn() *pgx.Conn {
	return t.tx.Conn()
}

// ErrNoRows is the sentinel pgx returns when QueryRow finds no rows.
// Re-exported so callers can errors.Is(err, postgres.ErrNoRows) without
// needing to import jackc/pgx/v5 directly.
var ErrNoRows = pgx.ErrNoRows
