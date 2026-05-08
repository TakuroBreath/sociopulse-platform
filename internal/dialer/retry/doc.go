// Package retry implements api.RetryOrchestrator — the dialer's
// scheduled scanner that re-enqueues mature respondent retries.
//
// # Why a leader-elected sweep
//
// The dialer worker binary may be deployed across N replicas for HA.
// If every replica ran the sweep concurrently, the FOR UPDATE SKIP
// LOCKED clause keeps Postgres correctness intact, but every replica
// pays the round-trip cost N times. Worse, the per-row enqueue would
// race against itself — only the first replica's queue.EnqueueRespondent
// succeeds (the dedup SET rejects duplicates), but the wasted Decrypt
// + queue calls + DB writes are pure waste.
//
// The fix is exactly-once leadership via Postgres advisory locks.
// pg_try_advisory_lock(int64) is non-blocking: at most one replica
// acquires the lock at a time; peers see ok=false and skip the sweep
// without queueing. The lock is bound to the holding session — when
// the session disconnects (process crash, network blip, deliberate
// Release), PG drops the lock automatically and the next peer to call
// Acquire wins. No heartbeats, no expiration windows to tune; the TCP
// keepalive IS the leadership renewal.
//
// # Run loop
//
// On each interval tick:
//
//  1. PgLeader.Acquire (non-blocking pg_try_advisory_lock).
//  2. If leading: ListMatureRetries(BatchLimit) inside a
//     BypassRLS transaction with FOR UPDATE SKIP LOCKED so concurrent
//     leaders during a failover window don't double-process.
//  3. For each row:
//     - if attempts >= max_attempts → MarkExhausted; metric "exhausted".
//     - else: Decrypt(phone) → Queue.EnqueueRespondent → MarkScheduled.
//  4. Histogram observe sweep duration.
//
// Per-row failures (decrypt error, queue error, DB error) are logged
// and bucketed under the "skip" metric label so a single bad row
// doesn't abort the rest of the batch.
//
// # Status rules
//
// status_rules.go owns the Plan 10 §8.5 disposition table and the
// Apply() helper that translates (status, attempts) into a Decision
// {Retry, Delay, MarkExhausted, MarkDNC, CountsAttempt}. The FSM /
// SubmitStatus path consumes Apply() to set the next_attempt_at on the
// respondents row at the moment a call concludes; this orchestrator
// just consumes the materialised row and re-enqueues at the right time.
//
// # Surface dependencies
//
// The package depends on small interfaces (Leader, RespondentReader,
// Decryptor) declared in this package, NOT on the concrete *PgLeader /
// *PgReader / tenancy.KMSResolver. Production wiring passes those
// concrete types — they satisfy the interfaces via duck typing — but
// unit tests use lightweight in-memory fakes and don't drag the whole
// telephony / tenancy surface in.
//
// # Plan 09 carry-forward
//
//   - *zap.Logger with typed fields, never PII.
//   - var _ api.RetryOrchestrator = (*Orchestrator)(nil) compile-time check.
//   - No init()-time MustRegister; metrics are wired explicitly via
//     RegisterMetrics(reg) so two test imports don't collide on the
//     default registerer.
//   - time.NewTicker (NEVER time.After) in Run; ticker stopped on the
//     defer path so a cancelled Run leaves no goroutine leak.
//   - Sentinel errors aliased to api.ErrXxx where applicable; this
//     package adds none of its own.
package retry
