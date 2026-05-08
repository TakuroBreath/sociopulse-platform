# Plan 10 — dialer references

> **Goal**: snap the dialer module (OperatorFSM + CallQueue + RDD + Router + Capacity + Hours + Retry + HTTP/WS + Module composition) to authoritative external sources, so subagents stop re-deriving canonical patterns. Loaded into every implementer prompt at dispatch time.

**Status**: in-progress, next after Plan 09 (`v0.0.10-telephony-bridge`).

**Carry-over note**: `internal/dialer/api/` (DTOs + interfaces + sentinel errors + NATS subjects) was completed by Plan 00a — Task 1 in plan 10 is mostly verifying + adding `doc.go`/FSM-diagram. The bulk of work is Tasks 2–10.

---

## Canonical specs (must-read)

- [Redis Lua scripting reference (Redis 7.4)](https://redis.io/docs/latest/develop/interact/programmability/eval-intro/) — atomic Lua for FSM CAS + ZSET enqueue/dequeue.
  Note: use `redis.NewScript` (NOT raw `EVAL`) — pkg auto-handles `EVALSHA` cache. Plan 09 locked this — see Plan 09 lessons. Lua's `cjson.decode` is OK for small payloads; for bigger maps use sequential ARGV.
- [Redis SCAN over ZSET / ZADD GT / ZPOPMIN docs](https://redis.io/commands/zpopmin/) — ZPOPMIN is atomic single-call; combined with ZADD-NX in Lua gives idempotent enqueue.
- [Postgres advisory locks docs (16.x)](https://www.postgresql.org/docs/current/explicit-locking.html#ADVISORY-LOCKS) — `pg_try_advisory_lock(bigint)` for leader election; lock auto-released on session disconnect.
- [Go time.Time.In + tzdata embedding](https://pkg.go.dev/time#Time.In) — `time.LoadLocation` + `_ "time/tzdata"` import to bundle 89 RU timezones into the binary (Go 1.20+).
- [E.164 / ITU-T E.164 numbering plan](https://www.itu.int/rec/T-REC-E.164/en) — RU mobile = country `+7`, 11 digits total, mobile prefixes 9XX (where XX is the carrier code).
- [Russian regional ABC/DEF prefix register (Минцифры РФ — открытый реестр)](https://digital.gov.ru/ru/activity/govservices/1/) — code prefix → region map. Locked source for 89 RU regions YAML.
- [JSON-Schema 2020-12 (informational)](https://json-schema.org/draft/2020-12) — used by neighbours; not strictly Plan 10 territory.
- [NATS JetStream durable consumer model](https://docs.nats.io/nats-concepts/jetstream/consumers) — already wired by Plan 09 telephony-bridge (`tenant.<t>.telephony.event.<call_id>.*`); dialer's `Router.Subscribe` consumes the same stream.

## Reference implementations

- [`looplab/fsm`](https://github.com/looplab/fsm) — canonical Go FSM lib. **DO NOT use the lib directly** — we need the FSM persisted in Redis, not in-process. But the API shape (transition table + guards + callbacks) is good prior art.
  Files of interest: `fsm.go` (transition matching), `event.go` (callback contract).
- [`bsm/redislock`](https://github.com/bsm/redislock) — Redlock-style distributed lock. Reference for the CAS-by-version pattern in our Lua. **Don't use directly** — we want optimistic concurrency, not pessimistic locks.
- [`hibiken/asynq` Scheduler](https://github.com/hibiken/asynq#scheduler) — already used by Plan 06 PurgeWorker. Plan 10 uses the same lib for `dialer.retry_due` periodic task. Files of interest: scheduler entry-spec + the way Plan 06 wires it in `internal/crm/module.go`.
- [`bits-and-blooms/bloom/v3`](https://github.com/bits-and-blooms/bloom) — Bloom filter for project-scope dedup of generated phone numbers (RDD). 100k-entry false-positive rate `m=1.5e6 k=7` ≈ 0.01.
- [`google/uuid`](https://pkg.go.dev/github.com/google/uuid) — already in `go.mod`. UUID-v7 (time-ordered) was discussed for queue keys but Plan 10 sticks with UUID-v4 for FSM consistency with existing schema.

## Production lessons (blog posts, talks)

- [Redis ZSET as a job queue — patterns and pitfalls](https://redis.io/blog/run-redis-as-a-message-queue/) — ZADD + ZPOPMIN is fine for low-to-medium throughput (≤10k/s). For higher throughput consider Redis Streams. We're nowhere near the threshold (50k calls/day = ~0.6/s peak). Stick with ZSET.
- [Postgres advisory-lock leader election — production reality](https://layerci.com/blog/postgres-is-the-answer/) — auto-release on session loss is the killer feature. Watchdog must hold the lock while running; if the worker pod dies, the lock evaporates and a peer takes over. Don't combine with `pg_advisory_unlock` from a different session — PG just rejects it silently.
- [Russian-cell prefix register — keeping data fresh](https://github.com/cyberlibrary/russian-phone-numbers) — community-maintained list. We embed our own snapshot in `pkg/regions/configs/regions.yaml` to avoid a runtime fetch dependency; refresh quarterly.
- [Go `time/tzdata` embedding](https://pkg.go.dev/time/tzdata) — adding this blank-import is the canonical way to ship a fully-portable binary that works on `FROM scratch` images. Without it, `time.LoadLocation("Asia/Kamchatka")` fails on alpine-without-tzdata.
- **From Plan 09 lessons (carry-forward)**:
  1. `redis.NewScript` not raw `EVAL`. Use `script.Run(ctx, rdb, keys, args...)` — pkg handles SHA caching.
  2. `time.NewTicker` not `time.After` in loops (one timer per iteration leaks).
  3. `time.NewTimer` for ad-hoc waits with `defer t.Stop()` — never `time.Sleep` inside ctx-aware code.
  4. `math/rand/v2` (`rand.IntN`, `rand.Float64`) — `math/rand` v1 is depguard-banned.
  5. `maps.Copy` not `for k,v := range src { dst[k]=v }` (golang-modernize).
  6. `strings.SplitSeq` for range-only consumers (Go 1.24+).
  7. Per-package metric injection via `RegisterMetrics(reg prometheus.Registerer)` — NEVER `init()`-time global registration.
  8. `var _ api.X = (*Impl)(nil)` compile-time interface check at the top of each impl file.
  9. Sentinel errors aliased via `var ErrFoo = api.ErrFoo` so consumers across module boundaries use `errors.Is`.
  10. `defer cancel()` for `context.WithTimeout` — panic-safe; never explicit `cancel()` at end of loop body.
  11. `sync.WaitGroup.Go` (Go 1.25+) instead of `wg.Add(1); go func(){ defer wg.Done(); ... }()`.
  12. `goleak.VerifyTestMain(m)` in every package with goroutines.
  13. Reviewers caught 5 critical bugs before main — keep two-stage review (spec compliance THEN code quality).

## Russian-specific (152-ФЗ, telephony, regions)

- [89 субъектов РФ — список с ОКТМО кодами](https://classifier.siemens.io/oktmo) — for `pkg/regions/configs/regions.yaml`. Each row: ISO 3166-2:RU code (RU-MOW, RU-SPE, ...), Russian name, IANA timezone (Europe/Moscow, Asia/Yekaterinburg, ...), ABC/DEF flag.
- [Reestr Минцифры — мобильные DEF-коды](https://opendata.digital.gov.ru/registry/numeric/) — обновляется ежеквартально. Не дёргаем в runtime — embed YAML и обновляем релизами.
- [152-ФЗ + § рабочих часов](https://digital.gov.ru/ru/activity/govservices/personaldata/) — звонки респондентам только в рабочее время по местному времени региона. Default window: будни 09:00–21:00, выходные 10:00–18:00. Override per-tenant в `tenant_settings.working_hours`.
- [Государственные праздники РФ 2026](http://www.consultant.ru/document/cons_doc_LAW_34683/) — 1–8 января, 23 февраля, 8 марта, 1, 9 мая, 12 июня, 4 ноября + переносы. Hardcoded в `internal/dialer/hours/holidays.go` массив `RUHolidays2026`.

## Gotchas (do-not-do list)

- **DO NOT** stick FSM state in Postgres for the per-tick read path. Redis is the source-of-truth for "is operator X ready right now?" because the dispatch loop reads it 10× per second per operator. Postgres `operator_state_log` is the audit trail (eventually consistent via outbox).
- **DO NOT** use `pg_advisory_lock` (blocking) for leader election — only `pg_try_advisory_lock` (non-blocking). The blocking variant queues a worker behind the existing leader and pinpoints a deadlock if the leader's session was closed mid-tx.
- **DO NOT** put RDD generation on the request path. RDD is a background asynq job — synchronous generation of 100k numbers takes 1.5–2 s and would tie up gin workers.
- **DO NOT** bypass `LineCapacityTracker.Acquire` even on retries. The cap is the wire-protocol invariant from FreeSWITCH (max 60 channels per node before SIP REGISTER fails). Retry MUST `Acquire` again, never reuse the previous slot.
- **DO NOT** silently re-enqueue a `wrong-person` status. That counts toward DNC, not retry. The status_rules table is authoritative — read it before adding any branch.
- **DO NOT** keep `current_call_id` set after `SubmitStatus`. The FSM mutator MUST clear it; subsequent `RecordCallStarted` would otherwise see stale data.
- **DO NOT** roll your own time-zone math. `time.LoadLocation("Europe/Moscow"); t.In(loc)` is the only correct path. Never compute UTC-offsets manually — DST in some RU regions is a thing of the past but the data still has historical entries.
- **DO NOT** use `time.After` in select loops (Plan 09 gotcha #1). Always `time.NewTicker` / `time.NewTimer` with `defer t.Stop()`.
- **DO NOT** call `Router.Dial` while holding the FSM transition lock. `Dial` publishes to NATS and may block; the FSM transition is a Redis CAS — keep them separate.
- **DO NOT** declare `var ErrXxx = errors.New(...)` in implementations and expect `errors.Is` to work across modules. Alias to `api.ErrXxx` (Plan 09 gotcha — caught by reviewer).
- **DO NOT** start a Redis transaction (MULTI/EXEC) and then call out to Postgres or NATS inside. Lua scripts are the right tool for atomic Redis-only state. Cross-resource atomicity → outbox pattern.
- **DO NOT** delete rows from `respondents` directly on DNC. Update `status='dnc'`. The audit trail relies on the row staying.

## Open questions

- **Q**: Backpressure between dialer and telephony-bridge. Plan 09 already implements per-node `op:active_channels:{node}` with cap=60 in `internal/telephony/router/backpressure.go`. Plan 10's `LineCapacityTracker.Acquire` should reuse that, NOT introduce a parallel counter. Resolution: Task 6 wraps `internal/telephony/router.Backpressure` exposing it through the `api.LineCapacityTracker` shape, choosing a node by `Pool.HealthyNodes()` + `RoundRobin.Pick`. **No new Redis key.**
- **Q**: How does the dialer know the operator's SIP extension? The plan says `req.OperatorExt` flows into `DialRequest`. But that field's source: `users.sip_user` column. The FSM needs to load this on `StartShift` and cache in the Redis hash, OR the HTTP layer fetches it from `auth.UserService.GetByID` and passes through. **Resolution**: HTTP layer fetches from `users` table at session start; we cache in `op:<t>:user:<id>.sip_ext`.
- **Q**: NATS subject for telephony events. Plan 11 owns this — the dialer's `Router` SHOULD use the existing `tenant.<t>.telephony.event.<call_id>.*` subject family that Plan 09 will (eventually) publish to. Until Plan 11 ships the real `nats_bridge`, the dialer's Router subscribes to the topic but receives nothing. Smoke tests use a test-local fake bridge.
- **Q**: Idempotency of `RecordCallStarted`. Plan says "idempotent — repeating an event in a state where it's already applied is a no-op". But `RecordCallStarted` from `dialing` → `call` mutates `current_call_id`; replay with the SAME `call_id` is a no-op, but with a different `call_id` is a bug. Resolution: in mutator, check `s.CurrentCallID != nil && *s.CurrentCallID != req.CallID` → return `ErrInvalidTransition` wrapped with both call IDs.
- **Q**: Should `EndShift` from `call` state be allowed? The plan transition table allows EndShift only from `ready/pause/status`. But what if FreeSWITCH dies mid-call? **Resolution**: NOT allowed via the public method. Watchdog uses `Force(target=offline, reason="heartbeat_lost")`; production op uses the same.
- **Q**: How big is the operator hash key TTL? Plan 09 used 1h for backpressure counter. Plan 10 OperatorFSM hash needs to outlive a typical shift (~8h). **Resolution**: 24h TTL refreshed on every transition. Heartbeat refreshes every 30s (kept separate from state hash).

## Path corrections (carry-over from Plan 09)

The plan body uses `social-pulse/...` as the import path. **Real module path is** `github.com/sociopulse/platform/...`. Substitute everywhere when reading the plan.

## Subagent dispatch checklist (from Plan 09 lessons)

When dispatching an implementer for any Plan 10 task:

1. Include the file path `docs/references/plan-10-dialer.md` in the prompt.
2. Include the path `docs/references/COMMON.md` for cross-cutting (zap, gin, golangci-lint, RLS).
3. Include the path `docs/references/plan-09-telephony-bridge.md` because **the dialer reuses telephony backpressure** — the implementer needs to read the existing patterns.
4. Specify Go 1.26 modernize idioms explicitly (maps.Copy, range over int, etc.) — golangci-lint enforces these.
5. Module path: `github.com/sociopulse/platform`. NEVER `social-pulse`.
6. Logger: `*zap.Logger` (NEVER `slog`). HTTP: `gin.RouterGroup`. Postgres: `pkg/postgres`. Redis: `redis.Client` from go-redis/v9. NATS: real publisher when available; nil-tolerant slot until Plan 11.
7. Tests: `testcontainers-go` for Redis 7.4 + Postgres 16; `miniredis/v2` for fast unit tests; `goleak.VerifyTestMain` in every package with goroutines.
8. Two-stage review after EVERY implementer return: spec compliance reviewer first, then code quality reviewer. Don't merge fixes inline.

---

## Lessons learned (filled at close-out)

_TBD — populate at v0.0.11 tag time with what we actually had to work around._
