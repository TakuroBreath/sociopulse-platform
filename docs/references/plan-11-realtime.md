# Plan 11 — realtime references

> **Goal**: snap the realtime module (WebSocket Hub + NATS dispatcher + Redis presence + listen-in v1) to authoritative external sources. Loaded into every Plan 11 implementer prompt.

**Status**: shipped at `v0.0.12-realtime` (2026-05-09).

**Module path**: `github.com/sociopulse/platform` (NOT `social-pulse`).

---

## Carry-overs from prior plans (handled by Plan 11)

These open carry-overs from Plans 06/09/10 land here:

1. **`internal/telephony/nats_bridge`** (Plan 09 stub) — subscribe to `tenant.<t>.telephony.cmd.>`, publish `tenant.<t>.telephony.event.>`, idempotency via Redis SETNX (TTL 24h).
2. **Per-call SIP credentials manager** — `mod_xml_curl` callback to `cmd/api`'s `/internal/freeswitch/directory` endpoint (per-call SIP user provisioning for listen-in legs).
3. **`pkg/eventbus.NATSPublisher` / `NATSSubscriber`** — currently `panic("not implemented: see Plan 03 Task 7")`. Real JetStream-backed impl lands here.
4. **`cmd/api` outbox publisher** — currently `noopPublisher`. Replace with real NATS publisher backed by `pkg/eventbus.NATSPublisher`.
5. **Dialer `RefreshPresence` wiring** — `internal/dialer/fsm.RefreshPresence` was exported by Plan 10 but not yet called from `internal/dialer/transport/http`. Wire as gin middleware on operator routes so the Heartbeat watchdog only triggers on ungraceful disconnect.
6. **Dialer `SnapshotPubSub` upgrade** — Plan 10 ships an in-memory PubSub. Plan 11 swaps for NATS-backed fan-out so cross-replica subscribers see transitions. Adapter pattern: keep the `dialer.PubSub` Subscribe API but back `Publish` with NATS publish to `tenant.<t>.dialer.op.<op_id>.state`; subscribers translate NATS events back into `api.Snapshot`.

## Canonical specs (must-read)

- [RFC 6455 — The WebSocket Protocol](https://datatracker.ietf.org/doc/html/rfc6455) — authoritative wire format. Read §5 (Data Framing), §7 (Closing the Connection — close codes), §10 (Security Considerations). Plan 11 uses close codes 4401 (unauthorized), 4403 (forbidden), 4503 (slow consumer), 1000 (normal close).
- [WHATWG WebSocket living standard](https://websockets.spec.whatwg.org/) — modern reference; clarifies subprotocols and origin checking.
- [NATS JetStream concepts](https://docs.nats.io/nats-concepts/jetstream) — durable consumer model; `nats.JetStream.Subscribe(subject, ...)` semantics; ack/nack/redelivery.
- [NATS subject hierarchy + wildcards](https://docs.nats.io/nats-concepts/subjects) — `tenant.<t>.>` matches every dialer/telephony/notify subject for that tenant. Use `>` (multi-token) NOT `*` (single-token).
- [Redis SCAN + EXISTS + TTL](https://redis.io/commands/scan/) — presence sweeper iterates keys without blocking.
- [Redis SETNX with PX](https://redis.io/commands/set/) — `SET key value PX 86400000 NX` for nats_bridge idempotency.
- [Server-side ping/pong cadence (Cloudflare blog, generic best practice)](https://developers.cloudflare.com/workers/learning/using-websockets/) — 30s ping period, 60s pong-grace, drop on miss. Plan 10 dialer transport/http already follows this; Plan 11 reuses.

## Reference implementations

- [`coder/websocket`](https://github.com/coder/websocket) (formerly `nhooyr.io/websocket`) — modern ctx-aware Go WS lib. Already in `go.mod` v1.8.14 (Plan 10 dialer transport/http uses it). Plan 11 stays on the same version.
  Files of interest: `accept.go` (Accept options + subprotocols), `read.go` (Read with ctx), `write.go` (Write with ctx + close-on-error semantics), `examples/echo/`.
- [`nats-io/nats.go`](https://github.com/nats-io/nats.go) v1.34+. JetStream client. Already in `go.mod`.
  Files of interest: `js/jetstream.go`, `pull_consumer.go`, `push_consumer.go`. **Use push consumer** for subjects with low message rates (notifications, force-commands); **pull consumer** for high-throughput (call lifecycle).
- [`redis/go-redis/v9`](https://github.com/redis/go-redis) v9.5+. Already in `go.mod`. Used for presence + SETNX idempotency.
- [`coder/websocket/wsjson`](https://pkg.go.dev/github.com/coder/websocket/wsjson) — typed JSON read/write helpers. Use for the auth handshake; raw `Reader/Writer` for the high-throughput event stream.

## Production lessons (blog posts, talks)

- **[Coder.com blog: Designing a websocket server](https://coder.com/blog/websocket-design)** — author of `coder/websocket` describing read/write goroutine separation, slow-consumer handling, and the "bounded buffer + drop-oldest" pattern. Plan 11 Task 2's drop-oldest impl is straight from this article.
- **[NATS JetStream production checklist](https://docs.nats.io/running-a-nats-service/configuration/clustering/jetstream_clustering)** — message redelivery, consumer pull/push tradeoffs, max_deliver settings.
- **[Plan 10 lessons (from `docs/references/plan-10-dialer.md`)](plan-10-dialer.md)** — read all 20 bullets. The reviewer pattern caught 7 issues in Plan 10; same patterns apply here. Especially:
  1. `redis.NewScript` not raw EVAL.
  2. `time.NewTicker` not `time.After` in loops.
  3. `var _ api.X = (*Impl)(nil)` compile-time check.
  4. Sentinel errors aliased via `var ErrFoo = api.ErrFoo` for `errors.Is` across module boundaries.
  5. Per-package `RegisterMetrics(reg)` injection — NEVER `init()`-time `MustRegister`.
  6. `wg.Go(...)` (Go 1.25+) not `wg.Add(1)/wg.Done`.
  7. `goleak.VerifyTestMain` in every package with goroutines (the realtime hub has many — apply rigorously).
  8. `*zap.Logger` typed fields, no PII (no auth tokens, no SIP creds, no full phone numbers in logs — last 4 digits only at debug level).
  9. `goleak`-clean exit paths for every goroutine on ctx.Done / channel close / parent close.
  10. Build-tag false alarms from gopls — always reality-check via direct `go test -tags=integration ...`.

## Russian-specific (152-ФЗ, listen-in compliance)

- **Listen-in audit requirement**: every listen-in start MUST emit an `audit_log` row with actor=admin/supervisor user_id + target=operator user_id + call_id + start/stop timestamps. 152-ФЗ doesn't require explicit consent for listen-in (operator under employment contract acknowledges monitoring), but the audit trail is the legal-defence layer.
- **Recording retention** vs listen-in: listen-in is silent/whisper/barge LIVE only — it does NOT create a recording. The recording subsystem (Plan 12) handles retention separately.

## Architecture decisions (locked in this references doc)

### Decision 1: Hub is per-replica, NATS is the cross-replica fabric

Each `cmd/api` pod runs its own `*service.Hub` holding LOCAL connections. Cross-replica fan-out happens entirely through NATS subjects. The Hub subscribes once per tenant prefix `tenant.<t>.>` and dispatches to local subscribers matching topic+filter.

Rationale: avoiding shared-state Redis pubsub (extra hop, extra failure mode). NATS is already the durable backbone. Connections never need to know about other replicas.

### Decision 2: Hub is NOT in the api/ surface

The `Hub` interface IS in `internal/realtime/api/interfaces.go` (already in Plan 00a). But other modules NEVER call `Hub.Broadcast` directly — they publish to NATS with the canonical subject and the dialer/telephony module's NATS subscriber handles fan-out via the Hub.

### Decision 3: Listen-in v1 = silent only

Whisper / barge / 3-way require deeper FreeSWITCH conferencing (mod_conference). v1 ships silent listen-in (`mixmonitor` outputs to a dedicated SIP user the admin's browser dials in via verto). Whisper/barge are stub interfaces that return `ErrListenModeNotSupported` until v1.1.

### Decision 4: Defer Task 8 (Helm) to sociopulse-infra

Task 8 of Plan 11 is Helm + ingress timeouts. That repo is separate (Plan 01 territory). Plan 11 ships the application code; the infra repo ships the values.yaml.

### Decision 5: Defer Task 6 (Listen-in v1) until Plan 08 lands

Listen-in depends on real FreeSWITCH being reachable. Until Plan 08 (FreeSWITCH cluster) lands, the listen-in service is wired with a stub telephony.api.CommandPublisher that returns `ErrTelephonyBridgeOffline`. Tests use a fake. Real-FS integration test moves to Plan 08.

## Gotchas (do-not-do list)

- **DO NOT** broadcast to ALL connections without RBAC + tenant filter. Every Hub.Dispatch call MUST consult the `topics.go` RBAC matrix and reject with `ErrSubscriptionForbidden` for unauthorized topics.
- **DO NOT** trust the JWT subject claim for cross-tenant ops. Every WS message handler MUST re-verify `claims.TenantID == req.TenantID` for the subject the message refers to.
- **DO NOT** block the writer goroutine on a slow client. Always non-blocking send via select+default; on full buffer, drop oldest frame and increment `dropped_frames_total{conn}`.
- **DO NOT** spawn unbounded goroutines per connection. The lifecycle is: 1 reader goroutine + 1 writer goroutine + 1 ping goroutine. Any extra goroutine must have an explicit lifecycle bound to the connection.
- **DO NOT** write directly to the WS conn from random goroutines. ONLY the writer goroutine writes; everything else sends via `Connection.Send(frame)` which is the only public mutator.
- **DO NOT** use `time.After` in select loops (Plan 09/10 carry-forward).
- **DO NOT** bypass the Hub's RBAC check for the `op.commands` topic — server→client force-commands MUST authenticate the publisher (only force_handler.go publishes to op.commands).
- **DO NOT** publish PII to NATS subjects. Use opaque IDs; resolve PII server-side at delivery via the auth/crm services with proper RBAC.
- **DO NOT** reply to a WebSocket close frame with another close frame — coder/websocket handles this. Just `conn.Close(code, reason)` and let the lib finish the handshake.
- **DO NOT** spawn the JetStream subscriber from `Module.Register` — Plan 11 Task 4 spec requires the subscriber to be `errgroup`-driven from the cmd/api composition root so graceful shutdown is centralised.

## Open questions

- **Q1**: Should the Hub maintain a per-tenant connection map, or one flat map keyed by conn-id? **Resolution**: per-tenant map for fast multi-cast; flat map for admin debug. Both, with the per-tenant map as primary.
- **Q2**: Should we use NATS push consumer or pull consumer for `tenant.<t>.dialer.op.<op>.state`? **Resolution**: push consumer with `MaxAckPending=1024`. Per-replica subscriber, queue group `realtime-replica-<podname>` so each replica gets its own copy of every event (each replica needs to dispatch to its local connections; NATS queue group on a per-replica name = effectively no queue group, every replica consumes).
- **Q3**: Idempotency for telephony NATS bridge — what window? **Resolution**: 24h SETNX TTL on `idempotency:<command_id>` so a re-published command (after a publisher crash + replay) is dropped.
- **Q4**: How long does a WS conn live without traffic? **Resolution**: 30s ping period, 60s pong-grace = drop after 90s. Token refresh window every 4 minutes (JWT typically 5-min TTL; client refreshes 1m before expiry).
- **Q5**: How does the Hub handle JWT token rotation mid-WS-session? **Resolution**: client sends `FrameRefresh` with new token; server validates + responds with `FrameRefreshOK` (or close 4401 on bad). Existing subscriptions stay; new claims apply to subsequent RBAC checks.

## Subagent dispatch checklist (carry-over from Plan 10)

When dispatching an implementer for any Plan 11 task:

1. Include the file path `docs/references/plan-11-realtime.md` in the prompt.
2. Include `docs/references/COMMON.md` for cross-cutting (zap, gin, golangci-lint, RLS).
3. Include `docs/references/plan-10-dialer.md` because **the dialer's PubSub is the test seam Plan 11 swaps**.
4. Include `docs/references/plan-09-telephony-bridge.md` because **the telephony nats_bridge is finally implemented here**.
5. Specify Go 1.26 modernize idioms explicitly (maps.Copy, range over int, min/max, slices.ContainsFunc, wg.Go, etc.) — golangci-lint enforces these.
6. Logger: `*zap.Logger`. HTTP: `gin.RouterGroup`. WS: `coder/websocket` v1.8.14. Postgres: `pkg/postgres`. Redis: `redis.Client` from go-redis/v9. NATS: `nats.go` v1.34+ via `pkg/eventbus`.
7. Tests: `testcontainers-go` for real Redis 7.4 + NATS 2.10; `miniredis/v2` for fast unit tests; `goleak.VerifyTestMain` in every package with goroutines (Hub spawns MANY).
8. Two-stage review after EVERY implementer return: spec compliance reviewer first, then code quality reviewer.

---

## Lessons learned (Plan 11 close-out)

These bullets were populated when `v0.0.12-realtime` shipped. Future plans inherit them.

### Architecture & API discipline

1. **api/ surface stayed UNCHANGED throughout Plan 11.** Plan 00a's `Hub` / `Connection` / `WSConn` / `PresenceTracker` interfaces survived 7 implementation tasks without a single signature change. The only api/ additions were the supplementary `topics.go` (`AllTopics` + `TopicAction`) and `locator.go` (3 string constants). Lock the api/ as early as possible — by the time Plan 11 ran, the api/ was stable for ~3 days.

2. **Locator-key constants live in `api/`, not the consumer's package.** `rtapi.LocatorHub` etc. let Plan 11 Task 7 (HTTP handler) and any future consumer resolve the Hub through the locator without taking a transitive dependency on `internal/realtime/service`. This was caught by code-quality reviewers and is now a permanent pattern.

3. **`Hub.AttachForTest` is misnamed but correct.** The production HTTP handler reuses it because the alternative (a Hub method that bypasses the wire-side AuthHandshake) would duplicate logic. The "ForTest" suffix should be renamed to `Attach` in a follow-up; documented inline in the WS handler.

4. **DO NOT spawn the JetStream subscriber from `Module.Register`.** This single gotcha (line 97) drove the whole Task 4c shape: `realtime.Module.Register` builds the Hub but the dispatcher's lifecycle lives in `cmd/api` so its Start/Stop is errgroup-driven from the composition root. Centralised graceful shutdown is more important than module-internal symmetry.

### NATS / JetStream

5. **Embedded `nats-server/v2` is the better test infra over `testcontainers-nats`.** No Docker dependency, ~200ms boot per test, fully in-process. Per-test random port + temp `StoreDir` keeps tests hermetic. The implementer chose this for Task 4a after weighing both options; matched and reused for the integration test in Task 9.

6. **JetStream push consumer with MaxAckPending=1024 + per-replica queue group.** Plan 11 Q2 resolution: each replica registers a UNIQUE queue group (`realtime-replica-<podname>` or `<uuid>`) so each replica receives every event. Same-queue subscribers load-balance; different-queue subscribers fan out. Tested explicitly in `pkg/eventbus/nats_test.go:TestNATSPublisherSubscriber_FanOut`.

7. **Sync publish is non-negotiable for outbox-relay drainage.** `js.PublishMsg(msg, nats.Context(ctx))` returns only after broker ack. Fire-and-forget would race the outbox status update.

8. **Connection options matter at boot.** `RetryOnFailedConnect(true)` + `MaxReconnects(-1)` + `ReconnectWait(2s)` + `Name(...)` survive NATS restarts and identify the connection in NATS server logs. `nats.Timeout(5s)` bounds the dial so `dialNATS` doesn't hang the boot.

9. **Subject-pattern dispatch is lock-free per-message.** The realtime dispatcher uses one `Subscribe` call per pattern (5 patterns, 5 subscriptions). The closure captures the `subjectPattern` per-iteration (Go 1.22+ scope rules) — no manual copy. `strings.Split(subject, ".")` happens once per delivery; the result is reused for both the token-count check and the per-pattern `extract`.

10. **Histogram buckets must match the workload.** `pkg/eventbus.PublishLatency` was originally `prometheus.DefBuckets` (5ms..10s); for healthy-cluster p99 < 50ms, default buckets land 80%+ of samples in the 5-25ms bucket and waste resolution above 100ms. Replaced with custom `{0.5ms..5s}` buckets in the Task 4a follow-up review.

### WebSocket / Connection lifecycle

11. **`coder/websocket` v1.8.14 is the project standard, NOT `nhooyr.io/websocket`.** Same library, new module path. The plan-file Task 7 still references `nhooyr.io` — outdated; Plan 11 Task 7 implementer was explicitly told NOT to copy that section verbatim. Documented in the plan-file as a maintenance lien.

12. **One reader + one writer + one pinger + one Touch ticker = the goroutine budget per WS conn.** `Connection.runReader` / `runWriter` / `runPinger` (Plan 11 Task 2) plus the Plan 11 Task 7 Touch goroutine for presence refresh. ALL spawned via `wg.Go` (Go 1.25+). `goleak.VerifyTestMain` catches any leak.

13. **Drop-oldest backpressure on a per-conn `sendChan`.** A slow consumer is interested in the LATEST state — a stale operator-state transition is more useless than missing one. Dropping the OLDEST queued frame (channel-receive then channel-send the new) is the right pattern for telemetry. Plan 11 Task 10.1 will split this into critical/telemetry queues.

14. **Per-pod refcount for multi-conn-same-user OnDisconnect.** PresenceTracker key is per-(tenant, user) but a single user can have multiple WS connections on the same pod. The WS handler maintains `map[tenant]map[user]int` and only fires PresenceTracker.OnDisconnect on the 1→0 transition. This is per-pod local; cross-pod scenarios rely on TTL refresh from surviving pods.

15. **Subscribe-by-frame goes through `SetHubCallback`.** Plan 11 Task 7 wires `Connection.SetHubCallback` so `FrameSubscribe` / `FrameUnsubscribe` flow through `Connection.Subscribe`/`Unsubscribe` and emit `subscribe.ok`/`subscribe.error` on the wire. The default arm of the dispatch logs at Debug — defensive in case a future regression forwards a different frame kind.

### Redis / Presence

16. **`SET key replicaID PX <ttl_ms>` for OnConnect; `PEXPIRE` for Touch; SET-not-EXPIRE on missing.** Touch on a missing key (key already expired or never set) returns `ErrPresenceLapsed` — the Hub then closes the connection. **Touch MUST NOT silently re-create the key.** Regression-tested in `presence_test.go:TestPresence_TouchDoesNotResurrectMissingKey`.

17. **`strings.SplitN(_, ":", 4)` for the SCAN result, NOT a custom splitN.** The plan-file's illustrative impl had a hand-rolled splitter; Plan 11 Task 5 replaced it with stdlib. Same lesson applied across the project — stdlib over hand-rolled when the semantics are equivalent.

18. **`slices.Sort` for OnlineUsers determinism.** The SCAN order is implementation-defined; tests would flake without a deterministic projection. `slices.Sort([]string)` is stable, deterministic, and free.

### Pipeline & review

19. **2-stage review (spec compliance + code quality) on every task.** Plan 11 ran 7 implementation tasks; reviews caught 13 issues (0 blocker + 2 important + 11 nit) BEFORE merge. The 2 importants were both modernize/style misses (not behavioural). Pattern is: dispatch implementer subagent (opus) → spec reviewer → code-quality reviewer → batch fixes inline → commit follow-up. Same pattern as Plan 10.

20. **Embedded NATS server boot needs a stream BEFORE Publish.** JetStream-backed publishes fail with "no matching stream" if the subject isn't covered by a configured stream. `pkg/eventbus/helpers_test.go:ensureStream` provisions one per test; `internal/realtime/integration_test.go` reuses the same helper.

### Carry-overs from prior plans (NOT closed in Plan 11)

21. **`internal/telephony/nats_bridge` is still a stub.** Originally listed as a Plan 11 carry-over but the scope was too large to land cleanly with the realtime work. Now Plan 11.1.

22. **Dialer `SnapshotPubSub` is still in-memory.** Cross-replica fan-out for operator-state was supposed to swap to NATS in Plan 11; deferred to Plan 11.1.

23. **`internal/dialer/fsm.RefreshPresence` is exported but not wired.** Same story — intended as Plan 11 work, deferred. Need a gin middleware on operator routes so the Heartbeat watchdog only triggers on ungraceful disconnect.

24. **Listen-in v1 (silent mode + audit) is still deferred to Plan 08.** Plan 11 Decision 5 locked this in; Plan 08 (FreeSWITCH cluster) is a prerequisite. Plan 11 Task 7's listen-in handlers return 503 + `telephony.bridge.offline` until then.

---

## Plan 11.4 Production lessons (post-execution 2026-05-10, `v0.0.20-auth-user-deleted-callresolver`)

These bullets capture what we actually learned executing Plan 11.4 — closing the auth.user.deleted carry-over from 11.3 + the CallResolver carry-over from 11.2.

### Architecture & API discipline

25. **Optional callback fields preserve degraded boot.** `*CacheInvalidator` now has 3 callback config fields: `ProjectInvalidate` (REQUIRED — preserves Plan 11.3 contract; nil panics) plus new `UserInvalidate` and `CallInvalidate` (OPTIONAL — nil → skip Subscribe + INFO log). This shape lets cmd/api boot with auth-only or recording-only or both, without changing the invalidator's surface. Mistake to avoid: making the new ones REQUIRED would break degraded test boots.

26. **Metric label evolution is breaking but the project has no external dashboards yet.** `realtime_cache_invalidations_total{result}` → `{subject, result}`, plus `empty_project_id` → uniform `empty_id`. Documented inline in `metrics.go`. If we had Grafana dashboards subscribed to this metric, the rename would be a coordinated migration; in v1 it's just internal evolution. **Standing rule for future plans:** when adding labels to bounded counters, the old query stops returning data — flag this as a breaking change in the milestone description.

27. **Empty-fallback chain is the security envelope on degraded boot.** `emptyCallResolver{}` + `emptyUserResolver{}` + `emptyProjectResolver{}` all return `ErrCrossTenantSubscribe` for every input. When cmd/api hasn't wired the real adapter, the cache layer caches "always reject" entries — the system fails CLOSED rather than open. This is the difference between "no check" and "explicit deny" — pick deny for security-sensitive ports.

28. **The 4 close-out task pattern is an integration-validation lens, not a code-extension.** Task 7 was 90% wiring (cmd/api adapter + module.go binding + 3 method values) and 10% new code. The reviewer's end-to-end behavioural property check (`tenant.X.recording.call.deleted` → `handleCall` → `cachedCalls.Invalidate` → next `CallResolver.Get` re-queries) was the actual deliverable — confirming the chain is connected. For future closing-wiring tasks, structure the review around the END-TO-END flow, not just file-by-file diff.

### Outbox pattern (auth's first NATS subject)

29. **`outbox.Writer.Append` works inside `WithTenant Tx` even when the table is owned by `tenancy_admin`.** `event_outbox` is owned by `tenancy_admin`; `app` retains full CRUD grants (Plan 03 setup). So a `WithTenant Tx { store.X + writeAudit + outbox.Append }` triple commits atomically without role-switching. Plan 12.1 verified this against migrations 000001 + 000002 + 000009; Plan 11.4 confirmed it for the auth path. Reusable rule: any module wanting to publish lifecycle events follows this exact triple, no surprises.

30. **`UserDeletedEvent` payload is opaque-UUIDs only.** `{UserID, TenantID, DeletedAt unix-seconds, Reason "archived"|"hard_deleted"}`. NO phone, email, login, full_name on the bus. The PII discipline scales: any future `tenant.<t>.auth.user.<verb>` subject MUST follow the same shape. The cache invalidator only needs the `user_id` to drop a cache entry — it does NOT need any of the deleted user's PII.

31. **Constructor signature evolution requires updating every test call site.** Adding `outboxWriter outbox.Writer` between `auditLogger` and `clock` in `NewUserService` — 22 existing test call sites. The implementer used the existing test-file's `newSvc(t)` helper (which now returns a 4-tuple) to localise the impact. Pattern: a single `newSvc(t)` constructor in test code makes future signature changes a 1-line edit instead of N edits.

### Resolver cache (third dimension)

32. **`CachedCallResolver` lives in its own file by design.** `internal/realtime/service/resolver_cache.go` is at 287 lines (User + Project mirrors). Adding the third copy would push past 400 lines in the same file — harder to hold in working memory. Lesson: split when adding the THIRD copy, not the second; reuse the unexported `cachedResolverEntry` + `defaultResolverTTL` + `resolverInnerTimeout` from the original file. Future agents touching the resolver cache need to read both files to understand the full triplet.

33. **`stubResolver` in `rbac_test.go` is generic — same struct satisfies all three resolver ports.** The `Get(ctx, id) → (ResolvedTenant, error)` signature is the SAME for User / Project / Call resolvers. The existing helper at `rbac_test.go:26-30` works for the new dimension without modification. Reusable rule: when adding a new resolver, check the test helper FIRST — odds are it's already generic.

34. **Plan 11.2 Task 3 IMPORTANT I-1 (singleflight ctx-bleed) is now regression-tested for THREE dimensions.** `context.WithoutCancel(ctx) + context.WithTimeout(5s)` inside the singleflight closure is the canonical fix. Each `Cached*Resolver` has its own `Test*_LeaderCtxCancelDoesNotPoisonDuplicates`. If the 4th resolver dimension ever lands (e.g. `RecordingResolver` for full recording metadata vs. just tenant), it MUST carry the same regression guard.

### Recording-side BypassRLS lookup

35. **`tenancy_admin` SELECT grant on `call_recordings`** was added by Plan 12.4 migration 000011. Plan 11.4 Task 4's `LookupTenant` BypassRLS SELECT relies on it — verify-before-assert step caught no surprises. Future cross-tenant lookups on recording tables must verify the same grant or add a new migration if the table is a different one (e.g. a hypothetical `recording_artifacts` would need its own grant).

36. **`recording.api.CallTenantLookup` is intentionally separate from `RecordingService.Get`.** The latter requires `tenantID` at the boundary; the former takes only `callID` and resolves via BypassRLS. Keeping them separate keeps the public RecordingService surface stable (HTTP + gRPC consumers don't see a new method). Reusable rule: when adding a tiny cross-tenant lookup, prefer a NEW narrow port over expanding an existing service interface.

37. **The cross-tenant integration test ACTUALLY exercises BypassRLS.** `TestPostgresStore_LookupTenant_BypassRLS_CrossTenant` seeds the row under tenantA, then enters `pool.WithTenant(tenantB, ...)` and calls `LookupTenant(callA)` from inside that scope. If the impl had used a regular tenant-scoped connection, RLS would hide the row and the test would FAIL. This test is the difference between "lookup compiles" and "lookup actually works cross-tenant".

### Pipeline & review

38. **2-stage review per task + final-implementation review caught everything.** Across 7 tasks: 0 CRITICAL + 0 IMPORTANT (the one IMPORTANT was a stale doc fixup landed inline in `dd9be37`) + ~6 MINOR (most deferred). 28 new tests / 28 t.Parallel() calls — perfect 1:1. Final reviewer (independent) reproduced the canonical commands and confirmed all 6 CI jobs would pass. **The pipeline pattern's value compounds**: each task's reviewer learns from the prior tasks' findings, so the lessons-learned hierarchy stays current.

39. **Re-review proportionality (`09-agent-workflow-improvements.md` #7) saved 2 review-rounds.** Task 1 had 1 IMPORTANT (stale package comment, doc-only) + 1 meaningful MINOR (idempotent test outbox-count pin). Total ~13 lines, no behavior change. Controller fixed inline in `dd9be37` — no full re-review needed. Heuristic worked as designed: 6-30 lines + doc/test only = single re-review or skip.

40. **Gopls cache pollution is a real and consistent pattern across all 7 tasks.** Every task triggered `<new-diagnostics>` warnings claiming the just-added function/constant was undefined. Every time, `go build ./...` was clean and tests passed. Standing rule from CLAUDE.md workflow #5 worked exactly as documented: "If those pass, the IDE diagnostics are noise." Reusable telemetry: the controller wasted 0 minutes investigating these — the canonical commands resolved every false-positive in <30 seconds each.

### Carry-overs from prior plans (NOW CLOSED in Plan 11.4)

41. **Plan 11.2 Task 5 NIT M-3 — wire-string scrub on `FrameSubscribeErr.Reason`** — was carried over to Plan 11.3 Task 1 and CLOSED there. Not re-opened in 11.4.

42. **Plan 11.3 deferred: auth user-deleted cache invalidation** — CLOSED by Plan 11.4 Task 1 (auth publishes) + Task 6 (CacheInvalidator subscribes) + Task 7 (cmd/api binds `cachedUsers.Invalidate`).

43. **Plan 11.2 deferred: CallResolver (Plan 12 dependency)** — CLOSED by Plan 11.4 Tasks 2-5 + 7. The wiring depends on Plan 12.4 having shipped (`v0.0.19-recording-workers`); now it's all live.

### Carry-overs still open (NOT closed in 11.4)

44. **Plan 11.1 carry-overs** (`internal/telephony/nats_bridge`, `dialer.SnapshotPubSub` NATS upgrade, `dialer.fsm.RefreshPresence` middleware) — still pending. Not in scope for 11.4.

45. **Plan 11.2 listen-in cleanup hooks (Plan 11 Task 10.2)** — still deferred until Plan 08 (FreeSWITCH cluster) lands.
