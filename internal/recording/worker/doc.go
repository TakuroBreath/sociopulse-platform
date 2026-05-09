// Package worker hosts the recording-module background daemons:
// the retention pass (this package, Plan 12.4 Task 2) and — when
// shipped — the integrity verifier (Plan 12.4 Task 3).
//
// # RetentionPass lifecycle
//
// RetentionPass is leader-elected via a Postgres advisory lock so a
// single replica owns each tick across the cluster. The Run loop
// mirrors internal/dialer/retry.Orchestrator: time.NewTicker, an
// immediate first sweep on boot, ctx-cancel teardown that releases
// the advisory lock with a detached background ctx so peers take
// over without a TCP keepalive wait.
//
// Each tick (when leading) runs SweepOnce, which is two passes in
// sequence: cold-moves THEN deletes. The passes share a tick because
// they read disjoint row sets and the same advisory lock guards both
// — running them sequentially keeps the Tx footprint bounded.
//
//	┌───────────────────────────────────────────────────────────────┐
//	│ Run(ctx)                                                      │
//	│   ┌─── ticker fires ────┐                                     │
//	│   │                     │                                     │
//	│   ▼                     │                                     │
//	│  tick(ctx)              │                                     │
//	│   ├─ Acquire(ctx) ──────┘  (peer holds lock → skip this tick) │
//	│   └─ leading?                                                 │
//	│      └─ SweepOnce(ctx)                                        │
//	│         ├─ sweepColdMoves(ctx)                                │
//	│         │   ListDueColdMoves (BypassRLS)                      │
//	│         │   for each row:                                     │
//	│         │     handleColdMove                                  │
//	│         │       WithTenant(tenantID, fn(tx) {                 │
//	│         │         MarkColdTx → 0? → errStaleSkip              │
//	│         │         writeAudit(action=cold_moved)               │
//	│         │       })                                            │
//	│         │     metrics.IncRetentionAction(ok|stale|error)      │
//	│         │                                                     │
//	│         └─ sweepDeletes(ctx)                                  │
//	│             ListDueDeletes (BypassRLS)                        │
//	│             for each row:                                     │
//	│               handleDelete                                    │
//	│                 Phase A: ObjectStore.Delete                   │
//	│                   ├─ ErrObjectNotFound → orphaned=true        │
//	│                   ├─ generic error → metric error + return    │
//	│                   └─ ok                                       │
//	│                 Phase B: WithTenant(tenantID, fn(tx) {        │
//	│                   MarkDeletedTx → 0? → errStaleSkip           │
//	│                   writeAudit(action=deleted)                  │
//	│                   outbox.Append(call.deleted event)           │
//	│                 })                                            │
//	│               metrics.IncRetentionAction(ok|stale|error|      │
//	│                                          orphaned)            │
//	└───────────────────────────────────────────────────────────────┘
//
// # Per-row failure isolation
//
// handleColdMove and handleDelete never propagate per-row errors back
// to SweepOnce — they log + bump the metric + continue. SweepOnce
// returns an error only when one of the LIST queries itself fails;
// that signals a Postgres-level outage worth surfacing to the
// orchestration layer.
//
// # Two-phase delete invariant
//
// Phase A (ObjectStore.Delete) is irreversible. Phase B (status flip
// + audit + outbox) is atomic via WithTenant Tx. The two phases are
// separate transactions because S3 is not transactional with PG;
// they cannot share a Tx. The invariant we maintain instead:
//
//   - Phase A succeeded → Phase B MUST run (DB and S3 stay consistent).
//   - Phase A failed (generic error) → Phase B MUST NOT run (the
//     audio object is still present; flipping status would orphan it).
//   - Phase A returned ErrObjectNotFound → DB and S3 are out of sync;
//     Phase B reconciles them. Bump "orphaned" metric so dashboards
//     can flag the divergence rate.
//
// # Lock keys
//
// RetentionLockKey and IntegrityLockKey are exported package vars so
// cmd/worker can construct retry.PgLeader instances without a
// transitive dep on this package's privates. They're FNV-1a hashes
// of "recording.retention_pass" and "recording.integrity_pass"
// respectively — distinct slots so the two passes do not block each
// other.
package worker
