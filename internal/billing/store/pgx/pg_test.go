//go:build integration

package pgx_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TestPG_GetSetting_NoRow_ReturnsErrNoRows verifies the canonical
// sentinel for the absent-key case — the service layer relies on
// errors.Is(err, pgx.ErrNoRows) for fallback-to-default behaviour.
func TestPG_GetSetting_NoRow_ReturnsErrNoRows(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid := seedBillingTenant(t, pool)

	_, err := store.GetSetting(t.Context(), tid, "billing.trunks")
	require.ErrorIs(t, err, pgxv5.ErrNoRows)
}

// TestPG_UpsertSettings_AtomicMulti exercises the multi-key write path:
// a single Upsert persists both the trunk-cost map and the wage scalar,
// and a subsequent GetSetting returns each key's raw JSON payload.
func TestPG_UpsertSettings_AtomicMulti(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid := seedBillingTenant(t, pool)

	kv := map[string][]byte{
		"billing.trunks":          []byte(`{"mtt-msk-1": 342}`),
		"billing.wage_per_survey": []byte(`{"value": 12000}`),
	}
	require.NoError(t, store.UpsertSettings(t.Context(), tid, kv))

	raw, err := store.GetSetting(t.Context(), tid, "billing.trunks")
	require.NoError(t, err)
	var trunks map[string]int64
	require.NoError(t, json.Unmarshal(raw, &trunks))
	require.Equal(t, int64(342), trunks["mtt-msk-1"])

	raw, err = store.GetSetting(t.Context(), tid, "billing.wage_per_survey")
	require.NoError(t, err)
	var wage struct {
		Value int64 `json:"value"`
	}
	require.NoError(t, json.Unmarshal(raw, &wage))
	require.Equal(t, int64(12000), wage.Value)
}

// TestPG_UpsertSettings_Idempotent verifies the ON CONFLICT update path:
// a second Upsert with the same key overwrites the value (not inserts a
// duplicate row, which would violate the (tenant_id, key) primary key).
func TestPG_UpsertSettings_Idempotent(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid := seedBillingTenant(t, pool)

	require.NoError(t, store.UpsertSettings(t.Context(), tid, map[string][]byte{
		"billing.wage_per_survey": []byte(`{"value": 10000}`),
	}))
	require.NoError(t, store.UpsertSettings(t.Context(), tid, map[string][]byte{
		"billing.wage_per_survey": []byte(`{"value": 11000}`),
	}))

	raw, err := store.GetSetting(t.Context(), tid, "billing.wage_per_survey")
	require.NoError(t, err)
	var wage struct {
		Value int64 `json:"value"`
	}
	require.NoError(t, json.Unmarshal(raw, &wage))
	require.Equal(t, int64(11000), wage.Value, "second upsert must overwrite the first")
}

// TestPG_UpsertSettings_EmptyMapIsNoOp checks the early-return path: an
// empty kv must not open a transaction or error.
func TestPG_UpsertSettings_EmptyMapIsNoOp(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid := seedBillingTenant(t, pool)

	require.NoError(t, store.UpsertSettings(t.Context(), tid, nil))
	require.NoError(t, store.UpsertSettings(t.Context(), tid, map[string][]byte{}))
}

// ─── Test fixtures ───────────────────────────────────────────────────────────

// startBillingPGContainer spins a postgres:16-alpine container, applies
// every repo migration via golang-migrate, opens a *postgres.Pool, and
// registers Cleanup hooks so the container is torn down at test exit.
// Pattern lifted from internal/reports/store/pg_pg_test.go.
func startBillingPGContainer(t *testing.T) *postgres.Pool {
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

	mig, err := migrate.New(billingMigrationsURL(t), dsn)
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

// billingMigrationsURL resolves the absolute path to the migrations
// directory relative to this file. file:// URL is what golang-migrate
// expects.
func billingMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err, "migrations dir not found at %s", abs)
	return "file://" + abs
}

// seedBillingTenant inserts a fresh tenant via BypassRLS so subsequent
// WithTenant writes have a valid foreign-key target. Returns the new
// tenant id.
func seedBillingTenant(t *testing.T, pool *postgres.Pool) uuid.UUID {
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
