package api_test

import (
	"context"
	"testing"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// fakeCallResolver is a compile-time conformance probe for the new
// rtapi.CallResolver interface — if the interface signature drifts,
// this test file will stop compiling.
type fakeCallResolver struct{}

func (fakeCallResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, nil
}

var _ rtapi.CallResolver = fakeCallResolver{}

// TestCallResolver_InterfaceShape is a runtime no-op — the actual
// guarantee is the compile-time `var _ rtapi.CallResolver` line above.
// The function exists so `go test` reports the file as "tested".
func TestCallResolver_InterfaceShape(t *testing.T) {
	t.Parallel()
	var _ rtapi.CallResolver = fakeCallResolver{}
}
