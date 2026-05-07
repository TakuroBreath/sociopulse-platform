package postgres

import (
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Tx is the unit of access to Postgres. internal/<module>/store/
// adapters import it as postgres.Tx so they don't have to import
// jackc/pgx/v5 directly. WithTenantTx and WithoutTenant on Pool hand a
// Tx to the supplied callback.
type Tx = pgx.Tx

// Rows mirrors pgx.Rows for the same reason: a thin re-export so store
// adapters never name pgx in their imports.
type Rows = pgx.Rows

// Row mirrors pgx.Row for QueryRow callers.
type Row = pgx.Row

// CommandTag mirrors pgconn.CommandTag for callers inspecting
// RowsAffected on Exec results.
type CommandTag = pgconn.CommandTag

// ErrNoRows is the sentinel pgx returns when QueryRow finds no rows.
// Re-exported so callers can errors.Is(err, postgres.ErrNoRows).
var ErrNoRows = pgx.ErrNoRows
