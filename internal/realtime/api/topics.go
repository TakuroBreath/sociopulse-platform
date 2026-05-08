// topics.go declares the realtime Topic registry helpers + the audit
// TopicAction enum. The Topic constants themselves live in dto.go (next
// to the FrameKind / CloseReason enums) — this file holds the
// supplementary registry used by:
//
//   - input validation in WS subscribe/unsubscribe handlers,
//   - the per-topic RBAC matrix (service/rbac.go),
//   - the audit_log emit path that records every Listen-in start /
//     force-command publish.
//
// Adding a new Topic constant requires extending AllTopics — that
// requirement is enforced by the exhaustive test in topics_test.go.
package api

// AllTopics is the canonical list of recognized realtime topics. Used
// by the Hub's subscribe pre-flight to reject unknown topic strings
// with a 4xxx close before the RBAC matrix is consulted.
//
// Order matters for one place only: the metrics gauge
// realtime_subscriptions{topic=<x>} initialises every label to 0 in
// this order so dashboards never show a missing-label NaN.
var AllTopics = []Topic{
	TopicOperatorsState,
	TopicDialerQueue,
	TopicTrunksHealth,
	TopicCallEvents,
	TopicNotifications,
	TopicForceCommands,
}

// TopicAction enumerates the actions a connection may perform on a
// topic. Used as a low-cardinality Prometheus label and as an
// audit_log field.
//
// Adding a new constant requires extending Valid() — exhaustively
// tested in topics_test.go.
type TopicAction string

const (
	// ActionSubscribe is the inbound "client requests events" action.
	ActionSubscribe TopicAction = "subscribe"
	// ActionPublish is the outbound "server delivers events" action.
	// Reserved for future symmetric audit (currently the Hub only
	// dispatches via NATS; per-connection publish doesn't audit).
	ActionPublish TopicAction = "publish"
)

// Valid reports whether a is a recognized TopicAction. Used by audit
// emission + RBAC checks before writing the action label.
func (a TopicAction) Valid() bool {
	switch a {
	case ActionSubscribe, ActionPublish:
		return true
	}
	return false
}
