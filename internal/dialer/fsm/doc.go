// Package fsm implements api.OperatorFSM — the per-operator state machine
// that drives the operator UI workflow (StartShift, GoReady, GoPause, ...,
// EndShift).
//
// # Architecture
//
// The state machine is split across two persistence layers, intentionally:
//
//  1. Live state — Redis hash op:<tenant>:user:<operator>. Source of truth
//     for "is operator X in state Y right now?". Read 10×/s by the dispatch
//     loop. Mutated by an HSET-with-CAS-version Lua script (see
//     lua/transition.lua) so concurrent transitions on the same operator
//     never tear the snapshot.
//
//  2. Audit trail — Postgres operator_state_log + outbox event_outbox.
//     Each successful transition writes one operator_state_log row and one
//     outbox row in the same Tx (BypassRLS — both are platform-internal
//     tables, not RLS-bound). The relay drains outbox to NATS for
//     downstream consumers (analytics, supervisor dashboard).
//
// Lifecycle is bound to operator_sessions: StartShift INSERTs a session
// row and binds session_id into the Redis hash; subsequent transitions
// audit against that session_id. EndShift UPDATEs ended_at on the session.
//
// # Concurrency model
//
// Optimistic concurrency. Every Redis hash carries a "version" integer
// that the Lua CAS script increments on every successful write. A
// transition reads the hash, checks the transition table, then issues a
// CAS write — if the version no longer matches, the script returns -1 and
// the caller surfaces the conflict so the calling layer can retry or
// reconcile.
//
// # Idempotency
//
// Every method is idempotent on identical replay. Specifically:
//
//   - Repeating an event in a state where it's already applied is a no-op
//     (returns the current Snapshot, nil error). No Redis write, no audit.
//
//   - RecordCallStarted with the same call_id replays cleanly; with a
//     different call_id while one is in flight returns ErrInvalidTransition
//     wrapped with both call IDs.
//
//   - StartShift on an already-started operator returns the current
//     ready Snapshot rather than erroring. EndShift on an already-offline
//     operator is a no-op.
//
// # Force escape hatch
//
// Force(target, reason) bypasses the transition table. Used by the
// heartbeat watchdog (Task 2c) and the supervisor "kick offline" admin op.
// It validates target.Valid() and writes the target state directly via
// the same CAS Lua script. Audit is recorded with reason on the
// operator_state_log row.
//
// # Tenant scoping
//
// Every method requires explicit tenant_id. The implementation validates
// that the loaded hash's stored tenant_id matches the requested tenant_id
// and returns ErrTenantMismatch on mismatch as a defence-in-depth layer
// above RLS.
//
// # Known limitations (Task 2 v1)
//
//   - StartShift INSERTs an operator_sessions row before the Redis CAS. On
//     concurrent StartShift the loser's session row is orphaned with
//     ended_at = NULL. This inflates "active operators" queries by the rate
//     of concurrent StartShift rejections (low — 1 per operator per shift).
//     A periodic reaper (Plan 10 Task 2c follow-up) is intended to UPDATE
//     ended_at on rows older than 24h with no matching Redis hash.
package fsm
