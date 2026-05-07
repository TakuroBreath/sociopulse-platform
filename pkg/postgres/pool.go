// Package postgres is the project-wide pgx pool wrapper. It is the only
// sanctioned entry point to Postgres for code outside the tenancy and
// migrator binaries: the depguard rule (Plan 00a Task 8) blocks direct
// imports of jackc/pgx/v5/pgxpool from internal/<module>/store/.
//
// The wrapper enforces tenant context: WithTenantTx opens a transaction
// and issues SET LOCAL app.tenant_id = '<uuid>' so the row-level
// security policies introduced in Plan 03 Task 5 filter rows by tenant
// without each query having to thread tenantID into its WHERE clause.
//
// See ADR-0006 (RLS + SET LOCAL) and Plan 03 Task 4 for the concrete
// pgxpool wiring (PgBouncer transaction-mode tuning, statement-cache
// disable, pool sizing).
package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Pool is the project-wide Postgres pool. It enforces tenant context
// for all reads/writes via WithTenantTx, which sets app.tenant_id LOCAL
// inside a transaction so RLS policies (Plan 03 Task 5) filter rows
// correctly.
type Pool struct {
	// unexported pgxpool.Pool — initialised in Plan 03 Task 4
}

// New opens a pool from the provided DSN and configures it for the
// project's PgBouncer transaction-mode setup (no prepared-statement
// cache, simple-protocol where required, sane pool defaults).
func New(ctx context.Context, dsn string) (*Pool, error) {
	panic("not implemented: see Plan 03 Task 4")
}

// Close drains and shuts down the pool. Safe to call once the
// surrounding context has been cancelled.
func (p *Pool) Close() {
	panic("not implemented: see Plan 03 Task 4")
}

// WithTenantTx is the only sanctioned way to access tenant-scoped
// tables. It opens a transaction, sets app.tenant_id LOCAL to
// tenantID, runs fn, and commits or rolls back based on fn's return
// value.
//
// fn MUST NOT spawn goroutines that outlive the call: the underlying
// pgx.Tx is bound to a single connection and is not safe for
// concurrent use.
func (p *Pool) WithTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
	panic("not implemented: see Plan 03 Task 4")
}

// WithoutTenant opens a transaction without setting app.tenant_id.
// Reserved for cross-tenant operations performed by service-owners
// (Plan 04 Task 7) and for migrations (cmd/migrator). Direct use
// elsewhere is a depguard violation (Plan 00a Task 8).
func (p *Pool) WithoutTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	panic("not implemented: see Plan 03 Task 4")
}
