// Package http is the gin HTTP + WebSocket transport for the realtime
// module. It exposes:
//
//   - GET    /api/realtime/ws                          — WebSocket upgrade.
//   - POST   /api/realtime/operators/:id/force-pause   — server→operator force-pause.
//   - POST   /api/realtime/operators/:id/force-end-shift — server→operator force-end-shift.
//   - POST   /api/realtime/calls/:id/listen            — STUB (Plan 08 dependency).
//   - DELETE /api/realtime/listen-sessions/:id         — STUB (Plan 08 dependency).
//
// # WebSocket flow
//
// Client connects to /api/realtime/ws (subprotocol "sociopulse-v1") and
// sends a FrameAuth frame as the FIRST inbound message. The server
// validates the JWT through the auth module's Authenticator (adapted
// to the realtime AuthValidator surface) and registers the connection
// with the per-replica Hub. Subsequent FrameSubscribe / FrameUnsubscribe
// frames flow through the standard rtapi.Connection contract.
//
// Per-connection presence is tracked via the Redis-backed
// PresenceTracker (Plan 11 Task 5). The handler:
//
//   - calls OnConnect at upgrade time,
//   - calls Touch every TTL/3 from a per-conn ticker goroutine,
//   - calls OnDisconnect on Run() exit (per-pod refcount keeps the
//     Redis key alive while any local connection for the user is open).
//
// # Force-action flow
//
// admin or supervisor POST to /api/realtime/operators/:id/force-pause
// or /force-end-shift. The handler builds a JSON payload, calls
// hub.Broadcast on TopicForceCommands with a tenant+user filter, and
// returns 202 with the local recipient count. Cross-replica delivery
// is the dispatcher's job (Plan 11 Task 4) — replicas other than the
// one the operator is connected to fan-out via NATS, so a 0-recipient
// response here does NOT mean the broadcast failed.
//
// # Listen-in (deferred)
//
// Per Plan 11 Decision 5 the Listen-in service is deferred until Plan
// 08 (FreeSWITCH cluster) lands. The handlers in listen_handler.go
// return 503 Service Unavailable + the canonical
// telephony.bridge.offline error envelope on every endpoint.
//
// # Authentication
//
// The realtime layer's AuthValidator returns rtapi.Claims with
// string-typed roles and IDs; the auth module's Authenticator returns
// auth.Claims with uuid.UUID + Role enums. authValidatorAdapter bridges
// the two — see auth_validator.go.
//
// # Carry-overs (Plan 09/10)
//
//   - *zap.Logger typed; nil-safe defaults via zap.NewNop().
//   - var _ rtapi.WSConn = (*coderWSAdapter)(nil) compile-time check.
//   - wg.Go (Go 1.25+); time.NewTicker (not time.After in loops).
//   - goleak.VerifyTestMain in this package's test binary so the Touch
//     ticker goroutine cannot leak.
//   - Errors wrapped with the package path:
//     fmt.Errorf("realtime/transport/http: <op>: %w", err).
package http
