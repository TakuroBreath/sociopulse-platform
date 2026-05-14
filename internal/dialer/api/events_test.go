package api_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// TestSubjectCallFinalizedFor_Canonical pins the per-tenant
// dialer.call.finalized subject helper. The format must match the
// pattern the outbox-relay binds to and the analytics ingester's
// tenant.<t>.* wildcard expects.
func TestSubjectCallFinalizedFor_Canonical(t *testing.T) {
	t.Parallel()

	tenantID := uuid.MustParse("01ABCDEF-0123-7456-89AB-0123456789AB")
	require.Equal(t,
		"tenant."+tenantID.String()+".dialer.call.finalized",
		api.SubjectCallFinalizedFor(tenantID))
}
