package outbox_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/outbox"
)

// TestEvent_ZeroValue exercises the zero-value Event so future field
// additions don't accidentally introduce non-nil-safe defaults.
func TestEvent_ZeroValue(t *testing.T) {
	t.Parallel()

	var e outbox.Event
	require.Zero(t, e.ID)
	require.Nil(t, e.TenantID)
	require.Nil(t, e.AggregateID)
	require.Empty(t, e.Subject)
	require.Empty(t, e.Payload)
	require.True(t, e.CreatedAt.IsZero())
	require.Nil(t, e.PublishedAt)
	require.Nil(t, e.LastError)
	require.Zero(t, e.Attempts)
}

// TestEvent_PointerFieldsAreOptional documents that TenantID/AggregateID
// are pointers so callers can encode "platform-global" / "no aggregate"
// events without smuggling sentinel UUIDs through the schema.
func TestEvent_PointerFieldsAreOptional(t *testing.T) {
	t.Parallel()

	tid := uuid.New()
	aid := uuid.New()
	e := outbox.Event{
		TenantID:    &tid,
		AggregateID: &aid,
		Subject:     "x.y.z",
		Payload:     []byte(`{}`),
	}
	require.Equal(t, tid, *e.TenantID)
	require.Equal(t, aid, *e.AggregateID)
}
