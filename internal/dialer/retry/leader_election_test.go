//go:build integration

// leader_election_test.go drives PgLeader against a real Postgres 16
// container to prove the production wiring honours the contracts the
// orchestrator depends on:
//
//   - Two PgLeader instances racing → exactly one wins; the loser sees
//     ok=false (NOT a queue-and-deadlock).
//   - Releasing the lock lets the loser take over.
//   - The TestMain for the integration build runs goleak.VerifyTestMain
//     across every test so a leaked goroutine surfaces here.
//
// Build tag `integration` keeps the testcontainer overhead out of the
// default test run; CI invokes `go test -tags=integration ./...` for
// the integration target.
package retry_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/retry"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TestMain runs goleak.VerifyTestMain across the integration suite so
// any goroutine spawned by go-redis or testcontainers is detected at
// exit. See main_test.go for the unit-build counterpart (gated by
// !integration).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// pgxpool spawns a background health-check goroutine when a pool
		// is opened; it terminates on Pool.Close, but leak detection at
		// Exit-time can fire if a t.Cleanup is mid-flight. We allow it
		// the same way pkg/postgres' own test main does.
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}

// startPG boots Postgres 16 in a container, applies all project
// migrations, and returns a connected *postgres.Pool. The migrations
// are needed because the orchestrator integration test reads the
// `respondents` table; the leader-only test re-uses the same helper
// for symmetry.
func startPG(t *testing.T) *postgres.Pool {
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

// repoMigrationsURL returns the file:// URL of the repo's migrations
// dir, resolved relative to this test file's location.
func repoMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err)
	return "file://" + abs
}

// TestIntegration_PgLeader_OnlyOneAcquires races two PgLeader instances
// over the same key. Exactly one Acquire returns ok=true; the other
// returns ok=false without error.
func TestIntegration_PgLeader_OnlyOneAcquires(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)
	logger := zaptest.NewLogger(t)
	ctx := context.Background()

	const key = int64(0x1234567890abcdef)
	a, err := retry.NewPgLeader(pool, key, logger.Named("a"))
	require.NoError(t, err)
	b, err := retry.NewPgLeader(pool, key, logger.Named("b"))
	require.NoError(t, err)
	t.Cleanup(func() {
		a.Release(context.Background())
		b.Release(context.Background())
	})

	// Race the two Acquires. With pg_try_advisory_lock the loser sees
	// ok=false (NOT a queue or deadlock).
	type result struct {
		owner string
		ok    bool
		err   error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ok, err := a.Acquire(ctx)
		results <- result{owner: "a", ok: ok, err: err}
	}()
	go func() {
		defer wg.Done()
		ok, err := b.Acquire(ctx)
		results <- result{owner: "b", ok: ok, err: err}
	}()
	wg.Wait()
	close(results)

	var leader, follower string
	wins := 0
	for r := range results {
		require.NoError(t, r.err)
		if r.ok {
			wins++
			leader = r.owner
		} else {
			follower = r.owner
		}
	}
	require.Equal(t, 1, wins, "exactly one PgLeader must hold the lock")
	require.NotEmpty(t, leader)
	require.NotEmpty(t, follower)

	// IsLeading reflects the post-Acquire state.
	if leader == "a" {
		require.True(t, a.IsLeading())
		require.False(t, b.IsLeading())
	} else {
		require.True(t, b.IsLeading())
		require.False(t, a.IsLeading())
	}
}

// TestIntegration_PgLeader_ReleasePassesLockToPeer — leader Releases,
// peer Acquires successfully on the next attempt.
func TestIntegration_PgLeader_ReleasePassesLockToPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)
	logger := zaptest.NewLogger(t)
	ctx := context.Background()

	const key = int64(0x4bcdef1234567890)
	a, err := retry.NewPgLeader(pool, key, logger.Named("a"))
	require.NoError(t, err)
	b, err := retry.NewPgLeader(pool, key, logger.Named("b"))
	require.NoError(t, err)
	t.Cleanup(func() {
		a.Release(context.Background())
		b.Release(context.Background())
	})

	// A acquires.
	got, err := a.Acquire(ctx)
	require.NoError(t, err)
	require.True(t, got)
	require.True(t, a.IsLeading())

	// B can't acquire while A holds it.
	got, err = b.Acquire(ctx)
	require.NoError(t, err)
	require.False(t, got)
	require.False(t, b.IsLeading())

	// A releases.
	a.Release(ctx)
	require.False(t, a.IsLeading())

	// B can now acquire.
	got, err = b.Acquire(ctx)
	require.NoError(t, err)
	require.True(t, got)
	require.True(t, b.IsLeading())
}

// TestIntegration_PgLeader_AcquireIsIdempotent — a second Acquire by
// the holding instance returns ok=true without touching the lock.
func TestIntegration_PgLeader_AcquireIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)
	logger := zaptest.NewLogger(t)
	ctx := context.Background()

	a, err := retry.NewPgLeader(pool, 0, logger)
	require.NoError(t, err)
	t.Cleanup(func() { a.Release(context.Background()) })

	got, err := a.Acquire(ctx)
	require.NoError(t, err)
	require.True(t, got)

	got, err = a.Acquire(ctx)
	require.NoError(t, err)
	require.True(t, got, "second Acquire on holding instance is a no-op")
}

// TestIntegration_PgLeader_KeyReturnsConfigured — Key() exposes the
// effective lock key for ops dashboards. A custom non-zero key passes
// through; a zero key falls back to DefaultLockKey.
func TestIntegration_PgLeader_KeyReturnsConfigured(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)

	const custom = int64(0x1122334455667788)
	a, err := retry.NewPgLeader(pool, custom, nil)
	require.NoError(t, err)
	require.Equal(t, custom, a.Key())

	b, err := retry.NewPgLeader(pool, 0, nil)
	require.NoError(t, err)
	require.Equal(t, retry.DefaultLockKey, b.Key())
}

// TestIntegration_PgLeader_ReleaseWithoutAcquireIsNoOp — Release on a
// non-leading instance must not panic.
func TestIntegration_PgLeader_ReleaseWithoutAcquireIsNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)

	a, err := retry.NewPgLeader(pool, 0, zaptest.NewLogger(t))
	require.NoError(t, err)

	// Release without any Acquire — should not panic, should not error.
	a.Release(context.Background())
	require.False(t, a.IsLeading())
}
