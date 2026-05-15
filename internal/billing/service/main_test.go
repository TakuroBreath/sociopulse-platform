package service_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the billing service
// test binary. The service-layer code is synchronous: CostCalculator,
// TariffStore, SpendReport, MarginReport, and OnCallFinalized all run
// inline within the caller's goroutine. A regression that spawns a
// background goroutine without joining it surfaces here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
