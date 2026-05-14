package store_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak for both unit and integration runs. The pgxpool
// background health check is ignored — it's a known long-lived goroutine
// the pool owns and that goleak would otherwise flag (same ignore the
// recording store uses, see internal/recording/store/main_test.go).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}
