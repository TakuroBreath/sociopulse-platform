// Package realtime is the registration entry point for the realtime
// module — WebSocket Hub + NATS dispatcher + Redis presence + listen-in.
//
// Plan 11 wires this in 10 sub-tasks:
//  1. Module skeleton + interfaces in internal/realtime/api (THIS task —
//     api/ contracts pre-existed from Plan 00a; this task added the
//     AllTopics / TopicAction registry helpers + the Module.New
//     constructor).
//  2. WebSocket connection lifecycle (auth handshake, writer/reader
//     goroutines, slow-consumer drop-oldest).
//  3. Hub fan-out + per-topic RBAC.
//  4. NATS subscriber + dispatcher (replaces pkg/eventbus.NATSPublisher /
//     NATSSubscriber stubs; cmd/api swaps noopPublisher for real NATS).
//  5. Redis-backed PresenceTracker.
//  6. Listen-in v1 (silent mode) + audit (DEFERRED until Plan 08
//     FreeSWITCH cluster lands; stub returns ErrTelephonyBridgeOffline).
//  7. HTTP handlers (/ws, listen-in endpoints, force-action endpoints).
//  8. Helm + ingress timeouts (DEFERRED to sociopulse-infra repo).
//  9. Integration tests + coverage.
//
// 10. Frame classification + listen-in cleanup on disconnect + janitor.
//
// Plus carry-overs from prior plans (handled here):
//   - internal/telephony/nats_bridge — Plan 09 stub becomes real here.
//   - cmd/api outbox publisher — noop swapped for real NATS.
//   - dialer transport/http — RefreshPresence wired into middleware.
//   - dialer SnapshotPubSub — in-mem swapped for NATS-backed fan-out.
//
// See docs/references/plan-11-realtime.md for the canonical reading
// list, gotchas, and architecture decisions.
package realtime

import "github.com/sociopulse/platform/internal/modules"

// Module is the top-level registration handle for the realtime module.
// Stateless at the type level; lifecycle state (Hub / NATS subscriber /
// Presence sweeper goroutines) lives on the value once Register has
// run. Stop tears them down (added when Tasks 2-7 land).
type Module struct{}

// New returns a fresh Module ready for Register. The constructor is
// intentionally trivial — every dep is supplied through modules.Deps,
// not through New, so cmd/api can construct the Module statically and
// hand it to the registry without knowing which deps it needs.
func New() *Module { return &Module{} }

// Name returns the module's unique identifier within the registry.
// Used by modules.Registry for ordering, locator key prefixes, and the
// /admin/modules HTTP endpoint.
func (*Module) Name() string { return "realtime" }

// Register wires the module's components into the composition root.
// Plan 11 Tasks 2-7 fill this in; for now it is a documented no-op so
// cmd/api boots cleanly with the module registered. Register MUST be
// idempotent (safe to call twice — defence against future refactors
// that re-run module init).
func (*Module) Register(d modules.Deps) error {
	_ = d // unused until Plan 11 Task 2 wires Hub.
	return nil
}
