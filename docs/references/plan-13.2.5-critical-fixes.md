# Plan 13.2.5 — Critical Fixes — references

> Curated reading list for Plan 13.2.5 (critical fixes). Read this BEFORE writing any code. "Production lessons" filled at close-out.

---

## Why this plan exists — backing audit reports

On 2026-05-14 a 6-agent adversarial code audit ran in parallel against high-risk slices of the platform. Findings (one-liner each):

| Slice | Result | Headline |
|---|---|---|
| Reality audit (docs vs git vs code) | ✅ Clean | 22/22 tags 1:1 with PROJECT_STATUS.md; Plan 13.2 deliverables verified |
| Tenancy / RLS | 🚨 3 CRITICAL | Admin endpoints take `:id` from URL → BypassRLS resolves tenant → operates under target row's tenant. Caller's tenant is NOT verified. |
| Encryption | 🟠 0 CRITICAL, 3 HIGH | Empty AAD in resolver Encrypt/Decrypt (phone/TOTP swap-attack at AEAD layer); KEK-version cache bypassed on Encrypt; Yandex KMS adapter is a stub |
| Dialer FSM | 🚨 2 CRITICAL | `EventCallFailed`, `RecordCallStarted`, `RecordCallEnded` defined & tested, no production callers. `telephony.Router.Subscribe` declared, never wired. |
| Outbox + analytics ingest | 🚨 2 CRITICAL | Silent OLAP row loss on CH error (no DLQ); guaranteed dupes on restart (`MergeTree` not `ReplacingMergeTree`) |
| Security + plans sample | 🟠 0 CRITICAL, 3 HIGH | 11 stdlib CVEs (Go 1.26.1 → 1.26.3); `x/net v0.52.0` → 0.53.0; refresh-tokens plaintext JTI in Redis. Plan 13.2 amendments (12) show cross-plan contract enforcement gap. |

Full reports persisted in the session transcript that produced this plan. The five CRITICAL findings drive Tasks 1, 2, 3 (spec-drift bonus), 4, and the highest-leverage HIGH (empty AAD in resolver — Task 6).

## Canonical specs

### Master system-design spec

- `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`
  - **§FR-A** — Auth + admin endpoints (Task 1 affects). The cross-tenant-guard requirement is implicit; spec assumes RLS protects admin-plane but it doesn't because admin handlers BypassRLS for the resolve step.
  - **§FR-D** — Dialer FSM behavior. Operator state machine canonical transitions. CONTEXT.md is the authoritative summary.
  - **§FR-I** — Insights/Analytics. Idempotency requirement spelled out; current implementation depends on in-memory LRU only.
  - **§6.4** — ClickHouse tables. Schema definitions; Task 4 evolves engine choice.
  - **§17** — test strategy.

### ADRs

- `docs/adr/0006-pgbouncer-transaction-mode.md` — Tasks 1 must preserve `SET LOCAL` discipline. Cross-tenant guard runs BEFORE service so it must not require an open Tx; resolver uses BypassRLS for the resolve step only.
- `docs/adr/0010-postgres-plus-clickhouse.md` — Task 4 stays within OLAP boundary; no schema change to Postgres.
- `docs/adr/0011-nats-over-kafka.md` — Task 2 consumes existing telephony NATS subjects; no new bus.
- `docs/adr/0015-tdd-mandatory.md` — Red-Green-Refactor for every task.

### Architecture docs

- `docs/architecture/02-module-contracts.md` § auth, crm, surveys, dialer, analytics — module APIs.
- `docs/architecture/03-error-handling.md` — sentinel-to-HTTP mapping; Task 1 returns `404 Not Found` (no body) on cross-tenant attempt (avoids existence-probe).
- `docs/architecture/06-observability.md` — Prometheus metric naming; Tasks 4 and 5 follow `sociopulse_<module>_<measurement>_<unit>` convention.
- `docs/architecture/07-go-coding-standards.md` — applies to all new code.
- `docs/architecture/08-tdd-discipline.md` — RED-GREEN-REFACTOR per task.

## Critical context (per task)

### Task 1 — Cross-tenant guard

**The bug pattern**:

```go
// BEFORE (BROKEN):
func (s *UserService) UpdateUser(ctx context.Context, userID uuid.UUID, ...) error {
    tenantID, err := s.store.ResolveTenantBypassRLS(ctx, userID)  // ← Tenant of target row
    if err != nil { return err }
    return s.store.WithTenant(ctx, tenantID, func(...) error {
        return s.store.Update(ctx, userID, ...)  // Operates as target's tenant
    })
}

// AFTER (CORRECT):
func (s *UserService) UpdateUser(ctx context.Context, callerTenantID, userID uuid.UUID, ...) error {
    // Middleware has already verified callerTenantID == ResolveTenant(userID); reject otherwise
    return s.store.WithTenant(ctx, callerTenantID, func(...) error {
        return s.store.Update(ctx, userID, ...)  // Operates as caller's tenant; RLS enforces row visibility
    })
}
```

**Why the middleware AND the explicit param**: defence in depth. The middleware can be forgotten on a future endpoint. The explicit param forces the service to fail-loud if the caller forgets.

**Existence-probe avoidance**: returning `404` on cross-tenant attempts means `Tenant A → Tenant B's user` looks indistinguishable from `Tenant A → nonexistent user`. Returning `403` would let an attacker enumerate. CONFIRMED industry practice.

### Task 2 — Telephony→Dialer wiring

**Subject layout** (per `internal/telephony/api/events.go` schema):
- `tenant.<t>.telephony.channel.created` — outbound dial started (FSM doesn't care; analytics consumes)
- `tenant.<t>.telephony.channel.answered` — respondent picked up → FSM `RecordCallStarted`
- `tenant.<t>.telephony.channel.hangup{cause}` — call ended; cause classifies outcome → FSM `RecordCallEnded(outcome)`
- `tenant.<t>.telephony.channel.originate_failed` — SIP busy/SIT/congestion → FSM `EventCallFailed`

**The FSM-already-in-target-state case**: a duplicate `answered` event (NATS at-least-once delivery) arrives while FSM is already `call`. Subscriber must ACK and skip — calling `RecordCallStarted` again must be idempotent (same callID = no-op) OR return a sentinel the subscriber recognizes.

### Task 3 — FSM spec drift

CONTEXT.md authoritative:
> **FSM (operator)** — Finite State Machine of the operator: `offline → ready → dialing → call → status → verify → ready`, plus `pause` from any state. `verify` is reachable only from `success`-class outcomes.

Implementation gaps (from audit):
- `pause` only from `ready` — violates spec.
- `verify` from `ready` — violates spec; should be from `status` (with success outcome class).
- `RecordCallEnded` from `ready` per docstring lies — code routes to `status`.

### Task 4 — ReplacingMergeTree

CH `ReplacingMergeTree(_inserted_at)` keeps the row with the **largest** `_inserted_at` per ORDER BY tuple. With ORDER BY `(tenant_id, event_id)`, same `event_id` in same tenant = idempotent. **Caveat**: dedup is async — happens at merge time. `SELECT FINAL` reads the deduped view (slower); regular `SELECT` may see both rows. Plan accepts FINAL-cost on the few analytics queries that care.

**Migration trap**: ClickHouse `ALTER TABLE ... MODIFY ENGINE` is **NOT supported**. Must rename to `_legacy`, create new table, `INSERT INTO new SELECT FROM legacy`, drop legacy. Plan structures migrations accordingly.

### Task 5 — Outbox DLQ

`relay.go` already has the `attempts` column; rows park at `attempts >= MaxRetry`. Today they're invisible. Adding a poll-and-gauge gives ops a single Prometheus query (`sum(sociopulse_outbox_parked_rows_total) by (tenant)`) and an alert rule (`> 0 for 5m`).

**Gauge not counter**: parked rows can decrease (manual retry resets attempts). Gauge reflects current backlog, counter would lie.

### Task 6 — BuildAAD helper

**The bug pattern**:

```go
// kms_resolver.go:188 (BROKEN — AAD is nil)
func (k *KMSResolverImpl) Encrypt(ctx context.Context, tenantID uuid.UUID, plaintext []byte) ([]byte, error) {
    return k.aead.Seal(nonce, nil, plaintext, additionalData=nil)  // ← nil AAD allows swap
}

// FIX:
func (k *KMSResolverImpl) Encrypt(ctx context.Context, tenantID uuid.UUID, scope, rowID string, plaintext []byte) ([]byte, error) {
    aad := encryption.BuildAAD(tenantID, scope, rowID)
    return k.aead.Seal(nonce, nil, plaintext, additionalData=aad)
}
```

**Versioning**: existing ciphertexts have no AAD bound. Must remain decryptable. Solution: prepend a single byte `0x01` (legacy) or `0x02` (BuildAAD) to the wrapped DEK. Decrypt path reads the byte → chooses AAD. Encrypt path always writes `0x02`. Eventual re-encryption migration is a future task (NOT this plan).

**Canonical AAD format** (from spec rationale — length-prefix prevents ambiguity):

```go
// BuildAAD encodes as: <uvarint(len(tenantStr))><tenantStr><uvarint(len(scope))><scope><uvarint(len(rowID))><rowID>
func BuildAAD(tenantID uuid.UUID, scope, rowID string) []byte {
    tenant := tenantID.String()
    out := make([]byte, 0, len(tenant)+len(scope)+len(rowID)+3*binary.MaxVarintLen64)
    out = binary.AppendUvarint(out, uint64(len(tenant)))
    out = append(out, tenant...)
    out = binary.AppendUvarint(out, uint64(len(scope)))
    out = append(out, scope...)
    out = binary.AppendUvarint(out, uint64(len(rowID)))
    out = append(out, rowID...)
    return out
}
```

## Cross-cutting

- `docs/references/COMMON.md` — cross-plan references; see § Outbox, § JetStream, § Crypto.
- `pkg/middleware/auth.go` — claims extraction; Task 1's middleware reads `claims.TenantID` from gin context that this middleware sets.
- `pkg/eventbus/nats.go` — `NATSSubscriber` configuration; Task 2 uses existing helper.
- `pkg/observability/metrics.go` — Prometheus registry; Tasks 4 & 5 register here.

## Production lessons

_Filled at close-out 2026-05-14, after the 6-task wave landed cleanly across 7 commits + 2 fix-up commits._

### Audit → plan → fix loop

1. **Adversarial-audit-then-fix worked.** Six parallel review agents found 5 CRITICAL findings that real users would have hit. Plans wrote, agents implemented, reviewers caught one more (the recording-flow AAD HIGH that the implementer wrongly deferred). The total cost (~6 audit agents + 6 implementers + 6 reviewers + 2 fix-up agents) was a fraction of what a missed cross-tenant breach in prod would cost.
2. **Two-stage review caught real bugs.** Task 5 review caught a Prometheus naming convention bug (`_total` suffix on a gauge) before any dashboard/alert locked the name in. Task 6 review caught that the recording-flow refactor was incorrectly deferred — implementer's justification ("non-empty AAD") missed that AAD must bind row identifier, not just tenant. Both fixes landed as fix-up commits before the plan close-out.
3. **Implementer scope expansion is OK when the spec demands it.** Tasks 1 and 6 both expanded scope (Task 1 into realtime + analytics for `CrmReader` signature propagation; Task 6 into dialer retry adapter + cmd/worker decryptor for `respondentID` AAD reproducibility). The "don't touch X" hint in implementer prompts is fine for net-new code but cascades on signature changes are unavoidable. Future plans should mark "expected ripple targets: ..." sections explicitly.

### Cross-tenant guard design lessons

4. **404, not 403, on mismatch.** The middleware returns 404 with no body on cross-tenant attempts. Returning 403 ("forbidden") would let an attacker enumerate which IDs exist in another tenant. The plan got this right and the reviewer confirmed.
5. **Explicit `callerTenantID` parameter is defence-in-depth.** Even with the middleware in place, services that receive `callerTenantID` and run under `WithTenant(callerTenantID, ...)` will hit RLS rejection if a future endpoint forgets the middleware. The surveys module chose to thread `tenantIDFromContext` instead — equivalent semantically, but a future refactor that removes the ctx key would silently regress.
6. **Out-of-scope finding**: `POST /api/calls/:id/hangup` in `internal/dialer/transport/http` was NOT in the original audit list (dialer was not in the C1 module scope) but the reviewer flagged it: an authenticated operator from Tenant A who learns a Tenant B `call_id` could hangup that call. **Tracked for Plan 14.**

### AEAD / envelope encryption lessons

7. **AAD must bind the row identifier, not just the tenant.** This was the single most important lesson from Task 6. The implementer's deferral justification ("recording flow already passes non-empty AAD") was technically true but missed the security model: a same-tenant cross-row swap is still possible if AAD only carries tenant_id. `BuildAAD(tenantID, scope, rowID)` is the canonical pattern.
8. **Length-prefixed AAD encoding is required.** Naive concatenation `tenant.String() + scope + rowID` is vulnerable to ambiguity-attack splits (e.g., `("t", "auth.user.phone", "id")` vs `("t", "auth.user", "phone.id")`). The `binary.AppendUvarint`-per-field encoding eliminates the class.
9. **Versioned-DEK byte dispatch for backward compat.** Three byte 0 dispatch paths (`0x00` unprefixed legacy, `0x01` versioned legacy, `0x02` new with BuildAAD) was the only safe way to introduce AAD changes to a system with existing encrypted data. **Caveat**: for the recording flow, no DEKs existed in production (cmd/recording-uploader still a stub) so the fix-up did a clean refactor without legacy paths.
10. **Service-layer swap-rejection tests are essential.** Resolver-layer tests catch the primitive correctness; service-layer tests verify that the application code actually USES the new resolver signature with the right scope+rowID. We almost missed a real production-relevant test that swaps respondent A's phone ciphertext bytes into respondent B's row.

### ClickHouse engine lessons

11. **`MergeTree` to `ReplacingMergeTree` migration is RENAME → CREATE → INSERT-SELECT → DROP.** CH does not support `ALTER TABLE … MODIFY ENGINE`. The migration must drop dependent MVs first, rename source tables, create new tables, backfill, drop legacy, recreate MVs. State tables (`mv_*_state`) are preserved — only the materialized-view (the `WITH … TO …` definitions) are dropped/recreated.
12. **CH migrations are not transactional.** `golang-migrate` runs each statement separately; a crash between `RENAME → CREATE → INSERT → DROP` leaves the DB in a half-state. Runbook entry needed: "if crashed mid-migration: drop the partially-created table, restore the `_legacy` rename, retry." Accepted trade-off for v1.
13. **ORDER BY trade-off for dedup.** Changing `ORDER BY (tenant_id, project_id, ts)` → `(tenant_id, event_id)` favors dedup correctness over query speed on raw source-table scans. Verified no production query path reads source tables directly with `project_id`-leading predicates outside the ingest path.
14. **Dedup-miss metric is best-effort.** The new `sociopulse_analytics_ingest_dedup_miss_total{subject}` counter increments on `WasEmpty || Evicted` LRU paths. Help text honest about approximation. ReplacingMergeTree reconciles asynchronously at merge; `SELECT FINAL` reads the deduped view for analytics queries.

### Pre-Prometheus-naming and post-deployment lessons

15. **Gauges should NOT have `_total` suffix.** Prometheus convention reserves `_total` for counters. Caught at Task 5 review. Fix landed before any dashboard/alert locked in the name.
16. **JetStream durable consumers persist server-side.** `callEventBoot.Close` is intentionally a no-op for push subscriptions managed by the bus — the durable consumer is NOT auto-deleted on disconnect (that's the resume property). Cluster-level cleanup is out of scope for v1.

### Dialer FSM design lesson

17. **Outcome enum on the FSM snapshot, persisted to Redis.** Task 3 considered two design options for "verify only from success-class outcomes": (a) carry outcome as a separate per-FSM-instance variable, or (b) field on the `OperatorState` snapshot. Option (b) won: outcome survives operator pause/restart between `RecordCallEnded` and `GoVerify`. Hash field `outcome` is HDEL'd via empty-string semantics whenever a transition exits `status` (centralized in `buildNextSnapshot` helper).
18. **`resolveTransition(from, event, outcome)` as the single decision point.** Every state mutation goes through one function. A future agent adding a transition cannot bypass the outcome guard — defence against future drift.

### Producer-consumer plan contracts

19. **Wire BOTH sides in one plan, OR test the contract.** The Plan 09 telephony publisher existed long before Plan 13.2.5 wired the consumer; the spec said "subjects look like `tenant.<t>.telephony.event.<call_id>.<type>`" and the consumer correctly bound to that pattern. **But there was no integration test asserting publisher ↔ consumer payload+subject contract.** Plan 13.2 amendments showed the same pattern (12 deviations from cross-plan-contract drift). **Future recommendation**: a `docs/contracts/` folder with cross-plan invariants validated by integration tests.

### Process lessons

20. **Wave-based dispatch with file partitioning works.** Three waves (Wave A: 3 parallel independent tasks; Wave B: 2 parallel after Wave A commits; Wave C: 1 task after Tasks 1+5 land). Zero merge conflicts despite 6 implementers + 6 reviewers running in parallel.
21. **Don't trust gopls during in-flight subagent dispatches.** Diagnostics about "wrong type for method" surfaced multiple times during signature-change cascades. By each implementer's commit time, the actual compile was clean. Always re-verify with `go build && go vet && go test -race`.
22. **Race-test the full repo before tagging.** Final `go test -race -count=1 ./...` covers ~80 packages and took ~30s. Single CRITICAL bug it could have caught: any cross-goroutine state in the new FSM outcome handling.
