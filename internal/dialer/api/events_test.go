package api_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// TestSubjectAnalyticsConstants_Canonical pins the cross-tenant
// analytics subject literals the dialer publishes on EventStatusSubmitted
// (and the audit path's secondary outbox row). Plan 13.2 § Q7 fixes them;
// drift here breaks analytics ingest silently.
func TestSubjectAnalyticsConstants_Canonical(t *testing.T) {
	t.Parallel()

	require.Equal(t, "analytics.event.calls", api.SubjectAnalyticsCalls)
	require.Equal(t, "analytics.event.operator_state", api.SubjectAnalyticsOperatorState)
}

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
