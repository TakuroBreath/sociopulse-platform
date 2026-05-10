# 10. End-to-End Testing Gaps — and how we plan to close them

> **Status:** Identified 2026-05-10 during a Plan 13.1 retrospective
> conversation. **This is not a process critique** — the per-task TDD +
> 2-stage review pipeline is doing its job. This document captures a
> level above per-module testing that the project has consciously
> deferred and now needs to schedule.
>
> **Owner:** platform team.
> **Priority:** medium-high — this is the production-confidence ceiling.
> **Closure plan:** see § "Closure plan" below.

## Why this document exists

The framing matters: **the goal is not coverage percentage, it is
confidence that the system works as a whole**. Per-module tests catch
per-module bugs by design; system-level surprises live in the cracks
between modules — locator wiring, NATS subject naming, JWT claims
shape, middleware order, migration grants, etc.

A retrospective inventory after Plan 13.1 close-out (16 commits across
all v0.x.x tags shipped to date) shows the testing surface is
**deeply layered but not stitched end-to-end**.

## What we DO test today

| Layer | How | What it catches |
|---|---|---|
| **Unit** (~2 000 funcs) | stdlib `testing` + testify + `t.Parallel()` + mocks via `mockery` | Logic bugs, branch coverage, error-handling discipline |
| **Integration with real deps** (246 funcs in 42 files) | `//go:build integration` + `testcontainers-go` (PG, Redis, NATS, MinIO/S3, ClickHouse) | SQL syntax, RLS policies, index usage, NATS durable consumer config, gRPC interceptor chain, Redis Lua scripts, Postgres advisory locks |
| **HTTP handler unit** (~280 funcs across `transport/http/*_test.go`) | `gin.SetMode(TestMode)` + `httptest.NewRecorder` + `ServeHTTP` against fake services | Request parsing, middleware chain, error→HTTP envelope mapping, response shape, RBAC role gates, body-size limits |
| **Single-module E2E** (1 scenario) | `internal/realtime/integration_test.go` — embedded NATS JetStream + real Hub + `httptest.NewServer` + real `coder/websocket.Dial` | Realtime WS pipeline: NATS publish → events.NATSSubscriber → Hub.Broadcast → WS client receive |
| **`cmd/api` boot smoke** (1 test) | `cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly` | Composition root boots, listeners bind on free ports, graceful shutdown leaks zero goroutines |
| **Goroutine leak detection** (mandatory) | `go.uber.org/goleak.VerifyTestMain` in every package that spawns production goroutines | Forgotten ctx cancel, lingering tickers, channel-blocked writers |

## What we DO NOT test today

### A. Cross-module E2E through the real `cmd/api`

A scenario that walks through multiple modules over the public HTTP /
WS surface against a real backing stack. There is **none** today.

A representative gap: there is no test that does

1. POST `/api/auth/login` → JWT
2. POST `/api/projects` (with that JWT) → 201 + `project_id`
3. POST `/api/respondents/import` (multipart CSV with that JWT) → 202 + `job_id`
4. polling GET `/api/respondents/imports/:job_id` → status=completed
5. GET `/api/projects/:id/progress` → asserts respondent count

…against a real `cmd/api` listening on a `httptest`-style ephemeral
port, with real Postgres + Redis + NATS containers underneath, and a
real outbox relay running.

### B. Manual API smoke (curl / Postman / Bruno)

There is **no REST collection** in the repository
(`*.postman_collection.json`, `*.bru`, etc.) that a developer or QA
can open and walk through the public surface in five minutes. The
`docs/api/recording/` folder ships an OpenAPI stub for the recording
module only.

### C. `tests/smoke/` directory

Mentioned in `docs/architecture/09-agent-workflow-improvements.md`
§ "Improvement #5" as a roadmap item. The directory **does not yet
exist**. `tests/` currently contains:

```
tests/
├── README.md     # "e2e/ — Playwright (filled in Plan 15+);
│                  load/ — k6 scripts (filled later)"
├── e2e/          # empty
└── load/         # empty
```

### D. Frontend E2E (Playwright)

Out of scope for this repository — lives in `sociopulse-web`. Spec
§17 lists eight admin walkthroughs that Playwright will cover once
the frontend exists. Plans 15-19.

### E. Real FreeSWITCH integration

ESL parser is tested against hand-rolled fake-fixtures.
`internal/telephony/pool` and `internal/telephony/router` use
`DialFunc` test seams. `Reconciler` uses a fake clock. **A real
FreeSWITCH instance has not been booted in any test.** Plan 09
description acknowledges this:
> "Subsystems wired but not yet end-to-end against real FreeSWITCH
> (integration tests deferred)"

Closure depends on Plan 08 (FS-cluster infra in `sociopulse-infra`).

### F. Real Yandex Cloud (KMS / Object Storage)

Production adapters are stubbed. `LocalKMSClient` and
`LocalBucketProvisioner` work in-memory. `-tags=yandex_kms` and
`-tags=yandex_s3` build-tag-gated implementations are scaffolded but
**do not compile or run** because the SDK code has not been written
yet. Plan 01.

### G. Chaos / load

`tests/load/` and `tests/chaos/` directories are empty. k6 scripts
and Chaos Mesh experiments are listed in spec §17 as
production-readiness items, deferred until pre-launch.

## Why this matters — concrete failure scenarios

The classes of bug below are **invisible** to the testing layers we
have today. Each is a real category, with at least one near-miss
already encountered during plan execution.

### 1. Locator wiring mismatch

Plan 11.4 wired `CallResolver` under locator key
`realtime.CallResolver`. If `cmd/api/main.go` registered it under a
typo'd key, every per-module integration test would still pass — but
in production the WS handler's `checkCrossTenant` would fall through
to the `emptyCallResolver{}` fail-closed fallback, returning
`ErrCrossTenantSubscribe` for *every* subscribe with `filter.CallID`.

Per-module testing cannot catch this; only a cross-module smoke
that actually subscribes to a `call.events` topic against a real
`cmd/api` will.

### 2. NATS subject pattern mismatch

Auth publishes on the concrete subject `tenant.<t>.auth.user.deleted`.
Realtime subscribes via the wildcard pattern
`tenant.*.auth.user.deleted`. If a future PR renames one without
the other, NATS happily silently drops the message — there is no
parse error, no test failure, and no log warning.

### 3. JWT claims schema drift

Auth issuer writes `roles []string` (multi-role since Plan 05). If
a middleware unit is later written against `role string` (single)
because a developer copied an old example, both will pass their
unit tests with stub fixtures and only fail when a real token from
a real `/api/auth/login` hits the real middleware.

### 4. Migration RLS / grant drift

Plan 12.1 originally claimed `tenancy_admin` had grants on
`call_recordings`. It did not. Plan 12.4 caught this only because
the retention worker actually exercised the BypassRLS path. A
smoke that boots `cmd/worker` against a freshly-migrated DB would
have caught it on day one of Plan 12.1.

### 5. HTTP middleware order

The gateway pipeline is roughly: request-ID → recovery → zap →
JWT extraction → RLS context bind → idempotency → rate limit. If
a refactor accidentally puts JWT extraction *after* RBAC, every
authenticated request reads the empty JWT and either 401s or
runs as anonymous (depending on RBAC default). Per-handler tests
mock the middleware chain; the bug only surfaces against the
real pipeline.

### 6. Resolver-cache invalidation drift

Plan 11.4 wired three lifecycle subjects to three cached
resolvers. If a future plan adds a fourth lifecycle event but
forgets to register an invalidation callback, stale (`user_id →
tenant_id`) entries linger for the full 60s TTL. Per-module
tests don't span the publish-side and the consumer-side together.

## Closure plan

### Phase 1 — `tests/smoke/` (target: 2-3 days of focused work)

A new package `tests/smoke/` gated by `//go:build smoke`. Each test
boots a fresh testcontainer stack (PG + Redis + NATS + MinIO),
applies all migrations, starts `cmd/api` as a goroutine on free
ports, and exercises the public surface via real `http.Client`
calls plus `coder/websocket.Dial` for WS.

To make this possible, **`cmd/api/main.go` will need a small
refactor** so the run loop is callable as
`Run(ctx context.Context, configPath string) error` from the smoke
suite — independent of `os.Args` / `signal.NotifyContext`. The
existing `cmd/api/main_test.go` already partially does this; we
extend it.

Initial scenario set (priority order — implementable today against
the modules already shipped):

1. **`TestSmoke_AuthFullFlow`** — login (with TOTP partial flow) →
   refresh → logout → refresh-after-logout=401. **Covers:** auth
   module, JWT issuer/validator, refresh-token rotation, Redis
   blacklist.
2. **`TestSmoke_AdminCreatesProjectAndImportsRespondents`** — admin
   login → POST `/api/projects` → POST `/api/respondents/import`
   (multipart CSV) → polling GET `/api/respondents/imports/:job_id`
   → SELECT count assertion. **Covers:** auth, crm, asynq job
   queue, KMS encrypt of phone numbers, NATS progress events.
3. **`TestSmoke_OperatorReadyAndStateBroadcast`** — operator login
   → connect WS → POST `/api/dialer/sessions/me/ready` →
   assert WS event delivered. **Covers:** dialer FSM, realtime
   Hub, NATS publish→subscribe→broadcast pipeline.
4. **`TestSmoke_SurveyCreatePreviewActivate`** — admin creates
   survey draft → preview → save v1.0 → activate → assert
   `survey_versions_active_one` partial-unique constraint
   honoured. **Covers:** surveys validator + DSL + version store
   + Postgres advisory lock.
5. **`TestSmoke_RecordingSearchAndStream`** — fixture row +
   pre-encrypted `.opus.enc` in MinIO → GET
   `/api/recordings/search` cursor → GET
   `/api/recordings/:id/stream` → assert ciphertext sha256
   matches and Range header is rejected. **Covers:** recording
   metadata store + KMS unwrap + S3 streaming + integrity verify.
6. **`TestSmoke_RbacEnforcement`** — operator JWT POSTs
   `/api/projects` → 403; admin JWT same → 201. **Covers:** RBAC
   matrix end-to-end.
7. **`TestSmoke_TenantIsolation`** — tenant A creates a project,
   tenant B's JWT GETs `/api/projects/:id` → 404 (RLS swallows).
   **Covers:** RLS policy + middleware tenant binding.
8. **`TestSmoke_RespondentSoftDelete152FZ`** — POST `/api/respondents`
   → DELETE → wait 30 days (clock injection) → run `cmd/worker`'s
   PurgeWorker tick → row physically gone. **Covers:** crm soft-
   delete + asynq scheduled worker + 152-ФЗ deletion right.

**CI integration:** a separate `smoke` job in `.github/workflows/ci.yml`,
parallel to existing `Test`:

- Runs on every push to `main` (parallel with deploy).
- Cron 1×/hour against `main` (catch external regressions: image
  rebuilds, Yandex SDK pinning).
- Tag push `v*` — **mandatory gate** before production rollout.

Phase 1 closes scenarios A and C from § "What we do not test today".

### Phase 2 — REST collection (target: ~4 hours)

`docs/api/collections/sociopulse.bru` (Bruno format; alternatively a
`.postman_collection.json` / Hurl / HTTPie file). Contents:

- All public endpoints grouped by module.
- Login flow at the top, JWT captured into env automatically.
- Happy-path + at least one error case per endpoint.
- Used for: developer manual exploration, QA pre-release sweep,
  onboarding a new agent on the system.

This closes scenario B.

### Phase 3 — Frontend E2E (Playwright) — Plans 15-19 territory

When `sociopulse-web` ships, Playwright tests against an ephemeral
`kind` Kubernetes cluster cover the eight admin walkthroughs from
spec §17. Owned by the frontend repo's plan workflow; scheduled
when frontend foundation lands.

This closes scenario D.

### Phase 4 — Real FreeSWITCH + Yandex SDK — Plans 01 + 08

- Plan 01 (infra): Yandex KMS / Object Storage real adapters
  compile + integration-test under build tags. Closes F.
- Plan 08 (FS-cluster): Ansible-managed FS VMs, telephony-bridge
  end-to-end against real FS. Closes E.

### Phase 5 — Chaos / load — pre-launch

- k6 scripts in `tests/load/`: 500 concurrent operators with state
  changes every 5 s, asserting p95 event latency < 500 ms (NFR-1).
- SIPp scenarios for telephony: 200 concurrent SIP channels through
  the FS cluster (NFR-1).
- Chaos Mesh in `tests/chaos/` on staging: kill a `cmd/api` pod,
  kill an FS-VM, network-partition Postgres. Asserts recovery
  semantics: realtime reconnect, recording lossiness ≤ ADR-0005
  budget, 503 vs 500 surface.

Closes G. Scheduled as a pre-launch milestone, separate from any
single Plan.

## Tracking

- **GitHub issue:** linked from `PROJECT_STATUS.md` standing rule
  (see "E2E testing gap" entry).
- **Standing rule** in `PROJECT_STATUS.md` — short reminder so new
  agents joining a session understand the gap.
- **This document (`10-end-to-end-testing-gaps.md`)** — long-form
  rationale + phased closure plan.
- **`09-agent-workflow-improvements.md` § Improvement #5** —
  original sketch; superseded by Phase 1 above (more detail here).

## Success criteria

The gap is **closed at Phase 1** when:

- ≥ 8 smoke scenarios live in `tests/smoke/` and run green locally.
- A `smoke` job in `.github/workflows/ci.yml` runs on every push to
  `main` and on every `v*` tag push.
- Pre-release (tag) workflow is **blocked** if smoke is red.
- At least one cross-module contract regression has been caught in
  PR by smoke that would have been missed by per-module tests
  (this is the proof that the layer is doing real work).

The gap is **fully closed (all phases)** when E and F deliver real-
component coverage and frontend Playwright runs against a live
cluster.

## Cross-references

- `04-testing-strategy.md` — the canonical layer-by-layer testing
  pyramid; this document extends it with the system-level layer.
- `09-agent-workflow-improvements.md` § Improvement #5 — the original
  smoke-test idea; this document is the realisation.
- `PROJECT_STATUS.md` § "Standing rules / patterns" — short pointer
  back here.
- Spec §17 — the source pyramid + load/chaos scenarios.
- ADR-0005 — recording-integrity 99.5% (relates to Phase 5 loss
  budget).
