//go:build integration

package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	// Native database/sql driver registration. golang-migrate's CH
	// driver registers its own driver via init(); this blank import
	// matches the cmd/migrator integration suite so the helpers stay
	// self-contained.
	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/clickhouse"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/require"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
)

// chImage pins ClickHouse server to 24.8 — matches the cmd/migrator
// suite (Plan 13.1) and Yandex Managed CH supported versions. Bumping
// this floats the test against whatever's :latest, breaking
// reproducibility.
const chImage = "clickhouse/clickhouse-server:24.8"

// chDSNs bundles the two DSNs a CH integration test needs. The pair is
// the documented mitigation for Plan 13.1 production lesson #4:
// x-multi-statement=true is a golang-migrate-only extension and
// clickhouse-go/v2.ParseDSN rejects it with "unexpected key".
type chDSNs struct {
	// migrate carries x-multi-statement=true and is the DSN passed to
	// migrate.New. It is NOT valid for clickhouse-go's native open.
	migrate string
	// verify is the bare DSN suitable for clickhouse-go's native
	// driver. The store.Open path uses this one.
	verify string
}

// startCH boots a fresh ClickHouse container, returns the DSN pair,
// and registers t.Cleanup so the container is reaped at test end.
//
// The container takes ~10s cold-start; tests that share a container
// should call this in a TestMain or sync.Once. For now we follow the
// cmd/migrator suite and accept the per-test startup — it lets every
// test run with a fully clean database, which is what we want for
// round-trip assertions.
func startCH(t *testing.T) chDSNs {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	ch, err := tcclickhouse.Run(ctx, chImage,
		tcclickhouse.WithDatabase("sociopulse_test"),
		tcclickhouse.WithUsername("test"),
		tcclickhouse.WithPassword("test"),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = ch.Terminate(context.Background())
	})

	migrateDSN, err := ch.ConnectionString(ctx, "x-multi-statement=true")
	require.NoError(t, err)
	verifyDSN, err := ch.ConnectionString(ctx)
	require.NoError(t, err)
	return chDSNs{migrate: migrateDSN, verify: verifyDSN}
}

// migrateUp applies every migration in migrations/clickhouse against
// the supplied (migrate-flavoured) DSN. Tests run from
// internal/analytics/store/, so the migrations directory is three
// levels up.
//
// We bind to golang-migrate's filesystem source so the test always
// drives the real migration files on disk — a future schema change
// that breaks the wrapper or batch helpers fails here, not silently
// in production.
func migrateUp(t *testing.T, dsn string) {
	t.Helper()
	absMigrations, err := filepath.Abs(filepath.Join("..", "..", "..", "migrations", "clickhouse"))
	require.NoError(t, err)

	m, err := migrate.New("file://"+absMigrations, dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		// Source/database close errors are deliberately ignored — the
		// container is being torn down on the parent t.Cleanup anyway.
		_, _ = m.Close()
	})
	require.NoError(t, m.Up())
}
