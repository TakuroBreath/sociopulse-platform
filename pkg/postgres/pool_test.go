package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPostgresCompiles is a placeholder smoke test that validates the
// package compiles and the pgx re-exports stay aligned with the
// upstream types. Real pool tests run with testcontainers in Plan 03
// Task 4.
func TestPostgresCompiles(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrNoRows, pgx.ErrNoRows) {
		t.Fatalf("ErrNoRows must alias pgx.ErrNoRows")
	}
}
