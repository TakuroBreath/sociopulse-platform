//go:build integration

package worker_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces goroutine quiescence on package exit. The worker
// package's tests are integration-tagged (testcontainers Postgres), so
// this file is integration-tagged too — mirrors the pattern in
// internal/dialer/retry/main_test.go where the unit and integration
// builds source distinct TestMain wrappers.
//
// Tests that exercise Run drive it via t.Context() and rely on the
// caller-side cancel + Run goroutine exit before TestMain runs goleak.
// Coverage is best-effort: a leaked ticker without defer Stop in Run
// itself surfaces here, but a goroutine leak created and joined inside
// a test (i.e. that already exited before TestMain runs goleak) does
// not. Tests that need stricter guarantees should add their own
// goleak.VerifyNone at scope.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}
