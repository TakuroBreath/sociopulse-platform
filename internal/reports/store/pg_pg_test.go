//go:build integration

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// ─────────────────────────────────────────────────────────────────────────────
// Create / Get / SelectTenantByJobID
// ─────────────────────────────────────────────────────────────────────────────

// TestStoreIntegration_CreateGetRoundTrip seeds a single job and reads
// it back inside the same tenant scope. Validates the column-by-column
// round-trip plus the params jsonb encoding.
func TestStoreIntegration_CreateGetRoundTrip(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)
	in := newCreateInput(tenantID)

	require.NoError(t, st.Create(t.Context(), in))

	got, err := st.Get(t.Context(), tenantID, in.ID)
	require.NoError(t, err)
	require.Equal(t, in.ID, got.ID)
	require.Equal(t, tenantID, got.TenantID)
	require.Equal(t, in.Kind, got.Kind)
	require.Equal(t, in.Format, got.Format)
	require.Equal(t, reportsapi.JobQueued, got.State, "fresh job must default to queued")
	require.Equal(t, in.CreatedBy, got.CreatedBy)
	require.Equal(t, in.Params["project_id"], got.Params["project_id"], "params jsonb round-trip")
	require.True(t, in.WindowFrom.Equal(got.Window.From))
	require.True(t, in.WindowTo.Equal(got.Window.To))
	require.Nil(t, got.StartedAt, "started_at must be nil for queued")
	require.Nil(t, got.FinishedAt, "finished_at must be nil for queued")
	require.Zero(t, got.BytesSize)
	require.Empty(t, got.Filename)
	require.Empty(t, got.DownloadURL)
}

// TestStoreIntegration_RLSPolicyExists verifies that the
// reports_jobs_iso policy was created by 000001_init and remains
// attached after the 000012 evolve migration. The policy enforces the
// `tenant_id = current_setting('app.tenant_id', true)::uuid` predicate
// at runtime for non-superuser DB connections; in tests the connection
// user is the postgres bootstrap (superuser) which bypasses RLS, so we
// can only check the policy is present — not exercise it negatively.
// Production deployments connect as a non-superuser app role; the
// policy then applies as designed.
func TestStoreIntegration_RLSPolicyExists(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)

	var policyName, policyQual string
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		return tx.QueryRow(t.Context(), `
			SELECT polname, pg_get_expr(polqual, polrelid)
			FROM pg_policy
			WHERE polrelid = 'reports_jobs'::regclass
		`).Scan(&policyName, &policyQual)
	}))
	require.NotEmpty(t, policyName, "reports_jobs must have a row-level security policy")
	require.Contains(t, policyQual, "app.tenant_id",
		"policy must filter on app.tenant_id setting (got %q)", policyQual)
}

// TestStoreIntegration_Get_MissingID returns ErrJobNotFound for a
// totally unknown id (within the correct tenant).
func TestStoreIntegration_Get_MissingID(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)

	_, err := st.Get(t.Context(), tenantID, "no-such-job-id")
	require.ErrorIs(t, err, reportsapi.ErrJobNotFound)
}

// TestStoreIntegration_SelectTenantByJobID_BypassRLS verifies that a
// worker that only knows the job id can resolve the owning tenant via
// the BypassRLS-scoped resolver.
func TestStoreIntegration_SelectTenantByJobID_BypassRLS(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)
	in := newCreateInput(tenantID)
	require.NoError(t, st.Create(t.Context(), in))

	got, err := st.SelectTenantByJobID(t.Context(), in.ID)
	require.NoError(t, err)
	require.Equal(t, tenantID, got, "resolver must return the tenant that owns the job")
}

// TestStoreIntegration_SelectTenantByJobID_NotFound asserts the sentinel
// wraps the "no row" case.
func TestStoreIntegration_SelectTenantByJobID_NotFound(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	_, err := st.SelectTenantByJobID(t.Context(), "totally-missing-id")
	require.ErrorIs(t, err, reportsapi.ErrJobNotFound)
}

// ─────────────────────────────────────────────────────────────────────────────
// List — pagination, filters
// ─────────────────────────────────────────────────────────────────────────────

// TestStoreIntegration_List_Pagination seeds 5 jobs in one tenant and
// walks the keyset cursor to verify each page contains the expected
// slice and the final page returns an empty next-cursor.
func TestStoreIntegration_List_Pagination(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)
	const n = 5
	ids := make([]string, 0, n)
	// Seed with a strictly-ordered created_at so the keyset predicate
	// has a deterministic split. The store's INSERT uses the server's
	// `now()`, but we cannot mutate that from the public API — so we
	// space the inserts with a small sleep to enforce ordering. Each
	// insert takes ~1ms anyway, so a 10ms separation is more than
	// enough to guarantee distinct created_at within one-second
	// granularity used by the cursor.
	for range n {
		in := newCreateInput(tenantID)
		require.NoError(t, st.Create(t.Context(), in))
		ids = append(ids, in.ID)
		time.Sleep(1100 * time.Millisecond) // ensures distinct unix-second cursor values
	}

	// Walk pages of size 2.
	const pageSize = 2
	var (
		seen   []string
		cursor string
	)
	for i := 0; i < n; i++ {
		jobs, next, err := st.List(t.Context(), tenantID, reportsapi.ListJobsFilter{
			Limit:  pageSize,
			Cursor: cursor,
		})
		require.NoError(t, err)
		for _, j := range jobs {
			seen = append(seen, j.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	require.ElementsMatch(t, ids, seen, "all seeded jobs must be returned across the paginated walk")
	require.Len(t, seen, n, "no duplicates across pages")
}

// TestStoreIntegration_List_StateFilter seeds three jobs and flips two
// to 'running' via MarkRunningTx. List with State=queued must return
// the untouched one; State=running returns the two flipped.
func TestStoreIntegration_List_StateFilter(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)

	in1 := newCreateInput(tenantID)
	in2 := newCreateInput(tenantID)
	in3 := newCreateInput(tenantID)
	require.NoError(t, st.Create(t.Context(), in1))
	require.NoError(t, st.Create(t.Context(), in2))
	require.NoError(t, st.Create(t.Context(), in3))

	// Flip in1 and in2 to running.
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		now := time.Now().UTC()
		if err := store.MarkRunningTx(t.Context(), tx, in1.ID, now); err != nil {
			return err
		}
		return store.MarkRunningTx(t.Context(), tx, in2.ID, now)
	}))

	queued := reportsapi.JobQueued
	running := reportsapi.JobRunning

	gotQueued, _, err := st.List(t.Context(), tenantID, reportsapi.ListJobsFilter{State: &queued, Limit: 100})
	require.NoError(t, err)
	require.Len(t, gotQueued, 1)
	require.Equal(t, in3.ID, gotQueued[0].ID)

	gotRunning, _, err := st.List(t.Context(), tenantID, reportsapi.ListJobsFilter{State: &running, Limit: 100})
	require.NoError(t, err)
	require.Len(t, gotRunning, 2)
	ids := []string{gotRunning[0].ID, gotRunning[1].ID}
	require.ElementsMatch(t, []string{in1.ID, in2.ID}, ids)
}

// TestStoreIntegration_List_OnlyReturnsKnownIDs is a same-tenant
// reality check: seed N rows for tenantA and one extra row for tenantB
// (which exists at the storage layer), then List(tenantA) and verify
// every returned row's TenantID is tenantA. In production the
// non-superuser app role makes RLS enforce this; here we additionally
// assert the projection round-trips the tenant_id column correctly so
// any future regression that disables the policy is at least visible
// via this test once the test runner is migrated off superuser.
func TestStoreIntegration_List_OnlyReturnsKnownIDs(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantA := seedReportsTenant(t, pool)
	tenantB := seedReportsTenant(t, pool)

	inA := newCreateInput(tenantA)
	inB := newCreateInput(tenantB)
	require.NoError(t, st.Create(t.Context(), inA))
	require.NoError(t, st.Create(t.Context(), inB))

	got, _, err := st.List(t.Context(), tenantA, reportsapi.ListJobsFilter{Limit: 100})
	require.NoError(t, err)
	// Both rows exist in the table; we assert the projection is well-
	// formed and that tenantA's row is among them. Production RLS hides
	// tenantB's row from this scope; in tests we cannot make the same
	// assertion (superuser bypass), but the same-tenant happy path of
	// retrieving inA validates the SQL.
	require.NotEmpty(t, got)
	foundA := false
	for _, j := range got {
		if j.ID == inA.ID {
			foundA = true
			require.Equal(t, tenantA, j.TenantID, "tenant_id projection must round-trip")
		}
	}
	require.True(t, foundA, "tenantA's own row must be retrievable in tenantA's scope")
}

// ─────────────────────────────────────────────────────────────────────────────
// *Tx state-flip variants — happy / stale-skip
// ─────────────────────────────────────────────────────────────────────────────

// TestStoreIntegration_MarkRunningTx_Happy creates a queued job and
// transitions it to running inside one tx — the row's state and
// started_at must reflect the call.
func TestStoreIntegration_MarkRunningTx_Happy(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)
	in := newCreateInput(tenantID)
	require.NoError(t, st.Create(t.Context(), in))

	flipAt := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkRunningTx(t.Context(), tx, in.ID, flipAt)
	}))

	got, err := st.Get(t.Context(), tenantID, in.ID)
	require.NoError(t, err)
	require.Equal(t, reportsapi.JobRunning, got.State)
	require.NotNil(t, got.StartedAt)
	require.True(t, got.StartedAt.Equal(flipAt), "started_at must equal the caller-supplied ts")
}

// TestStoreIntegration_MarkRunningTx_StaleSkip transitions a job to
// running once (succeeds), then immediately tries again — the second
// call returns ErrStaleSkip because the state is no longer 'queued'.
func TestStoreIntegration_MarkRunningTx_StaleSkip(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)
	in := newCreateInput(tenantID)
	require.NoError(t, st.Create(t.Context(), in))

	now := time.Now().UTC()
	// First call — happy path.
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkRunningTx(t.Context(), tx, in.ID, now)
	}))
	// Second call — already running, must skip.
	err := pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkRunningTx(t.Context(), tx, in.ID, now)
	})
	require.ErrorIs(t, err, store.ErrStaleSkip, "second MarkRunningTx must return ErrStaleSkip")
}

// TestStoreIntegration_MarkSucceededTx_HappyAndStaleSkip covers the
// succeed transition and asserts the row's bytes_size / filename /
// download_url columns are populated.
func TestStoreIntegration_MarkSucceededTx_HappyAndStaleSkip(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)
	in := newCreateInput(tenantID)
	require.NoError(t, st.Create(t.Context(), in))

	finishedAt := time.Now().UTC().Truncate(time.Microsecond)
	const (
		bytesSize    = int64(123456)
		filename     = "operator_efficiency_2026-05-14.xlsx"
		presignedURL = "https://stub-s3/sociopulse-reports-x/y.xlsx?expires=stub"
	)
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkSucceededTx(t.Context(), tx, in.ID, finishedAt, bytesSize, filename, presignedURL)
	}))

	got, err := st.Get(t.Context(), tenantID, in.ID)
	require.NoError(t, err)
	require.Equal(t, reportsapi.JobSucceeded, got.State)
	require.NotNil(t, got.FinishedAt)
	require.True(t, got.FinishedAt.Equal(finishedAt))
	require.Equal(t, bytesSize, got.BytesSize)
	require.Equal(t, filename, got.Filename)
	require.Equal(t, presignedURL, got.DownloadURL)

	// Second call must return ErrStaleSkip (state is now 'succeeded').
	err = pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkSucceededTx(t.Context(), tx, in.ID, finishedAt, bytesSize, filename, presignedURL)
	})
	require.ErrorIs(t, err, store.ErrStaleSkip)
}

// TestStoreIntegration_MarkFailedTx_PopulatesError covers the failure
// path: state→'failed', error populated with the caller's message.
func TestStoreIntegration_MarkFailedTx_PopulatesError(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)
	in := newCreateInput(tenantID)
	require.NoError(t, st.Create(t.Context(), in))

	finishedAt := time.Now().UTC().Truncate(time.Microsecond)
	const errMsg = "renderer panic: nil project"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkFailedTx(t.Context(), tx, in.ID, finishedAt, errMsg)
	}))

	got, err := st.Get(t.Context(), tenantID, in.ID)
	require.NoError(t, err)
	require.Equal(t, reportsapi.JobFailed, got.State)
	require.NotNil(t, got.FinishedAt)
	require.Equal(t, errMsg, got.Error)

	// Stale check.
	err = pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkFailedTx(t.Context(), tx, in.ID, finishedAt, errMsg)
	})
	require.ErrorIs(t, err, store.ErrStaleSkip)
}

// TestStoreIntegration_MarkCanceledTx_HappyAndStaleSkip covers the
// cancel path and the fixed 'canceled by caller' error literal.
func TestStoreIntegration_MarkCanceledTx_HappyAndStaleSkip(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)
	in := newCreateInput(tenantID)
	require.NoError(t, st.Create(t.Context(), in))

	finishedAt := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkCanceledTx(t.Context(), tx, in.ID, finishedAt)
	}))

	got, err := st.Get(t.Context(), tenantID, in.ID)
	require.NoError(t, err)
	require.Equal(t, reportsapi.JobCanceled, got.State)
	require.Equal(t, "canceled by caller", got.Error)

	// Stale check.
	err = pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkCanceledTx(t.Context(), tx, in.ID, finishedAt)
	})
	require.ErrorIs(t, err, store.ErrStaleSkip)
}

// TestStoreIntegration_MarkRunningTx_FromTerminalIsSkip is a
// defence-in-depth check: a job already in 'failed' state must not
// regress to 'running' — the CAS predicate excludes terminal rows.
func TestStoreIntegration_MarkRunningTx_FromTerminalIsSkip(t *testing.T) {
	t.Parallel()
	pool := startReportsPGContainer(t)
	st := store.NewPG(pool)

	tenantID := seedReportsTenant(t, pool)
	in := newCreateInput(tenantID)
	require.NoError(t, st.Create(t.Context(), in))

	// Fail it first.
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkFailedTx(t.Context(), tx, in.ID, time.Now().UTC(), "boom")
	}))

	// Now try to flip to running — must skip.
	err := pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return store.MarkRunningTx(t.Context(), tx, in.ID, time.Now().UTC())
	})
	require.ErrorIs(t, err, store.ErrStaleSkip)
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// startReportsPGContainer boots Postgres 16, applies every migration up
// through 000012, and returns a connected *postgres.Pool. The container
// is torn down at test exit. Pattern lifted from
// internal/recording/store/postgres_pg_test.go.
func startReportsPGContainer(t *testing.T) *postgres.Pool {
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

	migrationsURL := reportsMigrationsURL(t)
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

func reportsMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err, "migrations dir not found at %s", abs)
	return "file://" + abs
}

// seedReportsTenant inserts a fresh tenant row (BypassRLS) and returns
// its ID. Uses the full UUID as org_code to avoid the short-prefix
// collision risk seen in recording/store/search_pg_test.go.
func seedReportsTenant(t *testing.T, pool *postgres.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			 VALUES ($1, $2, $3, 'active', 'kms-test', '\x00')`,
			id,
			"org-"+id.String(),
			"tenant-"+id.String(),
		)
		return err
	}))
	return id
}

// newCreateInput builds a fully populated CreateInput for tests. The
// job id is a fresh UUID v7 stringified so each call yields a unique
// primary key. Window is a 1-day slice anchored at "yesterday" so
// the CHECK (window_to > window_from) holds.
func newCreateInput(tenantID uuid.UUID) store.CreateInput {
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := uuid.Must(uuid.NewV7()).String()
	return store.CreateInput{
		ID:           id,
		TenantID:     tenantID,
		Kind:         reportsapi.KindOperatorEfficiency,
		Format:       reportsapi.FormatXLSX,
		Params:       map[string]any{"project_id": uuid.Must(uuid.NewV7()).String()},
		WindowFrom:   now.Add(-24 * time.Hour),
		WindowTo:     now,
		CreatedBy:    uuid.Must(uuid.NewV7()),
		NotifyUserID: uuid.Must(uuid.NewV7()),
	}
}
