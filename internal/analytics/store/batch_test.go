package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/store"
	recordingapi "github.com/sociopulse/platform/internal/recording/api"
)

// TestInsertCalls_EmptySliceIsNoop pins the early-return contract: an
// empty rows slice MUST short-circuit before touching the driver so
// the helper is safe to call from an ingest flush loop that has just
// drained its buffer. The same guard applies to operator states and
// recordings uploaded — all three branch on len(rows) == 0 first.
//
// We pass a nil *store.Conn deliberately: if the early-return triggers
// before the driver dereference, the test passes without panicking.
func TestInsertCalls_EmptySliceIsNoop(t *testing.T) {
	t.Parallel()
	require.NoError(t, store.InsertCalls(t.Context(), nil, nil))
	require.NoError(t, store.InsertCalls(t.Context(), nil, []apianalytics.AnalyticsCallEventPayload{}))
}

// TestInsertOperatorStates_EmptySliceIsNoop mirrors the calls guard.
func TestInsertOperatorStates_EmptySliceIsNoop(t *testing.T) {
	t.Parallel()
	require.NoError(t, store.InsertOperatorStates(t.Context(), nil, nil))
	require.NoError(t, store.InsertOperatorStates(t.Context(), nil, []apianalytics.AnalyticsOperatorStateEventPayload{}))
}

// TestInsertRecordingsUploaded_EmptySliceIsNoop mirrors the calls guard.
func TestInsertRecordingsUploaded_EmptySliceIsNoop(t *testing.T) {
	t.Parallel()
	require.NoError(t, store.InsertRecordingsUploaded(t.Context(), nil, nil))
	require.NoError(t, store.InsertRecordingsUploaded(t.Context(), nil, []recordingapi.RecordingUploadedEvent{}))
}
