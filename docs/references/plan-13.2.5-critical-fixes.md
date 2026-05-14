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

_Filled at close-out._
