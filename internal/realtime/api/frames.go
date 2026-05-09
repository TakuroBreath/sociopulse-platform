// frames.go declares the FrameClass enum and the Topic → class map
// consumed by Connection.Send to route critical vs. telemetry frames
// onto separate per-connection queues. Critical frames overflowing
// their bounded queue close the connection (the client reconnects and
// re-fetches via REST); telemetry frames overflow into drop-oldest.
//
// Adding a new Topic constant requires extending TopicClass — the
// exhaustive test in frames_test.go enforces this.
package api

// FrameClass is the per-frame priority class consulted by
// Connection.Send to decide queue routing + overflow policy.
//
// Zero value is FrameClassUnknown on purpose: a topic that wasn't
// classified produces a deliberately observable signal (fail loud)
// instead of silently routing to the wrong lane.
type FrameClass int

const (
	// FrameClassUnknown is the zero value. Returned by TopicClass for
	// topics not in the explicit switch — Connection.Send treats this
	// as a contract violation and closes with CloseProtocolErr.
	FrameClassUnknown FrameClass = iota

	// FrameClassCritical is reserved for frames where silent drop is
	// unacceptable. The Connection routes these onto a small bounded
	// queue (criticalQueueSize=32) and closes the connection on
	// overflow with CloseRateLimited so the client reconnects and
	// re-fetches state via REST. Better an explicit reconnect than
	// quietly missing a Hangup or a force-pause command.
	FrameClassCritical

	// FrameClassTelemetry is the default class for periodic state
	// updates where the next tick supersedes the previous payload.
	// Drop-oldest is acceptable: a missed operators.state tick is
	// immediately overwritten by the following one. Connection.Send
	// routes these onto cfg.WriteBufferSize-deep telemetryCh.
	FrameClassTelemetry
)

// TopicClass returns the priority class for the supplied Topic.
//
// Critical topics:
//   - TopicCallEvents: per-call telemetry incl. Hangup; billing UI
//     must observe every event or it shows the wrong call duration.
//   - TopicForceCommands: admin-issued force-end-shift / force-pause;
//     compliance requires the operator to receive these.
//
// Telemetry topics: TopicOperatorsState, TopicDialerQueue,
// TopicTrunksHealth, TopicNotifications — periodic; next tick
// replaces.
//
// Unknown topic: returns FrameClassUnknown. Connection.Send uses this
// to fail loud (close + log) rather than silently route to telemetry.
func TopicClass(t Topic) FrameClass {
	switch t {
	case TopicCallEvents, TopicForceCommands:
		return FrameClassCritical
	case TopicOperatorsState, TopicDialerQueue, TopicTrunksHealth, TopicNotifications:
		return FrameClassTelemetry
	default:
		return FrameClassUnknown
	}
}
