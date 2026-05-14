package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/analytics/api"
)

// TestSubjectConstants_Canonical locks the canonical NATS subjects the
// analytics ingester binds against. These match:
//
//   - dialer emits analytics.event.calls and analytics.event.operator_state
//     (Plan 13.2 § Q7).
//   - recording emits tenant.<t>.recording.uploaded; analytics subscribes
//     via the cross-tenant wildcard tenant.*.recording.uploaded
//     (Plan 13.2 § Q4).
//
// Drift in either direction (this Go constant or the dialer/recording
// publisher) breaks ingest. Keep this test loud.
func TestSubjectConstants_Canonical(t *testing.T) {
	t.Parallel()

	require.Equal(t, "analytics.event.calls", api.SubjectCallsAnalytics)
	require.Equal(t, "analytics.event.operator_state", api.SubjectOperatorStateAnalytics)
	require.Equal(t, "tenant.*.recording.uploaded", api.SubjectRecordingUploadedWildcard)
}
