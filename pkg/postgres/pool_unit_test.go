package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/postgres"
)

// TestPostgresCompiles is a smoke test that validates the package
// compiles and the pgx re-export of ErrNoRows stays aligned with the
// upstream value.
func TestPostgresCompiles(t *testing.T) {
	t.Parallel()

	if !errors.Is(postgres.ErrNoRows, pgx.ErrNoRows) {
		t.Fatalf("ErrNoRows must alias pgx.ErrNoRows")
	}
}

// TestOpen_RejectsEmptyDSN verifies the constructor refuses to dial when
// the DSN is empty: tests that bypass the validation would silently pass
// because pgxpool.ParseConfig accepts the zero string in some versions.
func TestOpen_RejectsEmptyDSN(t *testing.T) {
	t.Parallel()

	_, err := postgres.Open(context.Background(), postgres.Config{DSN: ""})
	require.Error(t, err)
}

// TestOpen_RejectsMalformedDSN exercises the parse-failure branch. The
// DSN has an obviously invalid scheme so pgxpool.ParseConfig returns an
// error wrapped with our prefix.
func TestOpen_RejectsMalformedDSN(t *testing.T) {
	t.Parallel()

	_, err := postgres.Open(context.Background(), postgres.Config{DSN: "::not a dsn::"})
	require.Error(t, err)
}

// TestPool_CloseIsIdempotentOnNil ensures Close on a *Pool whose
// underlying pgxpool was never opened (nil receiver path) does not panic.
// This simplifies error-cleanup paths in callers.
func TestPool_CloseIsIdempotentOnNil(t *testing.T) {
	t.Parallel()

	var p *postgres.Pool
	require.NotPanics(t, func() { p.Close() })
}

// TestWithTenant_RejectsZeroUUID guards the contract that
// WithTenant refuses uuid.Nil up front, before any database round-trip.
// We can verify this without a database because the validation runs
// before BeginTx.
func TestWithTenant_RejectsZeroUUID(t *testing.T) {
	t.Parallel()

	// Pool with no underlying pgxpool — call must error before any DB
	// access happens. The Open code path is exercised in the integration
	// tests; here we only assert the argument-validation contract.
	var p postgres.Pool
	err := p.WithTenant(context.Background(), uuid.Nil, func(tx postgres.Tx) error {
		t.Fatal("fn must not run on uuid.Nil")
		return nil
	})
	require.Error(t, err)
}
