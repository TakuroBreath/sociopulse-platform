//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/postgres"
)

// TestTx_QueryRoundtrip exercises Tx.Exec + Tx.Query inside WithTenant.
// It writes through one transaction and reads through another so the
// commit boundary is genuinely crossed.
func TestTx_QueryRoundtrip(t *testing.T) {
	dsn := startPG(t)
	ctx := context.Background()
	pool, err := postgres.Open(ctx, postgres.Config{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.RawExec(ctx, "create table notes (id int primary key, body text)")
	require.NoError(t, err)

	tenantID := uuid.New()
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, "insert into notes (id, body) values ($1, $2)", 1, "hello")
		return err
	}))

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var body string
		if err := tx.QueryRow(ctx, "select body from notes where id = $1", 1).Scan(&body); err != nil {
			return err
		}
		require.Equal(t, "hello", body)
		return nil
	}))
}
