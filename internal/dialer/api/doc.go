// Package api defines the public contract of the dialer module.
//
// Other modules (cmd/api HTTP handlers, cmd/worker retry runner, internal/realtime
// WebSocket hub) import only from this package — never from internal/dialer/fsm,
// internal/dialer/queue, internal/dialer/rdd, or internal/dialer/router. The
// implementations are package-private to internal/dialer and are reachable only
// through these stable interfaces and DTOs.
//
// # OperatorFSM transition diagram
//
//	    ┌────────────────────────────────────────────────────────────────────┐
//	    │                                                                    │
//	    │  EndShift                                                          │
//	    ▼                                                                    │
//	[offline] ──StartShift──▶ [ready] ──GoPause──▶ [pause] ──Resume──▶ [ready]
//	                              │                                         │
//	                              │ CallStarted (dial begins)               │
//	                              ▼                                         │
//	                          [dialing] ──CallStarted (ANSWER)──▶ [call]    │
//	                              │                                  │      │
//	                              │ CallEnded / CallFailed           │      │
//	                              │                                  │      │
//	                              ▼                                  ▼      │
//	                          [status] ◀──── CallEnded ──────────[call]     │
//	                              │                                         │
//	                              │ StatusSubmitted                         │
//	                              └─────────────────────────────────────────┘
//
//	[ready] ──GoVerify──▶ [verify] ──VerifyDone──▶ [ready]
//
// Force(target, reason) is the escape hatch invoked by the heartbeat watchdog
// when presence:<tid>:user:<id> TTL expires. Force bypasses the transition
// table and writes the target state directly with reason="heartbeat_lost".
// It is intentionally not part of the diagram above.
//
// # Persistence model
//
//   - Redis hash op:<tenant>:user:<operator> is the source of truth for
//     "is operator X in state Y right now?". Read 10×/s by the dispatch
//     loop. Mutated by an HSET-with-CAS-version Lua script so concurrent
//     transitions on the same operator never tear the snapshot.
//
//   - Postgres operator_state_log (session_id, ts, state, reason) is the
//     audit trail. Each successful FSM transition writes one row and one
//     outbox event in the same transaction. The relay drains outbox to
//     NATS subject tenant.<t>.dialer.op.<op_id>.state for downstream
//     consumers (analytics, supervisor dashboard).
//
//   - Per-operator presence:<tid>:user:<id> Redis key with 30s TTL holds
//     the operator's heartbeat. The watchdog observes expiry and calls
//     Force(target=offline, reason="heartbeat_lost").
//
// # Idempotency contract
//
// Every FSM method is idempotent on identical replay. Specifically:
//
//   - Repeating an event in a state where it's already applied is a no-op
//     (returns the current Snapshot, nil error).
//
//   - RecordCallStarted with the same call_id replays cleanly; with a
//     different call_id while one is in flight returns
//     ErrInvalidTransition wrapped with both call IDs.
//
//   - StartShift on an already-started operator returns the current
//     ready Snapshot rather than erroring.
//
// # Tenant scoping
//
// Every method requires explicit tenant_id. Implementations validate that the
// loaded hash's stored tenant_id matches the requested tenant_id and return
// ErrTenantMismatch on mismatch as a defence-in-depth layer above RLS.
package api
