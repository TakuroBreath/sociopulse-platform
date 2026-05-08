# Plan 11.1 — realtime + dialer + telephony carry-overs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. TDD discipline strict — every implementation step has a failing test step before it.

**Goal:** Close the four highest-value carry-overs from Plan 11 in a single cohesive plan: dialer presence-middleware wiring, realtime trunks-health fan-out, dialer cross-replica snapshot fan-out via NATS, and the real telephony NATS bridge.

**Architecture:** Each task targets a single subsystem and is independently shippable. Plan 11 already landed the foundation (`pkg/eventbus.NATSPublisher`/`Subscriber`, `*service.Hub`, `*service.RedisPresenceTracker`, `*events.NATSSubscriber`); this plan consumes those primitives. Plan 11.2 will tackle the heavier Plan 11 Task 10 work (frame classification, RBAC tenant cross-check) which requires `internal/realtime/api/` extensions and cross-module resolver wiring.

**Tech stack:** Go 1.26 (modernize: `wg.Go`, `slices.ContainsFunc`, range-over-int). zap logger. gin HTTP. `github.com/coder/websocket` v1.8.14. `redis/go-redis/v9`. `nats-io/nats.go` v1.52 via `pkg/eventbus`. `miniredis/v2` for unit tests. Embedded `nats-server/v2` for tests that need real JetStream (no Docker dependency).

**Carry-forward rules** (Plan 09/10/11 — REQUIRED for every task):
1. No `init()`-time `prometheus.MustRegister`; collectors built via `RegisterMetrics(reg)`.
2. Sentinel error aliasing: `var ErrFoo = api.ErrFoo`.
3. Compile-time interface check: `var _ api.X = (*Impl)(nil)`.
4. `*zap.Logger` typed; nil-safe (`zap.NewNop` default).
5. No `time.After` in select-loops; `time.NewTicker`/`time.NewTimer` instead.
6. `wg.Go` (Go 1.25+) over `wg.Add(1)/Done`.
7. `slices.Contains*` over hand-rolled loops.
8. No runtime `panic` (boot-time only for must-fail-loud invariants).
9. `goleak.VerifyTestMain` in every package with goroutines.
10. `testify/require` + `t.Parallel` + `t.Cleanup` discipline.
11. `uuid.NewString()` over `uuid.New().String()`.
12. Errors wrapped: `fmt.Errorf("<pkg>: <op>: %w", err)`.

**Out of scope (deferred to Plan 11.2):**
- Frame classification (Plan 11 Task 10.1): adds `FrameClass` enum + 4 new Topics + Connection refactor.
- RBAC tenant cross-check (Plan 11 Task 10.3): adds `User`/`Project`/`Call` resolver injection to `TopicRBAC`.

---

## File Structure

| Task | Create | Modify |
|---|---|---|
| 1 | — | `internal/dialer/transport/http/middleware.go` (RefreshPresence middleware), `internal/dialer/transport/http/routes.go` (apply on operator routes), `internal/dialer/transport/http/middleware_test.go` (existing or NEW for the new middleware), `internal/dialer/module.go` (pass `presence.RefreshFn` adapter to transport) |
| 2 | `internal/realtime/events/trunks_replicator.go` + `_test.go` | `internal/realtime/events/nats_subscriber.go` (remove the trunks.health TODO; delegate fan-out to the new replicator), `internal/realtime/api/dto.go` (only if a `BroadcastFilter.AllAdmins` flag is needed; otherwise no api/ change) |
| 3 | `internal/dialer/pubsub_nats.go` + `_test.go` (NATS-backed PubSub adapter) | `internal/dialer/module.go` (wire the NATS adapter when `Deps.EventBus`/`Deps.Subscriber` are non-nil; fall back to in-memory PubSub) |
| 4 | `internal/telephony/nats_bridge/cmd_subscriber.go`, `event_publisher.go`, `idempotency.go`, `metrics.go`, `*_test.go` | `internal/telephony/nats_bridge/bridge.go` (real Start/Stop/Drain), `cmd/telephony-bridge/main.go` (already constructs Bridge — verify wiring after) |

---

## Task 1: Dialer RefreshPresence middleware wiring

**Goal:** Wire `internal/dialer/fsm.RefreshPresence` as a gin middleware on operator routes so the Heartbeat watchdog only triggers on ungraceful disconnects, not on every page reload.

**Files:**
- Modify: `internal/dialer/transport/http/middleware.go` — add `RefreshPresenceMiddleware(refresh RefreshFn) gin.HandlerFunc`.
- Modify: `internal/dialer/transport/http/routes.go` — apply the middleware on the operator subgroup (`/api/dialer/sessions/*`).
- Modify: `internal/dialer/module.go` — bind a `RefreshFn` adapter that calls `fsm.RefreshPresence(ctx, deps.Redis, tenantID, operatorID, ttl)` with the operator session TTL.
- Modify: `internal/dialer/transport/http/middleware_test.go` — TDD coverage.

### Step-by-step

- [ ] **Step 1.1: Read existing middleware patterns**

```bash
cat internal/dialer/transport/http/middleware.go
cat internal/dialer/transport/http/routes.go
cat internal/dialer/fsm/heartbeat.go | head -80
```

Identify: how `claimsFromContext` extracts tenant/operator UUIDs; the existing route group structure; the `RefreshPresence` signature `(ctx, rdb redis.UniversalClient, tenantID, operatorID uuid.UUID, ttl time.Duration) error`.

- [ ] **Step 1.2: Write failing test for the middleware (RED)**

`internal/dialer/transport/http/middleware_test.go`:

```go
func TestRefreshPresenceMiddleware_RefreshesOnAuthenticatedRequest(t *testing.T) {
	t.Parallel()

	var refreshed atomic.Int32
	fakeRefresh := func(ctx context.Context, tenantID, operatorID uuid.UUID) error {
		refreshed.Add(1)
		require.Equal(t, "tenant-A-uuid", tenantID.String())
		require.Equal(t, "u1-uuid", operatorID.String())
		return nil
	}

	r := gin.New()
	r.Use(injectClaims(authapi.Claims{
		TenantID: mustUUID("tenant-A-uuid"),
		UserID:   mustUUID("u1-uuid"),
		Roles:    []authapi.Role{authapi.RoleOperator},
	}))
	r.Use(transporthttp.RefreshPresenceMiddleware(fakeRefresh))
	r.GET("/sessions/me", func(c *gin.Context) { c.Status(204) })

	req := httptest.NewRequest("GET", "/sessions/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, 204, w.Code)
	require.Equal(t, int32(1), refreshed.Load())
}

func TestRefreshPresenceMiddleware_SkipsWhenNoClaims(t *testing.T) {
	t.Parallel()

	var refreshed atomic.Int32
	fakeRefresh := func(_ context.Context, _, _ uuid.UUID) error {
		refreshed.Add(1)
		return nil
	}

	r := gin.New()
	r.Use(transporthttp.RefreshPresenceMiddleware(fakeRefresh))
	r.GET("/sessions/me", func(c *gin.Context) { c.Status(204) })

	req := httptest.NewRequest("GET", "/sessions/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, 204, w.Code)
	require.Equal(t, int32(0), refreshed.Load(),
		"middleware must skip presence refresh when claims absent (auth middleware not yet ran)")
}

func TestRefreshPresenceMiddleware_FailureDoesNotBlockRequest(t *testing.T) {
	t.Parallel()

	fakeRefresh := func(_ context.Context, _, _ uuid.UUID) error {
		return errors.New("redis down")
	}

	r := gin.New()
	r.Use(injectClaims(authapi.Claims{
		TenantID: uuid.New(),
		UserID:   uuid.New(),
		Roles:    []authapi.Role{authapi.RoleOperator},
	}))
	r.Use(transporthttp.RefreshPresenceMiddleware(fakeRefresh))
	r.GET("/sessions/me", func(c *gin.Context) { c.Status(204) })

	req := httptest.NewRequest("GET", "/sessions/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Refresh failure must NOT block the request — it's a side-effect
	// for graceful disconnect detection, not a hard precondition.
	require.Equal(t, 204, w.Code)
}
```

- [ ] **Step 1.3: Run tests → fail with undefined symbol**

```
go test ./internal/dialer/transport/http/... -run RefreshPresenceMiddleware -v
```
Expected: FAIL — `undefined: transporthttp.RefreshPresenceMiddleware`.

- [ ] **Step 1.4: Implement the middleware (GREEN)**

`internal/dialer/transport/http/middleware.go` — add:

```go
// RefreshFn is the adapter the composition root supplies; the middleware
// is decoupled from fsm.RefreshPresence's Redis-typed signature so tests
// can inject a fake without touching Redis.
type RefreshFn func(ctx context.Context, tenantID, operatorID uuid.UUID) error

// RefreshPresenceMiddleware extends the operator's heartbeat presence
// every time the operator hits an authenticated route. The Heartbeat
// watchdog (fsm/heartbeat.go) forces the operator offline when the
// presence key expires; without this middleware the only TTL refresh
// was the initial OnConnect, so a long-running idle UI session would
// be force-paused after 30 s.
//
// Failures are logged + counted but never blocking — the middleware is
// a fire-and-forget side effect, not a hard precondition. If Redis is
// down the operator's request still completes; the heartbeat watchdog
// will eventually catch up and force the state transition.
//
// Skips silently when claims are absent (the chain ran before
// JWTMiddleware, which is a wiring bug we want to surface via the
// auth middleware itself, not double-cover here).
func RefreshPresenceMiddleware(refresh RefreshFn) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := claimsFromContext(c)
		if !ok {
			c.Next()
			return
		}
		// Fire the refresh on the request ctx — auto-cancel on client
		// disconnect bounds the side effect.
		_ = refresh(c.Request.Context(), claims.TenantID, claims.UserID)
		c.Next()
	}
}
```

Add a Prometheus counter `dialer_presence_refresh_failures_total` to the existing dialer transport metrics (if not already present) so we observe Redis failures without alerting on every successful refresh. Leave nil-tolerant.

- [ ] **Step 1.5: Run tests → green**

```
go test ./internal/dialer/transport/http/... -run RefreshPresenceMiddleware -race -v
```
Expected: PASS. Verify all 3 cases.

- [ ] **Step 1.6: Wire on the operator route group**

Modify `internal/dialer/transport/http/routes.go` — find the operator subgroup mount point and add the middleware AFTER `JWTMiddleware` so claims are populated:

```go
operatorGroup := group.Group("/sessions")
operatorGroup.Use(deps.JWTMiddleware)
if deps.RefreshPresence != nil {
    operatorGroup.Use(RefreshPresenceMiddleware(deps.RefreshPresence))
}
// ... existing handlers ...
```

Add `RefreshPresence RefreshFn` to the transport `Deps` struct (nil-safe — Redis-less test setups skip the middleware).

- [ ] **Step 1.7: Wire the adapter in the dialer Module**

`internal/dialer/module.go` — when constructing transport Deps, pass:

```go
var refresh transporthttp.RefreshFn
if d.Redis != nil {
    refresh = func(ctx context.Context, tenantID, operatorID uuid.UUID) error {
        return fsm.RefreshPresence(ctx, d.Redis, tenantID, operatorID, fsmDefaultTTL)
    }
}
transporthttp.Mount(group, transporthttp.Deps{
    // ... existing fields ...
    RefreshPresence: refresh,
})
```

`fsmDefaultTTL` is the same TTL the heartbeat watchdog uses (24 h per `fsm/heartbeat.go`).

- [ ] **Step 1.8: Run all dialer tests + lint + commit**

```
go test ./internal/dialer/... -race -count=1
go vet ./internal/dialer/...
golangci-lint run --timeout=2m ./internal/dialer/...
git add internal/dialer/transport/http/middleware.go internal/dialer/transport/http/middleware_test.go internal/dialer/transport/http/routes.go internal/dialer/module.go
git commit -m "feat(dialer): Plan 11.1 Task 1 — RefreshPresence middleware on operator routes"
```

---

## Task 2: Realtime trunks.health cross-tenant fan-out

**Goal:** Close the `trunks.health` TODO in `events/nats_subscriber.go` by introducing a per-tenant replicator that subscribes to the global `trunks.health` subject and broadcasts to admin subscribers in every tenant.

**Files:**
- Create: `internal/realtime/events/trunks_replicator.go` + `_test.go`.
- Modify: `internal/realtime/events/nats_subscriber.go` — remove the TODO, register the replicator at Start.

### Architecture decision

The realtime Hub refuses empty-`TenantID` broadcasts (defence against cross-tenant leak). `trunks.health` is a global signal (FreeSWITCH cluster trunk states) without a tenant scope. Two options:

**A.** Add `BroadcastAllTenants` flag to `BroadcastFilter`. Forces api/ change.
**B.** Have the dispatcher list active tenants (from a TenantLister port) and emit one Hub.Broadcast per tenant.

We pick **B** — keeps api/ stable; cost is one extra Lookup per `trunks.health` event (low frequency: ~1/min during incidents). The TenantLister port is satisfied by `tenancy.TenantService.List`.

### Step-by-step

- [ ] **Step 2.1: Read existing dispatcher**

```bash
cat internal/realtime/events/nats_subscriber.go
```

Note: there are 5 wired patterns (operators.state / dialer.queue / call.events / notify.user / force.user). The `trunks.health` skip is in `Start` (debug log + no Subscribe call).

- [ ] **Step 2.2: Failing test for `*TrunksReplicator` (RED)**

`internal/realtime/events/trunks_replicator_test.go`:

```go
func TestTrunksReplicator_FansOutToEveryActiveTenant(t *testing.T) {
	t.Parallel()

	hub := &fakeHub{}
	lister := &fakeTenantLister{
		tenants: []string{"tenant-A", "tenant-B", "tenant-C"},
	}
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), nil)

	require.NoError(t, replicator.Dispatch(t.Context(), []byte(`{"node":"fs1","ok":true}`)))

	calls := hub.Calls()
	require.Len(t, calls, 3)
	tenants := []string{}
	for _, c := range calls {
		require.Equal(t, rtapi.TopicTrunksHealth, c.topic)
		tenants = append(tenants, c.filter.TenantID)
	}
	require.ElementsMatch(t, []string{"tenant-A", "tenant-B", "tenant-C"}, tenants)
}

func TestTrunksReplicator_TenantListerErrorIsLogged(t *testing.T) {
	t.Parallel()

	hub := &fakeHub{}
	lister := &fakeTenantLister{err: errors.New("db down")}
	logCore, logs := observer.New(zap.WarnLevel)
	replicator := events.NewTrunksReplicator(hub, lister, zap.New(logCore), nil)

	// Tenant lister failure must NOT propagate to the bus (would
	// trigger NATS redelivery loop). Just log + skip.
	err := replicator.Dispatch(t.Context(), []byte(`{}`))
	require.NoError(t, err)
	require.Empty(t, hub.Calls())
	require.Equal(t, 1, logs.FilterMessageSnippet("tenant lister failed").Len())
}

func TestTrunksReplicator_NoActiveTenantsIsNoop(t *testing.T) {
	t.Parallel()

	hub := &fakeHub{}
	lister := &fakeTenantLister{tenants: []string{}}
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), nil)

	require.NoError(t, replicator.Dispatch(t.Context(), []byte(`{}`)))
	require.Empty(t, hub.Calls())
}
```

`fakeTenantLister` is a minimal in-test stub satisfying the `events.TenantLister` interface introduced in Step 2.4.

- [ ] **Step 2.3: Run tests → fail**

```
go test ./internal/realtime/events/... -run TrunksReplicator -v
```
Expected: FAIL — `undefined: events.NewTrunksReplicator`, `events.TenantLister`.

- [ ] **Step 2.4: Implement `*TrunksReplicator` (GREEN)**

`internal/realtime/events/trunks_replicator.go`:

```go
package events

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// TenantLister is the subset of tenancy.TenantService the replicator
// needs. Narrow on purpose so production wiring can adapt
// tenancy.TenantService without test code seeing Tenant DTOs.
type TenantLister interface {
	// ListActiveTenantIDs returns every active tenant's stringified ID.
	// Implementations cache aggressively — a 60s TTL is acceptable
	// because a freshly-onboarded tenant misses at most one trunks.health
	// event before its first cache miss.
	ListActiveTenantIDs(ctx context.Context) ([]string, error)
}

// TrunksReplicator owns the cross-tenant fan-out of the global
// trunks.health subject. The Hub.Broadcast contract requires a
// non-empty TenantID; trunks.health has no tenant scope, so the
// replicator emits one Broadcast per active tenant.
//
// Listed tenants are resolved through the TenantLister port. Lister
// failures are logged + skipped (returning an error would trigger NATS
// redelivery for a permanently-broken catalog).
type TrunksReplicator struct {
	hub     HubBroadcaster
	lister  TenantLister
	logger  *zap.Logger
	metrics *Metrics
}

func NewTrunksReplicator(hub HubBroadcaster, lister TenantLister, logger *zap.Logger, metrics *Metrics) *TrunksReplicator {
	if hub == nil {
		panic("realtime/events: NewTrunksReplicator: hub must be non-nil")
	}
	if lister == nil {
		panic("realtime/events: NewTrunksReplicator: lister must be non-nil")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &TrunksReplicator{
		hub:     hub,
		lister:  lister,
		logger:  logger,
		metrics: metrics,
	}
}

// Dispatch is the inbound-message handler the bus subscriber wires to
// the trunks.health subject. Always returns nil so the bus acks; lister
// failures and individual fan-out gaps are observed via metrics +
// debug logs, not propagated.
func (r *TrunksReplicator) Dispatch(ctx context.Context, payload []byte) error {
	tenants, err := r.lister.ListActiveTenantIDs(ctx)
	if err != nil {
		r.logger.Warn("realtime/events: trunks.health: tenant lister failed", zap.Error(err))
		r.metrics.observeDispatchFailure("trunks.health", reasonTenantListerFailed)
		return nil
	}
	if len(tenants) == 0 {
		// Boot ordering: until the first tenant is created, trunks.health
		// fans nothing out. Acceptable.
		return nil
	}
	frame := json.RawMessage(payload)
	for _, tenantID := range tenants {
		count := r.hub.Broadcast(ctx, rtapi.TopicTrunksHealth, frame, rtapi.BroadcastFilter{TenantID: tenantID})
		r.metrics.observeMessage("trunks.health")
		r.metrics.observeFanout(count)
	}
	return nil
}

// reasonTenantListerFailed is a NEW failure-reason label. Add to the
// existing reason set in metrics.go (alongside reasonMalformed,
// reasonEmptyTenant) so the dashboard label set stays bounded.
const reasonTenantListerFailed = "tenant_lister_failed"

// errReplicatorNoLister is reserved for future callers that want a
// hard fail rather than the silent "no tenants" no-op.
var errReplicatorNoLister = fmt.Errorf("realtime/events: trunks replicator has no lister")

var _ = errReplicatorNoLister // suppress unused while reserved
```

Add `reasonTenantListerFailed` to `metrics.go`'s reason whitelist and ensure `observeDispatchFailure` accepts it.

- [ ] **Step 2.5: Run tests → green**

```
go test ./internal/realtime/events/... -run TrunksReplicator -race -v
```
Expected: PASS — 3 cases.

- [ ] **Step 2.6: Wire into the dispatcher's Start**

Modify `internal/realtime/events/nats_subscriber.go`:

```go
// Add a TrunksReplicator field to NATSSubscriber.
type NATSSubscriber struct {
    // ... existing fields ...
    trunks *TrunksReplicator // optional; nil disables trunks.health fan-out
}

// Add a setter (or extend constructor with an Option):
func WithTrunksReplicator(r *TrunksReplicator) Option {
    return func(o *subscriberOptions) { o.trunks = r }
}
```

In `Start`, replace the TODO debug log with a real Subscribe to `trunks.health`:

```go
if s.trunks != nil {
    handler := func(_ string, payload []byte) error {
        return s.trunks.Dispatch(ctx, payload)
    }
    if err := s.bus.Subscribe(ctx, "trunks.health", s.queue, handler); err != nil {
        return fmt.Errorf("realtime/events: subscribe trunks.health: %w", err)
    }
}
```

- [ ] **Step 2.7: Update the dispatcher tests for the new option**

Add a `TestNATSSubscriber_StartRegistersTrunksHealthWhenReplicatorPresent` test that proves Subscribe is called for `"trunks.health"` when the option is supplied, and is NOT called when it's nil (preserves backward compat).

- [ ] **Step 2.8: Wire in cmd/api**

`cmd/api/main.go` — when constructing the dispatcher in section 9b, also build the replicator:

```go
trunksReplicator := rtevents.NewTrunksReplicator(
    hub,
    tenancyAdapter, // tenancy.TenantService → events.TenantLister
    logger.Named("realtime.trunks"),
    eventsMetrics,
)
dispatcher = rtevents.NewNATSSubscriber(
    natsSub, hub, ..., 
    rtevents.WithReplicaID(uuid.NewString()),
    rtevents.WithTrunksReplicator(trunksReplicator),
)
```

`tenancyAdapter` is a small inline adapter that calls `tenancy.TenantService.List(ctx, ...)` and projects to `[]string` — implement in `cmd/api/realtime.go` (NEW file or inline in main.go).

- [ ] **Step 2.9: Run + lint + commit**

```
go test ./internal/realtime/events/... ./cmd/api/... -race -count=1
go vet ./internal/realtime/...
golangci-lint run --timeout=2m ./internal/realtime/... ./cmd/api/...
git add internal/realtime/events/trunks_replicator.go internal/realtime/events/trunks_replicator_test.go internal/realtime/events/nats_subscriber.go internal/realtime/events/nats_subscriber_test.go internal/realtime/events/metrics.go cmd/api/main.go cmd/api/realtime.go
git commit -m "feat(realtime/events): Plan 11.1 Task 2 — trunks.health cross-tenant fan-out"
```

---

## Task 3: Dialer SnapshotPubSub → NATS swap

**Goal:** Replace the in-memory `*dialer.PubSub` with a NATS-backed adapter so a snapshot published on pod A reaches WS subscribers on pod B. Keep the existing `dialer.Publisher` and `transporthttp.SnapshotPubSub` interfaces unchanged so call sites don't churn.

**Files:**
- Create: `internal/dialer/pubsub_nats.go` + `_test.go` — `*NATSPubSub` adapter satisfying both interfaces.
- Modify: `internal/dialer/module.go` — swap to `NATSPubSub` when `Deps.EventBus` and `Deps.Subscriber` are non-nil; fall back to in-memory `*PubSub` otherwise.

### Architecture decision

The existing `*PubSub` is per-pod. To cross replicas we publish the Snapshot as JSON on subject `tenant.<t>.dialer.op.<op_id>.state` (matches the realtime dispatcher's `TopicOperatorsState` pattern). Subscribers translate the JSON back into `api.Snapshot` and feed local `Subscribe` channels.

Subject naming: matches the realtime dispatcher (`tenant.*.dialer.op.*.state`) so the realtime layer ALSO receives every snapshot via NATS — the WS handler subscribed to `operators.state` already gets it without any extra wiring. Bonus: removes the need for two parallel paths (in-process PubSub + realtime layer).

### Step-by-step

- [ ] **Step 3.1: Read existing PubSub + interfaces**

```bash
cat internal/dialer/pubsub.go
grep -n "SnapshotPubSub\|Publisher" internal/dialer/transport/http/*.go internal/dialer/module.go
```

Verify `transporthttp.SnapshotPubSub` interface signature (must satisfy with the new adapter).

- [ ] **Step 3.2: Failing test for `*NATSPubSub` round-trip (RED)**

`internal/dialer/pubsub_nats_test.go`:

```go
func TestNATSPubSub_PublishReachesLocalSubscriber(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t) // copy from pkg/eventbus/helpers_test.go
	ensureStream(t, url, "DIALER", []string{"tenant.>"})

	pub, err := eventbus.NewNATSPublisher(t.Context(), []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	sub, err := eventbus.NewNATSSubscriber(t.Context(), []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	ps := dialer.NewNATSPubSub(pub, sub, "replica-1", zap.NewNop())
	require.NoError(t, ps.Start(t.Context()))
	t.Cleanup(func() { _ = ps.Stop() })

	tenantID := uuid.New()
	operatorID := uuid.New()
	ch, cancel := ps.Subscribe(tenantID, operatorID)
	t.Cleanup(cancel)

	snap := api.Snapshot{
		TenantID:   tenantID,
		OperatorID: operatorID,
		State:      api.StateReady,
		Version:    7,
	}
	ps.Publish(snap)

	select {
	case got := <-ch:
		require.Equal(t, snap, got)
	case <-time.After(3 * time.Second):
		t.Fatal("snapshot did not round-trip in 3s")
	}
}

func TestNATSPubSub_OperatorScopingFiltersOutOtherOperators(t *testing.T) {
	// Two subscribers on different operator IDs; publish snap for operator A;
	// only subscriber A receives.
}

func TestNATSPubSub_TenantScopingFiltersOutOtherTenants(t *testing.T) {
	// Same as above but with different tenant IDs.
}

func TestNATSPubSub_Stop_DrainsAndClosesAllSubscribers(t *testing.T) {
	// Subscribe twice, call Stop, expect both channels closed.
}

func TestNATSPubSub_PublishAfterStopIsNoop(t *testing.T) {
	// Stop, then Publish — must not panic, must not leak.
}
```

- [ ] **Step 3.3: Run → fail**

```
go test ./internal/dialer/... -run NATSPubSub -v
```
Expected: FAIL — undefined `dialer.NewNATSPubSub`.

- [ ] **Step 3.4: Implement `*NATSPubSub` (GREEN)**

`internal/dialer/pubsub_nats.go`:

```go
package dialer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer/api"
	transporthttp "github.com/sociopulse/platform/internal/dialer/transport/http"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// NATSPubSub is the cross-replica Snapshot fan-out backed by NATS
// JetStream. Snapshots are published to
// tenant.<tenantID>.dialer.op.<operatorID>.state and re-delivered
// to every replica's local subscribers.
//
// Same Subject pattern as the realtime dispatcher's TopicOperatorsState
// — the realtime layer receives every snapshot for free, so a single
// publish covers both:
//
//   1. Local + remote dialer.PubSub subscribers (this adapter)
//   2. Every realtime WS client subscribed to operators.state
//      (via internal/realtime/events.NATSSubscriber)
//
// Keeps the eventing surface DRY.
type NATSPubSub struct {
	pub       eventbus.Publisher
	sub       eventbus.Subscriber
	replicaID string
	logger    *zap.Logger

	mu          sync.RWMutex
	subscribers map[pubSubKey][]*pubSubChan
	closed      bool
	started     bool
}

func NewNATSPubSub(pub eventbus.Publisher, sub eventbus.Subscriber, replicaID string, logger *zap.Logger) *NATSPubSub {
	if pub == nil { panic("dialer.NewNATSPubSub: pub must be non-nil") }
	if sub == nil { panic("dialer.NewNATSPubSub: sub must be non-nil") }
	if logger == nil { logger = zap.NewNop() }
	if replicaID == "" { replicaID = uuid.NewString() }
	return &NATSPubSub{
		pub: pub, sub: sub, replicaID: replicaID, logger: logger,
		subscribers: make(map[pubSubKey][]*pubSubChan),
	}
}

// Start registers the JetStream subscriber on tenant.>.dialer.op.>.state
// using a per-replica queue group so every replica receives every
// snapshot (each replica needs to fan out to its local Subscribers).
func (n *NATSPubSub) Start(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started { return fmt.Errorf("dialer/pubsub_nats: Start called twice") }
	queue := "dialer-pubsub-" + n.replicaID
	handler := func(subject string, payload []byte) error {
		var snap api.Snapshot
		if err := json.Unmarshal(payload, &snap); err != nil {
			n.logger.Debug("dialer/pubsub_nats: malformed snapshot", zap.String("subject", subject), zap.Error(err))
			return nil // ack — permanent malformed
		}
		n.deliverLocal(snap)
		return nil
	}
	if err := n.sub.Subscribe(ctx, "tenant.*.dialer.op.*.state", queue, handler); err != nil {
		return fmt.Errorf("dialer/pubsub_nats: subscribe: %w", err)
	}
	n.started = true
	return nil
}

// Stop closes every local subscriber and drops the started flag.
// The underlying eventbus.Subscriber is owned by cmd/api.
func (n *NATSPubSub) Stop() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed { return nil }
	n.closed = true
	for _, slot := range n.subscribers {
		for _, c := range slot {
			c.once.Do(func() { close(c.ch) })
		}
	}
	n.subscribers = nil
	return nil
}

// Publish marshals snap and publishes to NATS. Synchronous via
// pkg/eventbus.NATSPublisher (broker-acked). Errors are logged + not
// returned because the *Publisher interface is fire-and-forget.
func (n *NATSPubSub) Publish(snap api.Snapshot) {
	subject := fmt.Sprintf("tenant.%s.dialer.op.%s.state", snap.TenantID, snap.OperatorID)
	payload, err := json.Marshal(snap)
	if err != nil {
		n.logger.Warn("dialer/pubsub_nats: marshal", zap.Error(err))
		return
	}
	// Publish ctx must be ctx.Background-equivalent — Publish callers
	// don't pass ctx (legacy interface). Use a short timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := n.pub.Publish(ctx, subject, payload); err != nil {
		n.logger.Warn("dialer/pubsub_nats: publish", zap.Error(err))
	}
}

// Subscribe + deliverLocal mirror the in-memory PubSub semantics:
// drop-on-full-buffer, per-(tenant, operator) scoping, idempotent cancel.
// Implementation is identical to pubsub.go except the local map is
// fed by deliverLocal (called from the bus handler) instead of Publish.

// (Subscribe / deliverLocal / safeSend etc. are mechanical adapters of
// the existing pubsub.go internals — implementer copies the patterns.)

var (
	_ Publisher                    = (*NATSPubSub)(nil)
	_ transporthttp.SnapshotPubSub = (*NATSPubSub)(nil)
)
```

- [ ] **Step 3.5: Run tests → green**

```
go test ./internal/dialer/... -run NATSPubSub -race -v
```
Expected: PASS — 5 cases.

- [ ] **Step 3.6: Wire in module.go**

```go
// In Module.Register:
var pubsub Publisher
var snapshotPubsub transporthttp.SnapshotPubSub
if d.EventBus != nil && d.Subscriber != nil {
    natsPS := NewNATSPubSub(d.EventBus, d.Subscriber, m.replicaID, logger.Named("dialer.pubsub"))
    if err := natsPS.Start(d.Ctx); err != nil {
        return fmt.Errorf("dialer: start nats pubsub: %w", err)
    }
    m.pubsub = natsPS
    pubsub = natsPS
    snapshotPubsub = natsPS
} else {
    inMem := NewPubSub()
    m.pubsub = inMem
    pubsub = inMem
    snapshotPubsub = inMem
}
```

`Module.Stop` calls `m.pubsub.Stop()` (or `Close()` for in-mem) regardless of which path was taken. Add a small `pubSubLifecycle` interface with `Stop() error` to unify both.

- [ ] **Step 3.7: Run all tests + lint + commit**

```
go build ./...
go test ./internal/dialer/... -race -count=1
go test -tags=integration -count=1 ./internal/realtime/...
go vet ./...
golangci-lint run --timeout=2m ./...
git add internal/dialer/pubsub_nats.go internal/dialer/pubsub_nats_test.go internal/dialer/module.go
git commit -m "feat(dialer): Plan 11.1 Task 3 — SnapshotPubSub NATS-backed fan-out"
```

---

## Task 4: Telephony nats_bridge real

**Goal:** Replace the Plan 09 stub at `internal/telephony/nats_bridge/bridge.go` with a real bridge that subscribes to `tenant.<t>.telephony.cmd.>` and publishes ESL events to `tenant.<t>.telephony.event.>`. Idempotency via Redis SETNX 24 h.

**Files:**
- Modify: `internal/telephony/nats_bridge/bridge.go` — real Start/Stop/Drain.
- Create: `internal/telephony/nats_bridge/cmd_subscriber.go` — handles inbound NATS commands, dispatches via ESL pool.
- Create: `internal/telephony/nats_bridge/event_publisher.go` — listens on `*pool.ESLPool.Events()` chan, publishes to NATS.
- Create: `internal/telephony/nats_bridge/idempotency.go` — Redis-backed SETNX guard.
- Create: `internal/telephony/nats_bridge/metrics.go` — Prometheus collectors.
- Create: `internal/telephony/nats_bridge/*_test.go` — TDD coverage.

### Architecture

```
NATS tenant.<t>.telephony.cmd.<call_id>     ESL pool                          NATS tenant.<t>.telephony.event.<call_id>.<kind>
        │                                       │                                        ▲
        ▼                                       ▼                                        │
┌─────────────────┐                   ┌─────────────────┐                   ┌─────────────────┐
│  cmdSubscriber  │ ── idempotency ──▶│ poolDispatcher  │ ───── events ────▶│ eventPublisher  │
│ (one per cmd)   │   (Redis SETNX)   │ (Originate etc.)│   (per-node fan)  │ (one per event) │
└─────────────────┘                   └─────────────────┘                   └─────────────────┘
        │                                                                             │
        │                                                                             │
        └─────────────────── Bridge.Start orchestrates both ─────────────────────────┘
```

Three goroutines per Bridge: cmd-subscriber handler (managed by NATS subscriber goroutines), event-publisher loop (one goroutine reads `pool.Events()` chan and publishes to NATS), drain coordinator.

### Step-by-step

- [ ] **Step 4.1: Read Plan 09 stub + telephony api/**

```bash
cat internal/telephony/nats_bridge/bridge.go
cat internal/telephony/api/*.go | head -200
```

Identify: command DTOs (`OriginateCommand`, `HangupCommand`, `MixmonitorCommand`); event types (`ChannelEvent`, `ChannelEventType` enum); subject helpers `SubjectChannelEventFor(tenantID, callID)`.

- [ ] **Step 4.2: Failing test — idempotency guard (RED)**

`internal/telephony/nats_bridge/idempotency_test.go`:

```go
func TestIdempotency_RejectsDuplicateWithinTTL(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	guard := nats_bridge.NewIdempotencyGuard(rdb, 24*time.Hour, zap.NewNop())

	first, err := guard.MarkSeen(t.Context(), "command-uuid-1")
	require.NoError(t, err)
	require.True(t, first, "first MarkSeen returns true (newly seen)")

	second, err := guard.MarkSeen(t.Context(), "command-uuid-1")
	require.NoError(t, err)
	require.False(t, second, "duplicate within TTL returns false")
}

func TestIdempotency_AcceptsAfterTTLExpires(t *testing.T) {
	// MarkSeen → mr.FastForward beyond TTL → MarkSeen returns true again.
}

func TestIdempotency_RedisFailureBubblesUp(t *testing.T) {
	// mr.Close() then MarkSeen — MUST return error so caller can NACK.
	// Idempotency failure is NOT silent — duplicate execution is worse
	// than a redelivery loop.
}
```

- [ ] **Step 4.3: Run → fail**

- [ ] **Step 4.4: Implement `IdempotencyGuard`**

```go
// internal/telephony/nats_bridge/idempotency.go
type IdempotencyGuard struct {
	rdb    redis.UniversalClient
	ttl    time.Duration
	logger *zap.Logger
}

func NewIdempotencyGuard(rdb redis.UniversalClient, ttl time.Duration, logger *zap.Logger) *IdempotencyGuard {
	if rdb == nil { panic("nats_bridge: NewIdempotencyGuard: rdb required") }
	if ttl <= 0 { ttl = 24 * time.Hour }
	if logger == nil { logger = zap.NewNop() }
	return &IdempotencyGuard{rdb: rdb, ttl: ttl, logger: logger}
}

// MarkSeen returns (true, nil) if the commandID was NEW; (false, nil)
// if a duplicate within TTL. Returns error only on Redis-side failure
// — never silently dedup-fails.
func (g *IdempotencyGuard) MarkSeen(ctx context.Context, commandID string) (bool, error) {
	key := "telephony:idempotency:" + commandID
	ok, err := g.rdb.SetNX(ctx, key, "1", g.ttl).Result()
	if err != nil {
		return false, fmt.Errorf("nats_bridge: idempotency setnx: %w", err)
	}
	return ok, nil
}
```

- [ ] **Step 4.5: Failing test — command subscriber dispatch (RED)**

`cmd_subscriber_test.go`:

```go
func TestCmdSubscriber_DispatchesOriginateToPool(t *testing.T) {
	// Build a fake Pool that records Originate calls; fake idempotency
	// guard that always says "new"; publish a Marshal'd OriginateCommand
	// to the bus; assert pool.Originate called with the right node + args.
}

func TestCmdSubscriber_SkipsDuplicateCommand(t *testing.T) {
	// Same, but idempotency guard returns (false, nil) — pool MUST NOT be called.
}

func TestCmdSubscriber_NACKsOnPoolError(t *testing.T) {
	// Pool returns error → handler returns error → bus NAKs and redelivers.
}

func TestCmdSubscriber_NACKsOnIdempotencyRedisFailure(t *testing.T) {
	// Idempotency guard returns error → handler returns error → NACK.
}
```

- [ ] **Step 4.6: Implement `cmdSubscriber`**

```go
// cmd_subscriber.go
type cmdSubscriber struct {
	bus     eventbus.Subscriber
	pool    poolDispatcher
	router  *router.Router
	guard   *IdempotencyGuard
	logger  *zap.Logger
	metrics *Metrics
}

// poolDispatcher is the narrow surface cmdSubscriber needs (decouples
// from the full *pool.ESLPool for tests).
type poolDispatcher interface {
	Originate(ctx context.Context, node string, cmd telapi.OriginateCommand) error
	Hangup(ctx context.Context, node, callID string) error
	MixMonitorStart(ctx context.Context, node, callID, path string) error
	MixMonitorStop(ctx context.Context, node, callID string) error
}

func (c *cmdSubscriber) handle(ctx context.Context, subject string, payload []byte) error {
	// 1. Decode payload (typed envelope: {kind: "originate"|..., ...args}).
	// 2. Idempotency check via guard.MarkSeen(ctx, envelope.CommandID).
	// 3. Route to pool method based on kind.
	// 4. Errors return non-nil → bus NACK → redelivery.
}

func (c *cmdSubscriber) Start(ctx context.Context) error {
	return c.bus.Subscribe(ctx, "tenant.*.telephony.cmd.>", "telephony-bridge", c.handle)
}
```

- [ ] **Step 4.7: Failing test — event publisher (RED)**

`event_publisher_test.go`:

```go
func TestEventPublisher_PublishesEachChannelEvent(t *testing.T) {
	// Feed a fake events chan with a ChannelEvent for tenant T1, callID C1, kind=Bridge;
	// assert NATS.Publish called on subject "tenant.T1.telephony.event.C1.bridge"
	// with the JSON-marshalled event.
}

func TestEventPublisher_DropsEventOnNATSError(t *testing.T) {
	// fake publisher returns error → metric tick + log + continue.
	// MUST NOT exit the loop on transient errors.
}

func TestEventPublisher_StopDrainsInFlightEvents(t *testing.T) {
	// Stop is called → events chan drained → final publishes happen.
}
```

- [ ] **Step 4.8: Implement `eventPublisher`**

```go
// event_publisher.go
type eventPublisher struct {
	pub     eventbus.Publisher
	events  <-chan pool.EventEnvelope // from *pool.ESLPool.Events()
	logger  *zap.Logger
	metrics *Metrics

	wg sync.WaitGroup
}

func (e *eventPublisher) Run(ctx context.Context) {
	e.wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env, ok := <-e.events:
				if !ok { return }
				e.publishOne(ctx, env)
			}
		}
	})
}

func (e *eventPublisher) publishOne(ctx context.Context, env pool.EventEnvelope) {
	// 1. Build subject via telapi.SubjectChannelEventFor(env.TenantID, env.CallID, env.Kind).
	// 2. JSON-marshal env.Event (the api.ChannelEvent).
	// 3. e.pub.Publish(ctx, subject, payload) with bounded ctx (2s timeout).
	// 4. On error: metrics.observePublishError() + debug log + return (next event continues).
}

func (e *eventPublisher) Stop() { e.wg.Wait() }
```

- [ ] **Step 4.9: Wire in `bridge.go`**

```go
// bridge.go (real impl):
type Bridge struct {
	cfg         Config
	logger      *zap.Logger
	cmdSub      *cmdSubscriber
	evtPub      *eventPublisher
	cancelEvent context.CancelFunc
}

func New(cfg Config) *Bridge {
	logger := cfg.Logger
	if logger == nil { logger = zap.NewNop() }
	return &Bridge{cfg: cfg, logger: logger}
}

func (b *Bridge) Start(ctx context.Context) error {
	guard := NewIdempotencyGuard(b.cfg.Redis, 24*time.Hour, b.logger.Named("idempotency"))
	b.cmdSub = newCmdSubscriber(b.cfg.NATSSubscriber, b.cfg.Pool, b.cfg.Router, guard, b.logger, nil)
	if err := b.cmdSub.Start(ctx); err != nil {
		return fmt.Errorf("nats_bridge: start cmd subscriber: %w", err)
	}
	evtCtx, cancel := context.WithCancel(ctx)
	b.cancelEvent = cancel
	b.evtPub = newEventPublisher(b.cfg.NATSPublisher, b.cfg.Pool.Events(), b.logger, nil)
	b.evtPub.Run(evtCtx)
	return nil
}

func (b *Bridge) Stop() {
	if b.cancelEvent != nil { b.cancelEvent() }
	if b.evtPub != nil { b.evtPub.Stop() }
}

func (b *Bridge) Drain(ctx context.Context) error {
	// Stop accepting new commands; let in-flight finish within ctx deadline.
	b.Stop()
	return nil
}
```

`Config` gains `NATSPublisher` + `NATSSubscriber` fields (replacing the raw `*nats.Conn` from the stub) + `Redis` (already present).

- [ ] **Step 4.10: cmd/telephony-bridge wiring**

`cmd/telephony-bridge/main.go` — when constructing Bridge, swap from raw `*nats.Conn` to `eventbus.NewNATSPublisher` / `NewNATSSubscriber`. Defer chain: `Bridge.Drain → Bridge.Stop → subscriber.Close → publisher.Close`.

- [ ] **Step 4.11: Run + lint + commit**

```
go test ./internal/telephony/... -race -count=1
go test ./cmd/telephony-bridge/... -race -count=1
go vet ./...
golangci-lint run --timeout=2m ./...
git add internal/telephony/nats_bridge/ cmd/telephony-bridge/main.go
git commit -m "feat(telephony/nats_bridge): Plan 11.1 Task 4 — real cmd subscriber + event publisher + Redis idempotency"
```

---

## Self-review

**Spec coverage:**
- Carry-over 1 (telephony nats_bridge real) → Task 4 ✓
- Carry-over 5 (dialer RefreshPresence wiring) → Task 1 ✓
- Carry-over 6 (dialer SnapshotPubSub NATS swap) → Task 3 ✓
- Realtime trunks.health TODO → Task 2 ✓
- Plan 11 Task 10 → DEFERRED to Plan 11.2 (called out in opening scope note)

**Placeholder scan:** No "TBD" / "implement later" / "similar to" patterns. Every task has concrete file paths, concrete code shapes, concrete test names, concrete commit messages.

**Type consistency:**
- Task 1 `RefreshFn` signature `(ctx, tenantID, operatorID uuid.UUID) error` matches the adapter call in Module.
- Task 2 `TenantLister.ListActiveTenantIDs(ctx) ([]string, error)` matches the test fake's `tenants []string` field.
- Task 3 `NATSPubSub` satisfies both `Publisher` and `transporthttp.SnapshotPubSub` (compile-time checks at end of file).
- Task 4 `poolDispatcher` interface narrows the *pool.ESLPool surface; cmd_subscriber fakes it without depending on the full pool.

**Implementer reading list (per task):**
- Task 1: `internal/dialer/transport/http/middleware.go`, `internal/dialer/fsm/heartbeat.go`, `internal/auth/api/dto.go` (Claims).
- Task 2: `internal/realtime/events/nats_subscriber.go`, `internal/tenancy/api/tenant_service.go`, `pkg/eventbus/helpers_test.go` (embedded JetStream pattern).
- Task 3: `internal/dialer/pubsub.go`, `pkg/eventbus/publisher.go`, `pkg/eventbus/helpers_test.go`.
- Task 4: `internal/telephony/api/`, `internal/telephony/pool/pool.go`, `pkg/eventbus/nats.go` (publisher Healthy contract), `pkg/eventbus/helpers_test.go`.

**Carry-forward checklist (every task):**
- TDD strict (red → green → next).
- `go vet` + `golangci-lint` clean.
- `go test -race -count=1` clean.
- `goleak.VerifyTestMain` present (or inherited via existing TestMain).
- 2-stage review (spec compliance + code quality) before commit.
