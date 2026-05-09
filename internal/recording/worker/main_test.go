package worker_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces goroutine quiescence on package exit. Run blocks; the
// integration tests cancel its ctx and wait for the goroutine to exit
// before TestMain runs goleak. A regression that leaks (e.g. a stray
// ticker without defer Stop) surfaces here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}
