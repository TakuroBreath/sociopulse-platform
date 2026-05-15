//go:build integration

// adapters_pg_test.go drives PgCallTenantResolver against real
// Postgres 16 — proves the BypassRLS cross-tenant read property
// end-to-end. Mirror of internal/recording/store/lookup_pg_test.go.
//
// Plan 21 Task 3 — closes the Plan 13.2.5 out-of-scope hangup finding.
// The unit-level coverage of the cross-tenant guard lives in
// internal/dialer/transport/http/routes_test.go::TestHangup_CrossTenant_Returns404;
// this file pins the SQL surface (BypassRLS + tenancy_admin grant on
// the calls table from migration 000014) so a future migration
// regression cannot silently break the guard.
package dialer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sociopulse/platform/internal/dialer"
	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// startPGContainer boots Postgres 16 in a container, applies all
// project migrations, and returns a connected *postgres.Pool. Pattern
// lifted from internal/dialer/fsm/audit_pg_test.go.
func startPGContainer(t *testing.T) *postgres.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("sociopulse_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	migrationsURL := repoMigrationsURL(t)
	mig, err := migrate.New(migrationsURL, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = mig.Close() })
	require.NoError(t, mig.Up())

	pool, err := postgres.Open(ctx, postgres.Config{
		DSN:            dsn,
		MaxConns:       8,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// repoMigrationsURL returns a file:// URL pointing at the repo's
// migrations directory. Mirrors fsm/audit_pg_test.go::repoMigrationsURL.
func repoMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err, "migrations dir not found at %s", abs)
	return "file://" + abs
}

// seedTenantFull inserts a tenant row via BypassRLS and returns its
// id. Uses the full UUID as org_code to avoid the same-millisecond
// collision risk seen with UUIDv7 prefixes.
func seedTenantFull(t *testing.T, pool *postgres.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			 VALUES ($1, $2, $3, 'active', 'kms-test', '\x00')`,
			id, "org-"+id.String(), "tenant-"+id.String(),
		)
		return err
	}))
	return id
}

// seedCallInTenant inserts a project + calls row under the given
// tenant and returns the call id. Mirrors the recording store's
// search_pg_test.go helper.
func seedCallInTenant(t *testing.T, pool *postgres.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	projectID := uuid.Must(uuid.NewV7())
	callID := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status)
			 VALUES ($1, $2, $3, 'Test Project', 'active')`,
			projectID, tenantID, "proj-"+projectID.String(),
		); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(),
			`INSERT INTO calls (id, tenant_id, project_id, started_at, status)
			 VALUES ($1, $2, $3, now(), 'success')`,
			callID, tenantID, projectID,
		)
		return err
	}))
	return callID
}

// TestPgCallTenantResolver_LookupCallTenant_Found verifies a BypassRLS
// SELECT returns the tenant for a call_id whose calls row exists.
func TestPgCallTenantResolver_LookupCallTenant_Found(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	r := dialer.NewPgCallTenantResolver(pool)

	tenantID := seedTenantFull(t, pool)
	callID := seedCallInTenant(t, pool, tenantID)

	got, err := r.LookupCallTenant(t.Context(), callID)
	require.NoError(t, err)
	assert.Equal(t, tenantID, got)
}

// TestPgCallTenantResolver_LookupCallTenant_NotFound verifies the
// dialerapi.ErrCallNotFound sentinel is returned for an unknown id.
// The transport-layer adapter folds this into pkg/middleware/tenant.ErrNotFound
// so the wire response is a 404 with no body (existence-probe defence).
func TestPgCallTenantResolver_LookupCallTenant_NotFound(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	r := dialer.NewPgCallTenantResolver(pool)

	_, err := r.LookupCallTenant(t.Context(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, dialerapi.ErrCallNotFound),
		"missing call_id must wrap ErrCallNotFound")
}

// TestPgCallTenantResolver_LookupCallTenant_BypassRLS_CrossTenant
// proves the core property of the Plan 21 Task 3 guard: the lookup
// must resolve regardless of any caller-set tenant context. Without
// the BypassRLS path the RLS predicate calls_iso (tenant_id =
// current_setting('app.tenant_id')::uuid) would hide tenant A's row
// from a caller scoped to tenant B — defeating the cross-tenant
// guard.
//
// Verified end-to-end here so a regression in the tenancy_admin grant
// (migration 000014) or the BypassRLS plumbing surfaces immediately.
func TestPgCallTenantResolver_LookupCallTenant_BypassRLS_CrossTenant(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	r := dialer.NewPgCallTenantResolver(pool)

	tenantA := seedTenantFull(t, pool)
	tenantB := seedTenantFull(t, pool)
	callA := seedCallInTenant(t, pool, tenantA)

	// Caller's ambient WithTenant context is tenant B — normally RLS
	// would hide tenant A's row. The BypassRLS read in
	// LookupCallTenant must still resolve, returning tenant A.
	require.NoError(t, pool.WithTenant(t.Context(), tenantB, func(_ postgres.Tx) error {
		got, err := r.LookupCallTenant(t.Context(), callA)
		require.NoError(t, err)
		assert.Equal(t, tenantA, got,
			"BypassRLS must resolve tenant A's call even when caller is scoped to tenant B")
		return nil
	}))
}
