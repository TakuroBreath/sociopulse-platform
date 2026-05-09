package api_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// TestTopicClass_CriticalTopics locks in the security/billing-relevant
// topics that MUST NOT be dropped silently — overflow on these closes
// the connection so the client reconnects + re-fetches state via REST.
func TestTopicClass_CriticalTopics(t *testing.T) {
	t.Parallel()

	for _, topic := range []rtapi.Topic{
		rtapi.TopicCallEvents,
		rtapi.TopicForceCommands,
	} {
		got := rtapi.TopicClass(topic)
		assert.Equal(t, rtapi.FrameClassCritical, got,
			"topic %q must be classified Critical (drop-on-full closes connection)", topic)
	}
}

// TestTopicClass_TelemetryTopics locks in the remaining topics where
// drop-oldest is acceptable: every subsequent tick supersedes the
// previous payload, so a momentary buffer overflow is benign.
func TestTopicClass_TelemetryTopics(t *testing.T) {
	t.Parallel()

	for _, topic := range []rtapi.Topic{
		rtapi.TopicOperatorsState,
		rtapi.TopicDialerQueue,
		rtapi.TopicTrunksHealth,
		rtapi.TopicNotifications,
	} {
		got := rtapi.TopicClass(topic)
		assert.Equal(t, rtapi.FrameClassTelemetry, got,
			"topic %q must be classified Telemetry (drop-oldest)", topic)
	}
}

// TestTopicClass_AllTopicsCovered guarantees every entry in AllTopics
// has an explicit classification. A future plan that adds a new Topic
// to the registry but forgets to extend TopicClass surfaces here as a
// failure on the new topic's zero-value classification.
func TestTopicClass_AllTopicsCovered(t *testing.T) {
	t.Parallel()

	for _, topic := range rtapi.AllTopics {
		got := rtapi.TopicClass(topic)
		// FrameClassUnknown means "topic was added to AllTopics but
		// not classified" — see the doc comment on FrameClassUnknown.
		assert.NotEqual(t, rtapi.FrameClassUnknown, got,
			"topic %q in AllTopics must have an explicit FrameClass", topic)
	}
}

// TestTopicClass_ZeroValueIsUnknown verifies the package's defensive
// classification: an unrecognised topic (zero-value or arbitrary
// string) returns FrameClassUnknown so the Connection can disconnect
// rather than silently route to the wrong queue.
func TestTopicClass_ZeroValueIsUnknown(t *testing.T) {
	t.Parallel()

	assert.Equal(t, rtapi.FrameClassUnknown, rtapi.TopicClass(""))
	assert.Equal(t, rtapi.FrameClassUnknown, rtapi.TopicClass("not.a.real.topic"))
}
