// Package router implements api.Router from internal/dialer/api — the
// dialer's NATS abstraction in front of the telephony bridge.
//
// # "dialer Router" vs "telephony Router"
//
// The codebase has two distinct things named Router. They serve different
// purposes; do not confuse them:
//
//  1. internal/telephony/router (Plan 09 Task 5) — selects the
//     {fs_node, trunk} pair for an outbound originate. It implements
//     telephony.api.Router.Select. Lives inside the telephony bridge
//     binary; talks directly to ESL via the pool.
//
//  2. internal/dialer/router (THIS PACKAGE, Plan 10 Task 5) — a thin
//     NATS-abstraction adapter the dialer uses to issue
//     Originate/Hangup commands and to receive ChannelEvent updates.
//     It implements dialer.api.Router (Dial / Hangup / Subscribe). It
//     does NOT touch ESL directly — it forwards every call to a
//     telephony.api.CommandPublisher (typically the Plan 11 NATS-backed
//     publisher; today the cmd/api locator wires the Plan 09 stub that
//     returns ErrTelephonyBridgeOffline).
//
// The dialer Router is intentionally trivial: there is no Redis, no
// goroutines, no buffering. It exists so the dialer's call sites (FSM
// transitions, retry orchestrator) talk to a single dialer-shaped
// interface instead of importing telephony/api directly. That keeps
// the dialer ↔ telephony seam thin and lets Plan 11 swap the underlying
// NATS publisher without touching any dialer call site.
//
// # Translation
//
// Two boundaries are crossed:
//
//   - dialer.api.DialRequest → telephony.api.OriginateCommand. Pass-
//     through of the shared fields (CallID, TenantID, OperatorExt,
//     Phone→Number, FsNode→FSNode). A fresh CommandID (uuid.New()) is
//     stamped per call — the bridge's Redis SETNX uses it for
//     idempotency, so a retry from the dialer's caller MUST get a fresh
//     CommandID, not a replay of the previous one.
//
//   - telephony.api.ChannelEvent → dialer.api.ChannelEvent. Telephony's
//     7-state event family collapses into the dialer's 3-state view:
//     "dialing" (one-to-one), "answered" (folds answer + bridge), and
//     "hangup" (one-to-one). Unbridge / DTMF / RecordStop are dropped
//     at the translator and never reach the dialer's handler — they
//     belong to mediation / recording layers, not the dialer's lifecycle
//     view. The translator returns ok=false to signal a drop; the
//     caller increments dialer_router_events_dropped_total{type} and
//     skips the handler invocation.
//
// # Idempotency contract
//
// Each call to Dial / Hangup mints a fresh CommandID. The bridge's
// SETNX-on-CommandID idempotency layer therefore protects against
// retries the BRIDGE replays internally — but the DIALER's own retry
// path is NOT idempotent at this layer. A dialer caller that retries
// Dial after a transient Originate failure publishes a NEW command,
// and the bridge will treat it as a NEW originate. The dialer's CallID
// is the cross-boundary identity token; the bridge's idempotency key
// is the CommandID and is per-publish.
//
// # Subscribe semantics
//
// Subscribe wraps telephony.api.EventConsumer.Subscribe. The dialer's
// handler signature differs from telephony's (different ChannelEvent
// type), so the wrapper installs a translator-shaped handler with the
// underlying consumer; on each event it translates and either invokes
// the dialer's handler with the projected event or drops + meters.
//
// Handler errors propagate back to the underlying telephony Consumer
// verbatim so the bridge can NACK and re-deliver — the dialer Router
// does NOT swallow errors from the user handler. A panic from the user
// handler is also propagated; the underlying consumer's panic-recovery
// (if any) is responsible for keeping the consumer alive.
//
// # Metrics
//
// Per-package Prometheus collectors live in metrics.go. RegisterMetrics
// builds and registers the collectors on a caller-supplied registerer;
// tests pass prometheus.NewRegistry(). Per the project-wide rule (Plan
// 09 lessons), this package deliberately does NOT register at init()
// time — two test imports of an init-registering package collide on
// prometheus.DefaultRegisterer.
package router
