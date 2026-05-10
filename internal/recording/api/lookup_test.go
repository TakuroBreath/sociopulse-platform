package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// fakeCallTenantLookup is a compile-time conformance probe.
type fakeCallTenantLookup struct{}

func (fakeCallTenantLookup) LookupTenant(_ context.Context, _ uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, nil
}

var _ rapi.CallTenantLookup = fakeCallTenantLookup{}

// TestCallTenantLookup_InterfaceShape — runtime no-op, real assertion
// is the compile-time `var _` above.
func TestCallTenantLookup_InterfaceShape(t *testing.T) {
	t.Parallel()
	var _ rapi.CallTenantLookup = fakeCallTenantLookup{}
}
