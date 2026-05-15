package pgx_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak for both unit and integration runs. The pgxpool
// background health-check goroutine is a known long-lived goroutine the
// pool owns; ignoring it matches the precedent in
// internal/reports/store/main_test.go and internal/recording/store/main_test.go.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}
