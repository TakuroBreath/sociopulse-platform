# Plan 09 — telephony-bridge sidecar references

> **Goal**: shrink the implementation surface for Plan 09 by collecting authoritative ESL/FreeSWITCH refs, prior-plan lessons, and the project-specific path corrections subagents need before writing code.

**Status**: shipped at `v0.0.10-telephony-bridge` (2026-05-08).

---

## Pragmatic decisions locked (read these FIRST)

These are not negotiable in this plan — they reflect either user directive or repo-wide conventions discovered during Plans 02–07. Subagents that ignore these will be sent back.

1. **Logger: `go.uber.org/zap`** — NOT `slog`. The plan's draft uses `slog` because it was written before our zap convention was fixed. All telephony code logs through `*zap.Logger` (constructor `pkg/observability.NewLogger(cfg)`). User directive: «для логирования прошу тебя использовать zap».
2. **HTTP framework: `gin` + ecosystem** — for the `/internal/freeswitch/directory` endpoint that lives in `cmd/api`. Use `pkg/observability.GinLoggingMiddleware` chain. User directive: «для http использовать gin и его экосистему».
3. **Module path: `github.com/sociopulse/platform`** — NOT `github.com/sociopulse/social-pulse`. The plan's draft uses the wrong slug. **Fix every import.**
4. **Config: `pkg/config` (Viper + `atomic.Pointer[Config]` Snapshot)** — NOT `caarlos0/env`. `TelephonyConfig` is already declared in `pkg/config/telephony.go`:
   ```go
   type TelephonyConfig struct {
       Bridge  TelephonyBridgeConfig  // FSNodes, HealthcheckInterval, MaxConcurrentPerNode
       Trunks  []TrunkConfig          // ID, SIPGateway, CapacityChannels, CostPerMinuteRub, Weight, ...
       Routing TelephonyRouting       // DefaultStrategy
   }
   ```
   `cmd/telephony-bridge/main.go` calls `config.Load(...)` exactly like `cmd/api/main.go` does.
5. **Observability: `pkg/observability`** — NOT `internal/observability`. The plan's draft path is wrong. Use:
   - `observability.NewLogger(cfg) (*zap.Logger, error)`
   - `observability.NewTracerProvider(ctx, cfg)` (already exists)
   - `observability.NewMeterProvider(ctx, cfg)` (already exists)
   - `observability.GinLoggingMiddleware(logger)` chain
6. **Existing telephony api surface MUST be honored.** `internal/telephony/api/` already declares:
   - `CommandPublisher` (Originate / Hangup / Mixmonitor / Play / CreateUser / DeleteUser) — NATS-side façade.
   - `EventConsumer` (Subscribe(tenantID) → unsubscribe).
   - `Router.Select(ctx, SelectRequest) (SelectionResult, error)`.
   - `LineCapacityTracker.Acquire/Release/Stats`.
   - `MixmonitorMode`, `ChannelEventType`, `RoutingStrategy` enums.
   - `OriginateCommand`/`HangupCommand`/`MixmonitorCommand`/`PlayCommand`/`CreateUserCommand`/`DeleteUserCommand` DTOs.
   - `ChannelEvent` event DTO.
   The new code IMPLEMENTS these — does not re-declare them. The plan draft introduces `OriginateRequest` etc. as locally-defined; they're internal-to-`esl/` only and must NOT collide with `api.OriginateCommand`. Convert at the boundary.
7. **Postgres access via `pkg/postgres`**, NOT raw `pgxpool.New`. Reading trunk config doesn't need RLS (it's tenant-agnostic infra config), but consistent depguard rule: `pgxpool` import only allowed inside `pkg/postgres`. Use `pkg/postgres.Open` + `BypassRLS` for reads.
8. **Audit logger noop fallback** (Plan 05 lesson). Look up `audit.Logger` in `modules.Locator`; if missing, use the no-op + `logger.Warn("audit logger missing — using noop")`. Don't panic.
9. **NATS publisher slot pattern** (Plan 05/06/07 lesson). `nats_bridge.Bridge` declares `Publisher` field; if `nil` (Plan 11 wires real NATS), publishing is a no-op + debug log. Subagents must NOT block on Plan 11 being done.
10. **No `prometheus.MustRegister` in `init()` for shared registry.** Use a private `*prometheus.Registry` per package and let `cmd/telephony-bridge/main.go` collect them. Otherwise tests that import the package twice panic at init.
11. **Goroutine leaks: `goleak.VerifyTestMain` in every package that spawns goroutines** (Plan 04 / 06 lesson). Especially: `esl/` (readLoop), `pool/` (per-node runners), `router/` (refreshLoop), `nats_bridge/` (subscription handlers).
12. **`time.After` ban** (`make grep-time-after`). Use `time.NewTimer` + `Reset` or `time.NewTicker`.
13. **`math/rand` v1 ban**. Use `math/rand/v2` for non-security (jitter, weighted choice) or `crypto/rand` for security (none in Plan 09).
14. **Idempotency Redis keys** must be NAMESPACED: the plan draft says `op:idempotency:{command_id}` — keep that exact prefix to align with the dialer's expectation in Plan 10.

---

## Path corrections (the plan draft is wrong about these)

| Plan draft says | Actual project path | Notes |
|---|---|---|
| `github.com/sociopulse/social-pulse/...` | `github.com/sociopulse/platform/...` | every import |
| `internal/observability` | `pkg/observability` | logger/tracer/meter live in pkg |
| `caarlos0/env/v11` | `pkg/config` (Viper) | already wired with `TelephonyConfig` |
| `slog` | `go.uber.org/zap` | match repo logging convention |
| `prometheus.MustRegister` in init | per-package private registry | tests fail otherwise |
| `pgxpool.New` direct | `pkg/postgres.Open` + `BypassRLS` | depguard enforces |
| Plan top says "Plan #00 Foundation" / Go 1.23+ | Plan 00 + 00a + 00b shipped; Go 1.26.3 (CI) / 1.26.1 (source) | adjust |
| `TestRedisAddr(t)` (undefined helper) | `miniredis.RunT(t).Addr()` | use miniredis directly |

---

## Canonical specs (must-read)

### ESL protocol
- [**FreeSWITCH Event Socket Library**](https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Modules/mod_event_socket_1048924/) — официальная документация ESL inbound + outbound. Шапка: protocol = "request/response over TCP", `\n\n` terminator, `Content-Length` for binary bodies.
  Note: take the protocol shape (auth/request → auth $pass → command → reply), event subscription syntax (`event plain CHANNEL_CREATE CHANNEL_HANGUP_COMPLETE …`), and the `bgapi` vs `api` distinction. Skip mod_event_socket source unless debugging a specific quirk.
- [**ESL Library API reference**](https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Client-and-Developer-Interfaces/Event-Socket-Library/) — list of commands, content-types, event-list. **Authoritative for Reply-Text format** (`+OK …` / `-ERR …`).
- [**FreeSWITCH `originate` command syntax**](https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Dialplan/originate_3375460/) — origination string format `{var=val,…}sofia/gateway/<gw>/<dest> &<application>(<args>)`. Critical for `Originate` impl.
  Gotcha: `bgapi originate` returns immediately with `+OK <Job-UUID>`; the actual call UUID arrives later via `BACKGROUND_JOB` event. Plan 09 skips that complication by assuming the originate `+OK <call-uuid>` shortcut works (some FS builds return call UUID directly, others return Job-UUID). **Verify on real FS in integration test before relying on it.**
- [**`uuid_record` / mod_record / mod_audio_fork**](https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Dialplan/uuid_record_3375473/) — recording API. We use `uuid_record <uuid> start <path>` (mod_dptools).
- [**Event types catalog**](https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Modules/mod_event_socket_1048924/8-event-types-and-fields/) — every CHANNEL_* and CUSTOM event header set. Cite this when decoding events.

### NATS (already in COMMON.md)
- [`docs.nats.io`](https://docs.nats.io/) — JetStream, subjects, account isolation.
- Project subject convention: `tenant.<t>.telephony.cmd.<verb>` / `tenant.<t>.telephony.event.<entity>.<verb>`. The bridge subscribes wildcard `tenant.*.telephony.cmd.>` and publishes per-tenant.

### Redis Lua scripts
- [**EVAL / Lua scripts**](https://redis.io/docs/latest/develop/interact/programmability/lua-api/) — atomic counter increment-with-cap. Plan 09's backpressure script:
  ```lua
  local v = tonumber(redis.call("GET", KEYS[1]) or "0")
  if v >= tonumber(ARGV[1]) then return 0 end
  redis.call("INCR", KEYS[1])
  redis.call("EXPIRE", KEYS[1], ARGV[2])
  return 1
  ```
  Gotcha: Lua scripts are atomic per-key — works correctly on Redis Cluster only if all keys hash to the same slot. Single key per call is fine.

### W3C Trace Context
- [**W3C traceparent header format**](https://www.w3.org/TR/trace-context/) — `00-<trace-id>-<span-id>-<flags>`. Bridge propagates this through `Telephony-Trace-Id` event header for cross-service correlation.

---

## Reference implementations

### Hand-rolled ESL clients (Go)
- [**percipia/eslgo**](https://github.com/percipia/eslgo) (v3.x) — actively maintained Go ESL client, supports inbound + outbound. **Pros**: handles auth/reconnect; **Cons**: opinionated API, doesn't expose raw frame headers easily; not battle-tested at our scale.
  Files of interest: [`inbound.go`](https://github.com/percipia/eslgo/blob/main/inbound.go), [`event.go`](https://github.com/percipia/eslgo/blob/main/event.go), [`command.go`](https://github.com/percipia/eslgo/blob/main/command.go).
  **Decision**: write our own thin wrapper on `net.Conn` (per plan draft Task 2) — gives us complete control over Frame headers, body, and timeout semantics. eslgo is the fallback if hand-rolled becomes unwieldy.
- [**0x19/goesl**](https://github.com/0x19/goesl) — older lib, less active. Skip.
- [**fiorix/go-eventsocket**](https://github.com/fiorix/go-eventsocket) — venerable, simple, but unmaintained. Useful as a reference for the parser.

### FreeSWITCH integration tests
- [**signalwire/freeswitch Docker image**](https://hub.docker.com/r/signalwire/freeswitch) — official, version 1.10.x. Use `signalwire/freeswitch:1.10.10` in `tests/integration/telephony/docker-compose.yml`.
  Files of interest: minimal config to enable inbound ESL on port 8021 with password `ClueCon` (default). Add a single `mod_sofia` profile listening on UDP 5060 for outbound originate testing.
- [**testcontainers-go FreeSWITCH module**](https://golang.testcontainers.org/) — no first-party module exists; spin up via `testcontainers.GenericContainer` with the `signalwire/freeswitch` image.

### Russian SIP-trunk providers (for Plan 10 coordination — not Plan 09 itself)
- [**Voximplant** (RU)](https://voximplant.ru/) — programmable telephony API. Not a SIP-trunk per se, but they expose SIP termination.
- [**Mango Office**](https://www.mango-office.ru/) — popular RU SIP-trunk for outbound.
- [**MTT (МТТ)**](https://www.mtt.ru/) — high-volume outbound trunk; common in Russian call-centers.
  Note: Plan 09 routing is provider-agnostic. Plan 10 will encode actual trunk configs.

---

## Production lessons (blog posts, talks)

- [**ClueCon 2024 talks (YouTube)**](https://www.youtube.com/c/ClueCon) — annual FreeSWITCH conference. Anthony Minessale's keynotes explain ESL idioms first-hand. Search "ESL inbound" / "originate timeout" for relevant talks.
- [**"Building telephony at scale: lessons from VICIDial / FreeSWITCH"** (Habr)](https://habr.com/ru/) — search for `ESL FreeSWITCH опыт`. The few articles that exist (VoxImplant blog, FreeSWITCH Russia channel) hit the bridge-level concerns: timeouts, reconnect semantics, channel leak detection.
- **VICIDial source as reference** (last open-source large-scale predictive dialer): Perl + AGI, but the operational lessons (channel-leak reconciliation; per-trunk caps; FS-node-down detection) translate.
- **FreeSWITCH Cookbook** (Anthony Minessale, Michael Collins, Darren Schreiber) — Chapter on Event Socket has the canonical ESL exchange.
- **Outbound dialer scaling lesson** (Bandwidth/Twilio engineering blogs): always treat the FS counter as eventually-consistent — drift WILL happen, reconciler is mandatory. Plan 09 Task 6 directly addresses this.

---

## Russian-specific (152-ФЗ, RU SIP)

- **152-ФЗ stance** — see `COMMON.md`. The bridge handles call-control only; PII (phone numbers in originate target) is logged in audit but NOT logged in zap (zap loggers go through `pkg/observability.NewRedactingEncoder` which masks `+7XXXXXXXXXX` patterns).
- **Russian phone number normalization** — already done by `internal/crm/service/phone.go` (libphonenumber). Plan 09 receives phones already-normalized via NATS commands; trust the upstream.
- **IVR consent prompt** — required for recording (152-ФЗ + 38-ФЗ). The bridge plays `mod_dptools.playback` BEFORE bridging audio. Prompt URL is in `OriginateCommand.PromptURL` → translates to a `bgapi uuid_broadcast <uuid> <prompt-url>` call before the bridge. Plan 12 owns the prompt-asset pipeline; Plan 09 just supports the URL.

---

## Gotchas (do-not-do list)

1. **Don't use `time.After` in a `select` inside a loop.** Each iteration leaks a timer. Use `time.NewTimer` + `Reset`. `make grep-time-after` enforces.
2. **Don't share a `*esl.Client` across goroutines without serialization.** The plan's `Client.sendCommand` uses `sync.Mutex` for write — keep it. The `replies` channel pattern relies on one outstanding command at a time per client; when extending to bgapi-with-Job-UUID flow (deferred), this changes.
3. **Don't `panic` in goroutine bodies.** `connectAndServe` and `runNode` return errors; the parent loop logs and retries with backoff.
4. **Don't hardcode `time.Sleep` in tests.** Use `require.Eventually` or `miniredis.FastForward` (for Redis TTL).
5. **Don't trust originate's `+OK <uuid>` blindly.** On some FS builds, `bgapi originate` returns `+OK <Job-UUID>` not the call UUID; the call UUID arrives via `BACKGROUND_JOB` event. **Test on real FS during integration tests** before shipping. If we hit this: switch to event-correlation-by-`Job-UUID` flow (defer to v0.0.11 if needed).
6. **Don't INCR backpressure in nats_bridge before originate is sent.** The plan correctly puts `bp.TryAcquire` inside `Router.Select` BEFORE building the call URL. Keep that order: acquire → originate → on-hangup release.
7. **Don't subscribe to `ALL` events.** `event plain ALL` floods the bridge with ~50 event types per call. Subscribe only to `CHANNEL_CREATE CHANNEL_ANSWER CHANNEL_HANGUP_COMPLETE CHANNEL_BRIDGE CHANNEL_UNBRIDGE DTMF RECORD_STOP CUSTOM sofia::register CUSTOM mod_callcenter::*`.
8. **Don't store ESL passwords in source.** Read from `cfg.Telephony.Bridge.FSNodes[i].ESLCert/ESLKey` (mTLS) or a future `ESLPassword` field; never literal in test code (gitleaks blocks it). Use `t.Setenv` in tests.
9. **Don't rely on `eslgo` (or hand-rolled) to recover deleted-while-running events.** FreeSWITCH does not buffer events for a disconnected ESL client. After reconnect, re-subscribe but accept that the gap is lost. CDR (Plan 12) is the source of truth for billable calls; bridge events are best-effort for realtime UI.
10. **Don't `prometheus.MustRegister` at package init().** Two test imports = panic. Use a per-package `*prometheus.Registry` and have `cmd/telephony-bridge/main.go` Gather() and Combine() them, OR pass a registry into the constructor.
11. **Don't use a single global `RoundRobin{}` Strategy across packages.** Its `atomic.Uint64` counter is per-instance; if the Router selects different operators with different RR strategies, they should be independent.
12. **Don't store secrets (`ClueCon` ESL password) in helm values.yaml unencrypted.** Use Helm's `secret` template + sealed-secrets or external secrets operator. For dev: load from env via Snapshot.

---

## Open questions (need to resolve during impl)

1. **bgapi originate Job-UUID issue**: do we use `bgapi` (returns Job-UUID, real call UUID arrives in BACKGROUND_JOB event) or `api originate` (synchronous, blocks ESL connection)? Plan draft says `bgapi`. **Decision needed**: bench in integration test. If `+OK <call-uuid>` is returned synchronously by bgapi (some builds do this), we keep it; otherwise we switch to event-correlation flow. **Default**: ship bgapi with `+OK` parser; add Job-UUID correlation as a hardening Phase 2.
2. **Concurrent originates per connection**: do we serialize all sendCommand calls with the same Client (current plan)? Or open multiple connections per FS node for parallelism? **Default**: serialize per Client; one Client per FS node should be enough (60 concurrent calls / connection cycle is fine over local socket). Re-evaluate if benchmarks show contention.
3. **FS directory XML endpoint mTLS**: which CA validates the FS-side cert presented to `cmd/api`? Do we ship a project-internal CA (per-tenant) or use Yandex Cloud's? **Default**: project-internal CA (Plan 01 owns), pinned in `internal/telephony/api/mtls.go`.
4. **per-call SIP credentials TTL**: 4h per plan. Is that enough? An IVR survey can run 30+ min; 4h covers operator break + retry. **Default**: keep 4h, document in module README.
5. **Redis cluster vs single-node**: bridge is a singleton sidecar per AZ; Redis is shared (cluster). Keep all Plan 09 Redis ops on a single key per call (idempotency, active_channels, credentials) so cluster-slot affinity works. **Verified**: each operation uses one key.

---

## Carry-over from earlier plans (state we depend on)

### Already built (from Plans 02–07)
- `pkg/config` — TelephonyConfig section already present. Hot-reload works.
- `pkg/observability` — zap logger, OTel tracer/meter, gin middleware. Use as-is.
- `pkg/postgres` — `Open(ctx, dsn) (*Pool, error)` + `BypassRLS(ctx, fn)` for tenant-agnostic queries.
- `pkg/eventbus` — interfaces only (Plan 11 wires real NATS). Bridge uses `nats.go` directly because it's the publisher *of* events; Plan 11 will wire a Publisher consumer for downstream.
- `internal/modules/Locator` — for inter-module lookup (audit logger, etc.).
- `internal/telephony/api/` — full contract surface (DTOs, interfaces, enums). NEVER re-declare. NEVER modify in this plan.

### Patterns to copy
- **Module composition root** — see `internal/auth/module.go` and `internal/crm/module.go`. Same shape:
  ```go
  func (Module) Register(d modules.Deps) error {
      logger := d.Logger.With(zap.String("module", "telephony"))
      // build stores → services → mounts → register in d.Locator
      return nil
  }
  ```
- **Audit logger noop fallback** — `internal/auth/module.go` does:
  ```go
  audit, _ := d.Locator.Lookup[audit.Logger]("audit.Logger")
  if audit == nil {
      logger.Warn("audit logger missing — using noop")
      audit = noopAudit{}
  }
  ```
  Copy this pattern in telephony.
- **NATS publisher slot** — `internal/crm/service/project_service.go` declares `Publisher eventbus.Publisher` (interface, optional). If `nil`, publish is no-op. Same pattern in `nats_bridge`.

### What Plan 09 will give downstream
- `internal/telephony/{esl,pool,router,nats_bridge,credentials}` real implementations.
- `cmd/telephony-bridge/main.go` real entrypoint.
- `cmd/api` gets a new HTTP route `/internal/freeswitch/directory` (mTLS-protected) that reads `op:credentials:{operator_id}:{call_id}` from Redis.
- `internal/telephony/api.CommandPublisher` — first real implementation (NATS-based).
- `internal/telephony/api.Router` — first real implementation.
- `internal/telephony/api.LineCapacityTracker` — first real implementation.
- Migration: NONE for Plan 09 (telephony_trunks table TBD — Plan 10 may add, or `cfg.Telephony.Trunks` may stay config-only). **Decision deferred to integration phase.**

### Carry-overs from this plan
TBD — fill at close-out.

---

## Lessons learned from Plan 09 implementation

- **Plan-draft uses wrong module path / wrong logger / wrong observability path / wrong config loader.** Subagents must read this references doc FIRST and substitute `github.com/sociopulse/platform`, zap (not slog), `pkg/observability` (not `internal/observability`), `pkg/config` (not caarlos0/env). Captured in §"Pragmatic decisions locked" — drove every Task.

- **`commandVerb` collapses bgapi/api commands to one metric label.** Naive `commandVerb(line)` returns the first whitespace token, so `bgapi originate` and `bgapi uuid_kill` both label as `bgapi`. Fix: unwrap when first token is `bgapi`/`api` and use the second. Caught by code-quality reviewer Task 3 (`f1eb679`). Future subagents wiring metrics should bake the unwrap into the labelling helper, not the call-site.

- **`sendCommand` reply-stealing race.** Holding `writeMu` only for write+flush — then unlocking before reading the shared `replies` chan — silently lets two concurrent callers swap each other's replies. Fix: extend the mutex to wrap the entire send+wait window AND drain stale replies on ctx-cancel. Caught by Task 2 review (`ebc9748`). The "one in-flight command per Client" invariant has to be enforced end-to-end, not just on the write half.

- **`Close()` must block on `readLoopDone` in BOTH CAS branches.** When dispatch's text/disconnect-notice path flips `closed=true` first, a later `Close()` hits the CAS-fail branch — and the naive impl returns nil immediately, leaving readLoop running. Fix: block on `<-readLoopDone` regardless of CAS outcome. Caught by Task 2 review.

- **`MapEvent` must clone the headers map.** Returning `Headers: ev.headers` aliases the parser's internal map; a downstream consumer can mutate it and corrupt subsequent `MapEvent` calls on the same Event. Fix: `Headers: maps.Clone(ev.headers)`. Caught by Task 2 review.

- **`time.After` is banned even in tests' for-loops.** `make grep-time-after` excludes `*_test.go` for one-shot select-arms only. A polling loop that calls `time.After` per iteration leaks timers under `-race -count=5`. Use `time.NewTimer` + `Reset` or `require.Eventually`. Project-wide gotcha rediscovered repeatedly.

- **Modernize linter is aggressive with Go 1.26.** Subagents kept tripping on `for i := 0; i < N; i++` (use `for range N`), `for k, v := range src { dst[k]=v }` (use `maps.Copy`), `wg.Add(1); go func() { defer wg.Done(); … }()` (use `wg.Go`), and `fmt.Sprintf("%d", n)` (use `strconv.Itoa`). Fix-up commits hit every Task. Future agents should pre-modernize before submission.

- **`prometheus.MustRegister` in `init()` panics under double-import in tests.** Always inject a `prometheus.Registerer` via the constructor (`RegisterMetrics(reg)` pattern). Panic clearly on nil reg with remediation message ("pass prometheus.NewRegistry() in tests"). Established in Task 2, repeated in Tasks 4, 5, 6.

- **`math/rand` v1 vs v2 depguard rule.** Default project rule (`pkg: math/rand`) is a prefix match that ALSO blocks `math/rand/v2`. Pin with `pkg: "math/rand$"` exact-match suffix so v2 is allowed. Done in Task 2 (`fbeeb4d`).

- **Sentinel errors must be aliased across api↔internal boundaries.** Plan 10 dialer composes against `api.ErrNoTrunkAvailable` via `errors.Is`. If `internal/telephony/router` declares its own `errors.New("router: no available trunk")`, the composition silently misses. Fix: `var ErrNoTrunkAvailable = api.ErrNoTrunkAvailable` aliasing. Caught by Task 5 review (`ba12b00`).

- **Config-only trunk catalog vs Postgres `telephony_trunks` table.** The plan-draft proposed a Postgres table with a 30s refresh loop. Defer to Plan 13/14: cfg.Telephony.Trunks (Viper Snapshot) is sufficient for v1, no migration debt. Document the deferral inline. Decision in Task 5 (`c4af4f8`).

- **`api show channels count` body shape varies.** Some FS builds emit `5 total.\n`; some emit just `\n` when no channels exist. ChannelsCount: trim, then `strings.Fields()[0]`, return 0 on empty body, wrap on non-numeric. Test all three paths. Task 6.

- **`ESLPool` health-probe needs a 3s bounded ctx, not the parent.** Parent ctx may have no deadline; one stalled FS can stall the entire reconciler sweep. Per-node `WithTimeout` with `defer cancel()` (panic-safe) inside `sweepNode`. Caught by Task 6 review (`3b65932`).

- **`bgapi originate` Job-UUID vs call-UUID is FS-build-dependent.** Some builds return the call UUID directly in `+OK <uuid>`; others return Job-UUID and emit the call UUID later via BACKGROUND_JOB. Plan 09 returns the +OK token verbatim and marks `// FIXME(plan-09)` in `commands.go`. Task 4 (pool) or Task 6 (reconciler) is the natural seam to wire BACKGROUND_JOB correlation when integration tests against real FS surface the issue.

- **`internal/telephony/api/` shape was already non-trivial.** The plan-draft introduced fresh DTO names (`OriginateRequest`, `Trunk`, `SelectionResult` with `CallURL`/`GatewayName` fields). Existing api had `OriginateCommand`, `Trunk` (different shape via api.Router enum), `SelectionResult{FSNode, TrunkID, Reason}`. Don't modify api/ — convert at the boundary. CallURL construction lives in the dialer/originate publisher, not the Router.

- **`gopls` cache lag continues to surface phantom diagnostics.** Subagents would report tests passing while gopls flagged "undefined: X" after each large commit — and even invented files (`/tmp/sortcheck.go`, `simple_test2.go`) that didn't exist on disk. Always reality-check via direct `go build && go test -race`; trust the harness, not the editor squiggles.

- **`nats_bridge` left as Task 1 stub.** Plan 09 ships ESL + pool + router + reconciler — but the actual NATS subscribe/publish loop awaits Plan 11 (which owns NATS subjects + JetStream wiring). The `cmd/telephony-bridge` binary boots and runs the bridge subsystems; commands published on `telephony.cmd.*` are not yet consumed. Document the gap explicitly in PROJECT_STATUS so Plan 10 (dialer) doesn't assume a working bridge.

- **Per-call SIP credentials + `/internal/freeswitch/directory` endpoint deferred.** Plan 09 drafts these as Task 5 deliverables — but they belong with NATS bridge + mTLS-protected HTTP route in `cmd/api`. Both shipping with Plan 11 is cleaner than a half-wired Plan 09. Documented as a carry-over.

- **Helm chart + Prometheus alert rules live in `sociopulse-infra` repo.** Plan 09 exports the metric (`telephony_router_active_channels_drift{node}`); the alert rule (`> 10 for 5m`) is an ops concern, NOT a sociopulse-platform deliverable. Don't try to land a `helm/` directory mid-Plan-09.
