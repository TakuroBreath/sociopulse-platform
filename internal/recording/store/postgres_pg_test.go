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

	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

func TestPostgresStore_InsertIdempotent_FreshRow(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)

	s := store.NewPostgresStore(pool)
	row := newRow(t, tenantID, callID)

	var got store.RecordingRow
	var replay bool
	err := pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		var err error
		got, replay, err = s.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	})
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, row.ID, got.ID)
	require.Equal(t, row.CallID, got.CallID)
}

func TestPostgresStore_InsertIdempotent_DuplicateReturnsReplay(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)

	s := store.NewPostgresStore(pool)
	first := newRow(t, tenantID, callID)
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := s.InsertRecordingIdempotent(t.Context(), tx, first)
		return err
	}))

	dup := first
	dup.ID = uuid.Must(uuid.NewV7())
	dup.S3Bucket = "different-bucket"

	var got store.RecordingRow
	var replay bool
	err := pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		var err error
		got, replay, err = s.InsertRecordingIdempotent(t.Context(), tx, dup)
		return err
	})
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, first.ID, got.ID)
	require.Equal(t, first.S3Bucket, got.S3Bucket)
}

func TestPostgresStore_InsertIdempotent_CallNotFound(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)

	s := store.NewPostgresStore(pool)
	row := newRow(t, tenantID, uuid.Must(uuid.NewV7())) // call never seeded

	err := pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := s.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	})
	require.ErrorIs(t, err, store.ErrCallNotFound)
}

func TestPostgresStore_GetByCallID_Found(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)

	s := store.NewPostgresStore(pool)
	row := newRow(t, tenantID, callID)
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := s.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	got, err := s.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, row.ID, got.ID)
	require.Equal(t, row.SHA256Hex, got.SHA256Hex)
}

func TestPostgresStore_GetByCallID_NotFound(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)

	s := store.NewPostgresStore(pool)
	_, err := s.GetByCallID(t.Context(), tenantID, uuid.Must(uuid.NewV7()))
	require.ErrorIs(t, err, store.ErrCallNotFound)
}

// ────────── helpers (one container per test for parallelism + isolation) ──────────

// startPGContainer boots Postgres 16 in a container, applies migrations, and
// returns a connected *postgres.Pool. Pattern lifted from
// internal/dialer/fsm/audit_pg_test.go.
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

func repoMigrationsURL(t *testing.T) string {
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

// seedTenant inserts a fresh tenant row and returns its ID. Uses BypassRLS so
// the test can write to the tenants table directly.
//
// The tenants table (from migrations/000001_init.up.sql) requires:
// org_code (unique text), name, status ('active'|'suspended'|'archived'),
// kms_kek_id, phone_hash_pepper (bytea).
func seedTenant(t *testing.T, pool *postgres.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			 VALUES ($1, $2, $3, 'active', 'kms-test', '\x00')`,
			id,
			"org-"+id.String()[:8],
			"tenant-"+id.String()[:8],
		)
		return err
	}))
	return id
}

// seedCall inserts a tenant, a project, and a calls row — returning both
// tenantID and callID.
//
// tenants is owned by tenancy_admin and is inserted via BypassRLS.
// projects and calls are RLS-protected with a tenant_id isolation policy and
// must be inserted via WithTenant (which sets app.tenant_id) — just like the
// canonical seedPrereqRows in internal/dialer/fsm/audit_pg_test.go does.
//
// projects table (migrations/000001_init.up.sql) requires:
//
//	id, tenant_id, code (unique per tenant), name, status ('active'|'paused'|'archived').
//
// calls table requires:
//
//	id, tenant_id, project_id, status — valid status values: 'in-progress',
//	'success', 'refused', 'dropped', 'no-answer', 'busy', 'callback',
//	'wrong-person', 'tech-failure'. ('completed' is NOT a valid value.)
func seedCall(t *testing.T, pool *postgres.Pool) (tenantID, callID uuid.UUID) {
	t.Helper()
	tenantID = seedTenant(t, pool)
	callID = uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())

	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status)
			 VALUES ($1, $2, $3, 'Test Project', 'active')`,
			projectID, tenantID, "proj-"+projectID.String()[:8],
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
	return tenantID, callID
}

// newRow builds a fully populated RecordingRow for tests.
func newRow(t *testing.T, tenantID, callID uuid.UUID) store.RecordingRow {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	return store.RecordingRow{
		ID:             uuid.Must(uuid.NewV7()),
		CallID:         callID,
		TenantID:       tenantID,
		S3Bucket:       "rec-bucket-1",
		AudioObjectKey: "recordings/x/x/x/x.opus.enc",
		KMSKeyID:       "kms-key-1",
		EncryptedDEK:   []byte("encrypted-dek-stub-32bytes-xxxxx"),
		BytesSize:      1234567,
		DurationMS:     12345,
		SHA256Hex:      "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100",
		Codec:          "opus",
		SampleRate:     48000,
		Status:         "stored",
		CommittedAt:    now,
		DeleteAt:       now.Add(730 * 24 * time.Hour),
		ColdAt:         now.Add(365 * 24 * time.Hour),
		RecordedAt:     now.Add(-1 * time.Hour),
		IngestAgentID:  "agent-test",
	}
}
