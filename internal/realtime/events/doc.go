// Package events is the realtime module's NATS-to-Hub dispatcher. A
// single *NATSSubscriber per cmd/api replica subscribes to the canonical
// tenant.<t>.> subject family and translates each delivery into a
// Hub.Broadcast call.
//
// # Subject → topic mapping
//
//	tenant.<id>.dialer.op.<op>.state           → TopicOperatorsState
//	tenant.<id>.dialer.queue                   → TopicDialerQueue
//	tenant.<id>.telephony.event.<call>.<phase> → TopicCallEvents
//	tenant.<id>.notify.user.<user>             → TopicNotifications
//	tenant.<id>.force.user.<user>              → TopicForceCommands
//
// The trunks.health subject (no tenant) is intentionally NOT wired in
// this layer — Hub.Broadcast refuses dispatches with an empty TenantID
// (cross-tenant leak guard). Plan 11 Task 7's HTTP layer fans
// trunks.health out per-tenant via a separate path.
//
// # Replica fan-out
//
// Each replica registers its subscriptions under a queue group named
// "realtime-replica-<replicaID>". Because every replica uses a unique
// replicaID, the JetStream queue-group semantics degenerate to "every
// replica receives every message" — which is exactly what we want, since
// each replica must dispatch to its own local WebSocket connections
// (Plan 11 Decision Q2).
//
// # PII discipline
//
// Subjects and payloads can carry tenant IDs, operator IDs and user IDs.
// The dispatcher logs only the subject + payload byte count, and only at
// debug level. Full payloads are NEVER logged.
//
// # Concurrency
//
// *NATSSubscriber holds a Mutex over its lifecycle state (started /
// stopped flags + subscription metadata). Once Start returns, the
// underlying eventbus.Subscriber owns the goroutines that deliver
// messages — this package spawns none of its own.
package events
