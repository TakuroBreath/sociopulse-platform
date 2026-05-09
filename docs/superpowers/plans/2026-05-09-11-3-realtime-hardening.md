# Plan 11.3: Realtime Hardening Follow-Ups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the three small security/maintenance follow-ups left after Plan 11.2 to harden the realtime cross-tenant subscribe path: (1) scrub the wire-side leakage of inner resolver error strings; (2) wire NATS-driven cache invalidation for `crm.project.status_changed` so a project archive doesn't leave 60s of stale-cached cross-tenant approvals; (3) surface the projectResolverAdapter's `(nil, nil)` defensive guard via a metric so a service-layer bug doesn't hide silently.

**Architecture:** Three independent additions threaded through `internal/realtime/transport/http`, `internal/realtime/service`, `internal/realtime/events`, and `cmd/api`.
- Task 1 patches `wsHandler.handleSubscribeFrame` so `FrameSubscribeErr.Reason` always emits a fixed string when the underlying error is `ErrCrossTenantSubscribe`.
- Task 2 adds a public `Invalidate(id string)` method on `*CachedUserResolver` and `*CachedProjectResolver` so external invalidators can drop a specific cache key + clear any in-flight singleflight.
- Task 3 introduces `*CacheInvalidator` in `internal/realtime/events/` that subscribes to `tenant.*.crm.project.status_changed` and calls `Invalidate` on the project cache. (auth-side user invalidation is out of scope — the auth module does not currently publish a `user.deleted` event; deferred to a future plan that introduces that subject.)
- Task 4 adds `realtime_resolver_adapter_inconsistent_total{type}` counter and ticks it from `projectResolverAdapter.Get`'s `(nil, nil)` defensive branch.

**Tech Stack:** Go 1.26.3, `internal/realtime`, `pkg/eventbus`, `prometheus`, `zap`, `go.uber.org/sync/singleflight`. No new external deps.

**Out of scope (deferred):**
- **Auth user-deleted invalidation.** `auth` module does not yet publish a `tenant.<t>.auth.user.deleted` event. Adding that event is out of scope for Plan 11.3. The 60s cache TTL bounds stale visibility for users; this is acceptable because the JWT lifetime (default 15min) bounds the security envelope independently. A future plan that wires auth-side user lifecycle events should extend `CacheInvalidator` accordingly.
- **CallResolver cache invalidation.** Plan 11.2 deferred CallResolver itself to Plan 12 (recording metadata); invalidation follows.

**Required reading list (skim before starting):**
- `docs/superpowers/plans/2026-05-09-11-2-realtime-frame-classification-rbac.md` — Plan 11.2's full implementation; this plan builds on its resolver wiring + RBAC error-fold pattern.
- `internal/realtime/transport/http/ws_handler.go:213-262` — current `handleSubscribeFrame` (Task 1 modifies the FrameSubscribeErr emission).
- `internal/realtime/service/resolver_cache.go` (full) — current cache + singleflight (Task 2 adds `Invalidate`).
- `internal/realtime/events/nats_subscriber.go` — existing event-subscriber pattern; Task 3's CacheInvalidator mirrors it.
- `internal/realtime/events/trunks_replicator.go` — Plan 11.1 example of an events-package goroutine with Start/Stop discipline (Task 3 follows the same shape).
- `internal/crm/api/events.go` — `SubjectProjectStatus` constant + `SubjectProjectStatusFor(uuid.UUID) string` helper + `ProjectStatusChangedEvent{ProjectID, TenantID, OldStatus, NewStatus, ChangedAt, ArchivedAt}` payload.
- `cmd/api/realtime.go:177-209` — current `projectResolverAdapter.Get` with the `(nil, nil)` defensive guard (Task 4 ticks a metric there).
- `internal/realtime/service/metrics.go` — current Metrics struct + RegisterMetrics constructor (Task 4 adds a counter).
- Plan 09/10/11/11.1/11.2 carry-forward rules:
  - No `init()` MustRegister.
  - `*zap.Logger` nil-safe.
  - Sentinel error aliasing.
  - Compile-time interface checks.
  - No `time.After` in select-loops; `time.NewTicker` + defer Stop.
  - `wg.Go(func)` (Go 1.25+).
  - Tests use `t.Parallel()`, `t.Cleanup()`, `t.Context()` (Go 1.24+).
  - `goleak.VerifyTestMain` in goroutine-spawning packages.
  - Tests in `package <pkg>_test` (external).
  - Doc comments on every exported identifier.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/realtime/transport/http/ws_handler.go` | Modify | `handleSubscribeFrame` patched: when `errors.Is(err, ErrCrossTenantSubscribe)`, emit a fixed string instead of `err.Error()`. |
| `internal/realtime/transport/http/ws_handler_test.go` | Modify | New test asserting the wire string is scrubbed. |
| `internal/realtime/service/resolver_cache.go` | Modify | Add `Invalidate(id string)` to `*CachedUserResolver` and `*CachedProjectResolver`. |
| `internal/realtime/service/resolver_cache_test.go` | Modify | Two tests: Invalidate drops a cache hit; subsequent Get re-queries inner. |
| `internal/realtime/events/cache_invalidator.go` | **NEW** | `*CacheInvalidator` subscribes to `tenant.*.crm.project.status_changed`, parses `ProjectStatusChangedEvent`, calls `projectResolver.Invalidate(project_id)`. Owns one goroutine via `wg.Go`. |
| `internal/realtime/events/cache_invalidator_test.go` | **NEW** | Embedded JetStream + fake invalidator target; verify subject pattern + Invalidate is called with the right ID; metric tick on parse error. |
| `internal/realtime/events/metrics.go` | Modify | Add `realtime_cache_invalidations_total{result}` collector + observer (result ∈ {"ok","parse_error","unknown_tenant"}). |
| `internal/realtime/module.go` | Modify | Construct + Start the CacheInvalidator when `d.Subscriber != nil` AND the project cache is wired. Stop on Module.Stop. |
| `cmd/api/realtime.go` | Modify | Call new metric tick on the `(nil, nil)` defensive branch in `projectResolverAdapter.Get`. |
| `cmd/api/realtime_test.go` | Modify (or new test) | Cover the metric tick on `(nil, nil)` returned by a fake `crmProjectGetter`. |
| `internal/realtime/service/metrics.go` | Modify | Add `realtime_resolver_adapter_inconsistent_total{type}` counter + observer. |

**Locator/cache invalidator wiring decision:** The CacheInvalidator needs to call `Invalidate` on the SAME `*CachedProjectResolver` instance that `TopicRBAC` consults. Module.Register today builds `cachedProjects` as a local var and feeds it to `NewTopicRBACWithResolvers`. We extend Module.Register to retain `cachedProjects` as a Module field so the invalidator (also constructed in Register) can reference it.

---

## Task 1: Scrub wire-string leakage on `FrameSubscribeErr.Reason`

**Files:**
- Modify: `internal/realtime/transport/http/ws_handler.go:228-262` (handleSubscribeFrame error path)
- Modify: `internal/realtime/transport/http/ws_handler_test.go` (add new test)

**Background:** Plan 11.2 Task 5 review NIT M-3 flagged this. `handleSubscribeFrame` currently emits `err.Error()` as the wire-side `FrameSubscribeErr.Reason`. When `Allow` rejects via `ErrCrossTenantSubscribe`, the inner not-found / cross-tenant message is appended:

- `"realtime: cross-tenant subscription denied: operator_id=X: cmd/api: get user X: not found"` (real not-found)
- `"realtime: cross-tenant subscription denied: operator_id=X belongs to tenant=Y"` (real cross-tenant)

The `errors.Is(err, ErrCrossTenantSubscribe)` envelope is correctly indistinguishable, but a client parsing the string CAN discriminate not-found vs. wrong-tenant. Fix: when the error chain contains `ErrCrossTenantSubscribe`, override Reason to a fixed `"cross-tenant subscription denied"`.

- [ ] **Step 1: Write the failing wire-string test**

Append to `internal/realtime/transport/http/ws_handler_test.go`:

```go
// TestWSHandler_HandleSubscribeFrame_CrossTenantReasonScrubbed locks
// in the Plan 11.3 Task 1 contract: when Allow returns
// ErrCrossTenantSubscribe (or any error wrapping it), the
// FrameSubscribeErr Reason MUST be the fixed
// "cross-tenant subscription denied" string — NOT err.Error() —
// so a client cannot probe entity existence cross-tenant via
// wire-string parsing.
func TestWSHandler_HandleSubscribeFrame_CrossTenantReasonScrubbed(t *testing.T) {
	t.Parallel()

	// stubAuth + crossTenantSubscribeFn: a SubscribeFn that always
	// rejects with a wrapped ErrCrossTenantSubscribe carrying a
	// distinguishing inner message.
	innerMsg := "operator_id=victim-op: cmd/api: get user victim-op: not found"
	subscribeFn := func(_ context.Context, _ *service.Connection, _ rtapi.Topic, _ rtapi.SubscriptionFilter) (string, error) {
		return "", fmt.Errorf("%w: %s", service.ErrCrossTenantSubscribe, innerMsg)
	}

	conn, fake := newTestConnection(t, service.ConnectionConfig{})
	conn.SeedClaims(rtapi.Claims{UserID: "u1", TenantID: "t1", Roles: []string{"supervisor"}})
	conn.SetSubscribeFnForTest(subscribeFn) // test seam — see Step 3

	h := newTestWSHandler(t)
	conn.SetHubCallback(h.HandleSubscribeFrame) // exposed test-only handle — see Step 3

	conn.HandleFrameForTest(rtapi.Frame{
		Type:  rtapi.FrameSubscribe,
		Topic: rtapi.TopicOperatorsState,
		Filter: &rtapi.SubscriptionFilter{
			OperatorID: "victim-op",
		},
	})

	// Drain the queued FrameSubscribeErr.
	got := conn.DrainSendForTest()
	require.NotNil(t, got)
	assert.Equal(t, rtapi.FrameSubscribeErr, got.Type)
	assert.Equal(t, "cross-tenant subscription denied", got.Reason,
		"Reason must be the fixed scrubbed string for ErrCrossTenantSubscribe")
	assert.NotContains(t, got.Reason, "victim-op",
		"scrubbed Reason must not leak the operator_id")
	assert.NotContains(t, got.Reason, "not found",
		"scrubbed Reason must not leak the inner not-found error")
}

// TestWSHandler_HandleSubscribeFrame_NonCrossTenantReasonPassthrough
// ensures the scrub is targeted: other RBAC errors still surface
// their err.Error() (operators may need that context to debug).
func TestWSHandler_HandleSubscribeFrame_NonCrossTenantReasonPassthrough(t *testing.T) {
	t.Parallel()

	subscribeFn := func(_ context.Context, _ *service.Connection, _ rtapi.Topic, _ rtapi.SubscriptionFilter) (string, error) {
		return "", fmt.Errorf("%w: roles=[operator] topic=trunks.health", service.ErrTopicForbidden)
	}

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	conn.SeedClaims(rtapi.Claims{UserID: "u1", TenantID: "t1", Roles: []string{"operator"}})
	conn.SetSubscribeFnForTest(subscribeFn)
	h := newTestWSHandler(t)
	conn.SetHubCallback(h.HandleSubscribeFrame)

	conn.HandleFrameForTest(rtapi.Frame{
		Type:  rtapi.FrameSubscribe,
		Topic: rtapi.TopicTrunksHealth,
	})

	got := conn.DrainSendForTest()
	require.NotNil(t, got)
	assert.Equal(t, rtapi.FrameSubscribeErr, got.Type)
	assert.Contains(t, got.Reason, "topic not allowed",
		"non-cross-tenant errors retain their original Reason")
}
```

(Helpers `SetSubscribeFnForTest`, `HandleFrameForTest`, `HandleSubscribeFrame` (exported test-handle), and `newTestWSHandler` may need to be added/exposed — see Step 3 for the seams.)

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test -race -run 'TestWSHandler_HandleSubscribeFrame_(CrossTenantReasonScrubbed|NonCrossTenantReasonPassthrough)' ./internal/realtime/transport/http/ -v
```

Expected: FAIL — `Reason` carries the inner message; helpers may be missing too.

- [ ] **Step 3: Implement the scrub + add the test seams**

In `internal/realtime/transport/http/ws_handler.go`, modify `handleSubscribeFrame`:

```go
func (h *wsHandler) handleSubscribeFrame(c *service.Connection, frame rtapi.Frame) {
	switch frame.Type {
	case rtapi.FrameSubscribe:
		filter := rtapi.SubscriptionFilter{}
		if frame.Filter != nil {
			filter = *frame.Filter
		}
		subID, err := c.Subscribe(frame.Topic, filter)
		if err != nil {
			c.Send(rtapi.Frame{
				Type:   rtapi.FrameSubscribeErr,
				Topic:  frame.Topic,
				Reason: scrubSubscribeErr(err),
			})
			return
		}
		c.Send(rtapi.Frame{
			Type:  rtapi.FrameSubscribeOK,
			Topic: frame.Topic,
			SubID: subID,
		})
	case rtapi.FrameUnsubscribe:
		c.Unsubscribe(frame.SubID)
	default:
		h.cfg.logger.Debug("realtime/ws: unexpected frame on hub callback",
			zap.String("conn_id", c.ID()),
			zap.String("kind", string(frame.Type)),
		)
	}
}

// scrubSubscribeErr returns the wire-side Reason string for a
// FrameSubscribeErr emission. ErrCrossTenantSubscribe folds to a
// fixed string so the client cannot probe entity existence
// cross-tenant via wire-string parsing (Plan 11.3 Task 1).
//
// Other RBAC errors (forbidden, filter_required, unknown_topic)
// keep their err.Error() string — operators legitimately need
// that context to debug a client-side bug.
//
// The errors.Is check uses the api-package sentinel so a wrapped
// chain (e.g. fmt.Errorf("%w: ...", ErrCrossTenantSubscribe)) is
// still detected.
func scrubSubscribeErr(err error) string {
	if errors.Is(err, rtapi.ErrCrossTenantSubscribe) {
		return "cross-tenant subscription denied"
	}
	return err.Error()
}
```

Add `"errors"` to imports. Add `rtapi` if not already imported.

For the test seams (test-only API extensions), add to `internal/realtime/service/connection.go`:

```go
// SetSubscribeFnForTest sets the SubscribeFn directly. Test helper —
// production callers go through Hub.Connect.
func (c *Connection) SetSubscribeFnForTest(fn SubscribeFn) { c.setSubscribeFn(fn) }

// HandleFrameForTest dispatches a frame as if the reader goroutine
// received it. Test helper — production callers do not invoke this.
func (c *Connection) HandleFrameForTest(frame rtapi.Frame) {
	c.dispatchFrame(context.Background(), frame)
}
```

For the wsHandler test seam, add to `internal/realtime/transport/http/ws_handler.go`:

```go
// HandleSubscribeFrame is the exported test seam for the inbound
// frame handler. Production callers wire it via SetHubCallback —
// tests may call it directly to drive the dispatch table without
// spinning up a Hub.
func (h *wsHandler) HandleSubscribeFrame(c *service.Connection, frame rtapi.Frame) {
	h.handleSubscribeFrame(c, frame)
}
```

If `newTestWSHandler` doesn't exist in the test file, add it (or use the existing test seam — read `internal/realtime/transport/http/main_test.go` and `*_test.go` files for the canonical pattern; some tests already construct a wsHandler via a private helper).

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test -race -count=3 ./internal/realtime/transport/http/ -v -run 'TestWSHandler_HandleSubscribeFrame_(CrossTenantReasonScrubbed|NonCrossTenantReasonPassthrough)'
```

Expected: PASS for both.

Run the broader package to ensure no regressions:

```bash
go test -race -count=1 ./internal/realtime/...
```

Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add internal/realtime/transport/http/ws_handler.go internal/realtime/transport/http/ws_handler_test.go internal/realtime/service/connection.go
git commit -m "$(cat <<'EOF'
feat(realtime/ws): scrub FrameSubscribeErr.Reason on cross-tenant rejection (Plan 11.3 Task 1)

Plan 11.2 Task 5 review NIT M-3 carry-over. handleSubscribeFrame
emitted err.Error() as the wire-side Reason on Subscribe rejection;
when Allow returned ErrCrossTenantSubscribe, the inner not-found /
cross-tenant message leaked into the wire so a client could parse
it to discriminate "user not found" from "wrong tenant" — defeating
the security guarantee that errors.Is correctly enforces.

Fix: extracted scrubSubscribeErr(err) helper. errors.Is check
folds ErrCrossTenantSubscribe (or any wrap) to the fixed string
"cross-tenant subscription denied". Other RBAC errors
(forbidden, filter_required, unknown_topic) keep their full
err.Error() — operators legitimately need that context.

Adds 2 tests: scrubbed-on-cross-tenant + passthrough-on-other.

Test seams added: Connection.SetSubscribeFnForTest +
HandleFrameForTest, wsHandler.HandleSubscribeFrame (exported alias)
so the dispatch-table test can run without spinning up a Hub.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `Invalidate` method on `*CachedUserResolver` / `*CachedProjectResolver`

**Files:**
- Modify: `internal/realtime/service/resolver_cache.go` (add `Invalidate(id string)` to both wrappers)
- Modify: `internal/realtime/service/resolver_cache_test.go` (2 new tests)

**Background:** Task 3 needs to drop a cached entry on a NATS event. The cache's internal `sync.Map` is unexported; we add a public `Invalidate(id)` method that:
1. Deletes the entry from the cache.
2. Calls `singleflight.Forget(id)` so an in-flight inner call (if any) is no longer cached for future joiners — they re-query the inner resolver.

- [ ] **Step 1: Write failing tests**

Append to `internal/realtime/service/resolver_cache_test.go`:

```go
// TestCachedUserResolver_InvalidateDropsCachedEntry verifies that
// Invalidate(id) drops the cached entry: the next Get re-queries
// the inner resolver, even within the TTL window.
func TestCachedUserResolver_InvalidateDropsCachedEntry(t *testing.T) {
	t.Parallel()

	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	cached := service.NewCachedUserResolver(stub, 60*time.Second)

	// First Get caches the entry.
	_, err := cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stub.Calls())

	// Second Get hits the cache (no new inner call).
	_, err = cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stub.Calls())

	// Invalidate the entry.
	cached.Invalidate("u1")

	// Next Get must re-query the inner resolver.
	_, err = cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 2, stub.Calls(),
		"Get after Invalidate must re-query the inner resolver")
}

// TestCachedUserResolver_InvalidateUnknownIDIsNoop verifies that
// Invalidate on an ID that was never cached is a silent no-op.
// (The singleflight.Forget on a missing key is also a no-op per
// the upstream documentation; the test locks in the contract.)
func TestCachedUserResolver_InvalidateUnknownIDIsNoop(t *testing.T) {
	t.Parallel()

	stub := newStubUserResolver(map[string]string{})
	cached := service.NewCachedUserResolver(stub, 60*time.Second)

	require.NotPanics(t, func() {
		cached.Invalidate("never-cached")
	})
}

// TestCachedProjectResolver_InvalidateDropsCachedEntry mirrors
// the user equivalent for the project port.
func TestCachedProjectResolver_InvalidateDropsCachedEntry(t *testing.T) {
	t.Parallel()

	stub := newStubProjectResolver(map[string]string{"p1": "t1"})
	cached := service.NewCachedProjectResolver(stub, 60*time.Second)

	_, err := cached.Get(t.Context(), "p1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stub.Calls())

	cached.Invalidate("p1")

	_, err = cached.Get(t.Context(), "p1")
	require.NoError(t, err)
	assert.EqualValues(t, 2, stub.Calls(),
		"Get after Invalidate must re-query the inner resolver")
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test -race -run 'TestCached.*_Invalidate' ./internal/realtime/service/ -v
```

Expected: FAIL — `Invalidate` is undefined on both wrappers.

- [ ] **Step 3: Add `Invalidate` to both wrappers**

In `internal/realtime/service/resolver_cache.go`, append after each wrapper's `Get` method.

For `*CachedUserResolver`:

```go
// Invalidate drops the cached entry for userID. Idempotent — no
// error if the key was never cached. Calls singleflight.Forget so
// any in-flight inner call (the leader) is uncached for future
// joiners — they re-query rather than inheriting the leader's
// (possibly stale) result. Used by the events-package cache
// invalidator (Plan 11.3 Task 3) to drop entries on NATS-side
// lifecycle events.
//
// Concurrency: safe for concurrent use with Get and other
// Invalidate calls. sync.Map.Delete + singleflight.Forget are
// independently safe; their composition is correct because the
// next Get either:
//   - finds the cache empty (Load miss) → enters singleflight,
//     gets a fresh result; OR
//   - finds an in-flight singleflight (a concurrent Get just past
//     the cache miss) → joins it, gets the leader's result. The
//     leader's result is from BEFORE the Invalidate call, but
//     that's the closest-to-current state available; Invalidate
//     guarantees the NEXT post-Forget Get sees the fresh state.
func (c *CachedUserResolver) Invalidate(userID string) {
	c.cache.Delete(userID)
	c.group.Forget(userID)
}
```

For `*CachedProjectResolver`:

```go
// Invalidate drops the cached entry for projectID. Mirror of
// CachedUserResolver.Invalidate.
func (c *CachedProjectResolver) Invalidate(projectID string) {
	c.cache.Delete(projectID)
	c.group.Forget(projectID)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test -race -count=3 -run 'TestCached.*_Invalidate' ./internal/realtime/service/ -v
```

Expected: PASS.

Full package:

```bash
go test -race -count=1 ./internal/realtime/service/
```

Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add internal/realtime/service/resolver_cache.go internal/realtime/service/resolver_cache_test.go
git commit -m "$(cat <<'EOF'
feat(realtime/service): Invalidate(id) on cached resolvers (Plan 11.3 Task 2)

Adds public CachedUserResolver.Invalidate(userID) and
CachedProjectResolver.Invalidate(projectID) methods so the
upcoming events-package cache invalidator (Task 3) can drop a
specific cache key on a NATS-side lifecycle event.

Both methods compose sync.Map.Delete + singleflight.Forget so
both the cached entry AND any in-flight inner call (the leader)
are dropped — future joiners re-query rather than inheriting
the leader's stale result.

3 tests: invalidate drops a cached entry, invalidate on never-
cached ID is a no-op, project mirror.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: NATS-driven `*CacheInvalidator` for project events

**Files:**
- Create: `internal/realtime/events/cache_invalidator.go`
- Create: `internal/realtime/events/cache_invalidator_test.go`
- Modify: `internal/realtime/events/metrics.go` (add `realtime_cache_invalidations_total` counter)
- Modify: `internal/realtime/module.go` (construct + Start the invalidator; retain `cachedProjects` as Module field)

**Background:** With Task 2's `Invalidate` method available, this task wires NATS-side project lifecycle events to cache eviction. When a project is paused / resumed / archived, the `crm` module publishes `tenant.<t>.crm.project.status_changed` carrying `ProjectStatusChangedEvent{ProjectID, TenantID, OldStatus, NewStatus, ChangedAt, ArchivedAt}`. The realtime cache holds `(project_id → tenant_id)` mappings used by `TopicRBAC`; archiving doesn't change tenant_id, but **deletion** (which the crm module doesn't yet support — out of scope) and tenant-id changes (impossible in v1) would need invalidation. We invalidate on every `status_changed` event regardless: it's cheap (single sync.Map.Delete) and covers any future status that maps to deletion.

**Pattern:** mirrors `internal/realtime/events/trunks_replicator.go` from Plan 11.1 Task 2. Owns one Subscribe, one handler, one metric tick.

- [ ] **Step 1: Write the failing invalidator tests**

Create `internal/realtime/events/cache_invalidator_test.go`:

```go
package events_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/realtime/events"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// fakeProjectInvalidator captures Invalidate calls for assertion.
type fakeProjectInvalidator struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeProjectInvalidator) Invalidate(projectID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, projectID)
}

func (f *fakeProjectInvalidator) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestCacheInvalidator_ProjectStatusChangedTriggersInvalidate verifies
// that publishing tenant.<t>.crm.project.status_changed routes the
// ProjectID to the invalidator.
func TestCacheInvalidator_ProjectStatusChangedTriggersInvalidate(t *testing.T) {
	t.Parallel()

	bus := eventbus.NewEmbeddedJetStreamForTest(t) // helper from existing events tests
	defer bus.Close()

	target := &fakeProjectInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus.Subscriber(),
		ProjectInvalidate: target.Invalidate,
		Metrics:           metrics,
		Logger:            zaptest.NewLogger(t),
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	tenantID := uuid.New()
	projectID := uuid.New()
	payload, err := json.Marshal(crmapi.ProjectStatusChangedEvent{
		ProjectID: projectID,
		TenantID:  tenantID,
		OldStatus: crmapi.ProjectStatusActive,
		NewStatus: crmapi.ProjectStatusArchived,
		ChangedAt: time.Now(),
	})
	require.NoError(t, err)

	require.NoError(t, bus.Publisher().Publish(ctx,
		crmapi.SubjectProjectStatusFor(tenantID),
		payload,
	))

	require.Eventually(t, func() bool {
		return len(target.Calls()) >= 1
	}, 2*time.Second, 10*time.Millisecond,
		"invalidator must call ProjectInvalidate on status_changed")

	calls := target.Calls()
	assert.Contains(t, calls, projectID.String(),
		"ProjectInvalidate must receive the project_id from the event")

	require.InDelta(t, 1.0,
		counterValueFromGather(t, reg, "realtime_cache_invalidations_total",
			map[string]string{"result": "ok"}), 0.0001)
}

// TestCacheInvalidator_MalformedPayloadTicksParseError verifies the
// observability path: a malformed payload bumps the parse_error
// metric label and does NOT call Invalidate.
func TestCacheInvalidator_MalformedPayloadTicksParseError(t *testing.T) {
	t.Parallel()

	bus := eventbus.NewEmbeddedJetStreamForTest(t)
	defer bus.Close()

	target := &fakeProjectInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus.Subscriber(),
		ProjectInvalidate: target.Invalidate,
		Metrics:           metrics,
		Logger:            zaptest.NewLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	tenantID := uuid.New()
	require.NoError(t, bus.Publisher().Publish(ctx,
		crmapi.SubjectProjectStatusFor(tenantID),
		[]byte("not-json"),
	))

	require.Eventually(t, func() bool {
		v := counterValueFromGather(t, reg, "realtime_cache_invalidations_total",
			map[string]string{"result": "parse_error"})
		return v >= 1.0
	}, 2*time.Second, 10*time.Millisecond,
		"malformed payload must tick parse_error metric")

	assert.Empty(t, target.Calls(),
		"malformed payload must NOT call ProjectInvalidate")
}

// TestCacheInvalidator_NewWithNilSubscriberPanics is the wiring guard.
func TestCacheInvalidator_NewWithNilSubscriberPanics(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		_ = events.NewCacheInvalidator(events.CacheInvalidatorConfig{
			ProjectInvalidate: func(string) {},
		})
	})
}

// TestCacheInvalidator_NewWithNilProjectInvalidatePanics ditto.
func TestCacheInvalidator_NewWithNilProjectInvalidatePanics(t *testing.T) {
	t.Parallel()

	bus := eventbus.NewEmbeddedJetStreamForTest(t)
	defer bus.Close()

	require.Panics(t, func() {
		_ = events.NewCacheInvalidator(events.CacheInvalidatorConfig{
			Subscriber: bus.Subscriber(),
		})
	})
}
```

The `counterValueFromGather` helper exists in other events_test files; reuse it. The `eventbus.NewEmbeddedJetStreamForTest` helper exists from Plan 11 Task 4a — reuse it.

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test -race -run 'TestCacheInvalidator' ./internal/realtime/events/ -v
```

Expected: FAIL — `events.NewCacheInvalidator` and `events.CacheInvalidatorConfig` and `events.RegisterCacheInvalidatorMetrics` are undefined.

- [ ] **Step 3: Implement the invalidator**

Create `internal/realtime/events/cache_invalidator.go`:

```go
// cache_invalidator.go subscribes to crm-side project lifecycle
// events and invalidates the realtime resolver cache so a project
// archive doesn't leave 60s of stale-cached cross-tenant approvals
// (the cache's lazy-expiry default).
//
// Subject pattern: tenant.*.crm.project.status_changed
// Payload: crmapi.ProjectStatusChangedEvent{ProjectID, TenantID, ...}
//
// Future plans extend this when auth.user.deleted (Plan 11.4+
// candidate) and recording.call.deleted (Plan 12) ship — both
// follow the same shape.
//
// Carry-forward of the events package patterns from Plan 11
// Task 4b (NATSSubscriber) and Plan 11.1 Task 2 (TrunksReplicator):
// one Subscribe, one handler, metric tick on every dispatch
// outcome, no goroutines beyond the bus's push consumer.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// projectInvalidateFn is the narrow signature CacheInvalidator
// consumes. *service.CachedProjectResolver.Invalidate satisfies it
// (string method value); tests substitute a fake.
type projectInvalidateFn func(projectID string)

// CacheInvalidatorConfig is the construction surface for
// NewCacheInvalidator. All fields except Logger are required.
type CacheInvalidatorConfig struct {
	// Subscriber is the eventbus we Subscribe to. Required.
	Subscriber eventbus.Subscriber

	// ProjectInvalidate is the function called with every
	// project_id from a parsed ProjectStatusChangedEvent. The
	// production wiring binds this to
	// (*service.CachedProjectResolver).Invalidate. Required.
	ProjectInvalidate projectInvalidateFn

	// Metrics receives counter ticks. Nil-tolerated (observe
	// methods are nil-safe).
	Metrics *CacheInvalidatorMetrics

	// Logger is named for the cache-invalidator subsystem.
	// Nil-tolerated → zap.NewNop().
	Logger *zap.Logger

	// QueueGroup is the JetStream queue group joined for the
	// subscription. Default "realtime-cache-invalidator". Test
	// suites may pin a different name to scope subscriptions.
	QueueGroup string
}

// projectStatusSubject is the wildcard subject the invalidator
// subscribes to. Built from crmapi.SubjectProjectStatus's
// "tenant.<t>.crm.project.status_changed" template by replacing
// the tenant placeholder with NATS '*' wildcard.
const projectStatusSubject = "tenant.*.crm.project.status_changed"

// defaultCacheInvalidatorQueueGroup is the JetStream queue group
// used for the cache-invalidator subscription when none is
// specified. All replicas of cmd/api join the same group so the
// bus delivers each event to exactly one replica's handler —
// matches the existing realtime/events queue-group pattern.
const defaultCacheInvalidatorQueueGroup = "realtime-cache-invalidator"

// CacheInvalidator is the events-package handle for NATS-driven
// cache invalidation. Owns one Subscribe registered at Start and
// torn down at Stop.
type CacheInvalidator struct {
	cfg CacheInvalidatorConfig

	wg       sync.WaitGroup // not used by this version; reserved.
	stopOnce sync.Once
}

// NewCacheInvalidator constructs a CacheInvalidator. nil Subscriber
// or nil ProjectInvalidate panics — wiring bugs surface at boot.
// Logger nil-safe; QueueGroup empty → defaultCacheInvalidatorQueueGroup.
func NewCacheInvalidator(cfg CacheInvalidatorConfig) *CacheInvalidator {
	if cfg.Subscriber == nil {
		panic("realtime/events: NewCacheInvalidator: Subscriber must be non-nil")
	}
	if cfg.ProjectInvalidate == nil {
		panic("realtime/events: NewCacheInvalidator: ProjectInvalidate must be non-nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.QueueGroup == "" {
		cfg.QueueGroup = defaultCacheInvalidatorQueueGroup
	}
	return &CacheInvalidator{cfg: cfg}
}

// Start registers the project-status subscription. Returns the
// bus error verbatim if Subscribe fails.
//
// Implementation note: the bus is push-mode; the handler is
// invoked in a goroutine the bus owns. CacheInvalidator does
// NOT spawn its own goroutine (carry-forward of the
// trunks_replicator pattern; Plan 11.1 Task 2).
func (c *CacheInvalidator) Start(ctx context.Context) error {
	if err := c.cfg.Subscriber.Subscribe(ctx, projectStatusSubject, c.cfg.QueueGroup, c.handle); err != nil {
		return fmt.Errorf("realtime/events: cache invalidator subscribe %q: %w", projectStatusSubject, err)
	}
	return nil
}

// Stop is the lifecycle teardown alias. The actual subscription
// is closed by the bus implementation when the parent ctx (passed
// to Start) cancels. Idempotent.
func (c *CacheInvalidator) Stop() {
	c.stopOnce.Do(func() {
		// Nothing to wait on — the bus owns the consumer goroutine.
		// Reserved for symmetry with TrunksReplicator + future
		// extensions that may add a worker goroutine.
	})
}

// handle is the per-message hook. Returns nil for ack-class
// outcomes (success, parse-error — a redelivery would not change
// the result) and never an error (parse-error is bounded by the
// crm publisher; not worth NACK-and-redeliver).
func (c *CacheInvalidator) handle(_ string, payload []byte) error {
	var ev crmapi.ProjectStatusChangedEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		c.cfg.Metrics.observe("parse_error")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop malformed payload",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}
	// Defensive: skip empty IDs. The crm publisher always sets
	// these but a future schema bump could omit; surface as a
	// metric tick rather than silent.
	if ev.ProjectID == (uuid.UUID{}) {
		c.cfg.Metrics.observe("empty_project_id")
		return nil
	}
	c.cfg.ProjectInvalidate(ev.ProjectID.String())
	c.cfg.Metrics.observe("ok")
	return nil
}

// Compile-time check that the events package's existing pattern
// (same as TrunksReplicator) is preserved.
var _ = errors.New // package-internal import-only marker — see trunks_replicator.go for context.
```

Note: the `var _ = errors.New` line at the bottom is leftover from the trunks_replicator pattern; keep it ONLY if `errors` is actually imported. If the implementation above does not use `errors`, drop the import + that line. Also: `uuid` is used in the `(uuid.UUID{})` zero-value comparison — add `"github.com/google/uuid"` import.

Also create the metrics file extension. Add to `internal/realtime/events/metrics.go`:

```go
// CacheInvalidatorMetrics is the per-handler counter set surfaced
// on /metrics. Nil-tolerated — observe is a no-op on nil receiver.
type CacheInvalidatorMetrics struct {
	invalidations *prometheus.CounterVec
}

// RegisterCacheInvalidatorMetrics registers the counter on reg and
// returns the wrapper. Panics on nil registerer (boot-time wiring
// bug); mirrors the rest of the realtime metrics constructors.
func RegisterCacheInvalidatorMetrics(reg prometheus.Registerer) *CacheInvalidatorMetrics {
	if reg == nil {
		panic("realtime/events: RegisterCacheInvalidatorMetrics: nil registerer")
	}
	m := &CacheInvalidatorMetrics{
		invalidations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "realtime_cache_invalidations_total",
			Help: "Number of resolver-cache invalidations dispatched, labelled by outcome (ok / parse_error / empty_project_id).",
		}, []string{"result"}),
	}
	reg.MustRegister(m.invalidations)
	return m
}

// observe ticks the result-labelled counter. Nil-safe.
func (m *CacheInvalidatorMetrics) observe(result string) {
	if m == nil {
		return
	}
	m.invalidations.WithLabelValues(result).Inc()
}
```

(If `internal/realtime/events/metrics.go` already imports `prometheus`, just append. Otherwise add the import.)

- [ ] **Step 4: Wire the invalidator in `internal/realtime/module.go`**

Add a Module field for `cachedProjects` so it survives Module.Register and feeds the invalidator:

```go
type Module struct {
	// ... existing fields ...
	cachedProjects *service.CachedProjectResolver
	cacheInvalidator *events.CacheInvalidator
}
```

In `Module.Register`, after constructing `cachedProjects`:

```go
m.cachedProjects = cachedProjects
```

After the existing HTTP/dispatcher wiring, add (only when `d.Subscriber != nil`):

```go
if d.Subscriber != nil {
	cacheInvalidatorMetrics := events.RegisterCacheInvalidatorMetrics(reg)
	m.cacheInvalidator = events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        d.Subscriber,
		ProjectInvalidate: cachedProjects.Invalidate,
		Metrics:           cacheInvalidatorMetrics,
		Logger:            logger.Named("cache_invalidator"),
	})
	if err := m.cacheInvalidator.Start(/* ctx — the dispatcher's ctx; if module doesn't have one, use context.Background here and rely on Stop */); err != nil {
		logger.Warn("realtime: cache invalidator start failed; cross-tenant cache will rely on TTL-only invalidation",
			zap.Error(err),
		)
		m.cacheInvalidator = nil
	} else {
		logger.Info("realtime: cache invalidator started",
			zap.String("subject", "tenant.*.crm.project.status_changed"),
		)
	}
}
```

(The implementer reads the actual `module.go` to find the right ctx to pass — likely `context.Background()` is fine here because Module.Stop will tear down via `m.cacheInvalidator.Stop()` and the bus's underlying consumer is ctx-bounded by the bus implementation.)

In `Module.Stop`, before the existing teardown:

```go
if m.cacheInvalidator != nil {
	m.cacheInvalidator.Stop()
}
```

- [ ] **Step 5: Run tests**

```bash
go test -race -count=3 ./internal/realtime/events/ -v
go test -race -count=1 ./internal/realtime/...
```

Expected: clean.

- [ ] **Step 6: Lint**

```bash
golangci-lint run ./internal/realtime/...
```

Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/realtime/events/cache_invalidator.go internal/realtime/events/cache_invalidator_test.go internal/realtime/events/metrics.go internal/realtime/module.go
git commit -m "$(cat <<'EOF'
feat(realtime/events): NATS-driven cache invalidator for project events (Plan 11.3 Task 3)

Adds *CacheInvalidator that subscribes to
tenant.*.crm.project.status_changed and calls
CachedProjectResolver.Invalidate on the project_id from each
ProjectStatusChangedEvent. Without this, a project archive (or
any future delete) would leave up to 60s of stale-cached
cross-tenant approvals in the resolver cache.

Mirrors the events-package patterns from Plan 11 Task 4b
(NATSSubscriber) and Plan 11.1 Task 2 (TrunksReplicator):
one Subscribe, one handler, metric tick on every dispatch
outcome (ok / parse_error / empty_project_id). No goroutines
beyond the bus push consumer.

Wired in realtime.Module.Register: the cached project resolver
is now retained as a Module field; CacheInvalidator binds its
Invalidate method as the project-side callback. Bus-down at
boot logs a Warn and falls back to TTL-only invalidation.

Auth-side invalidation deferred — auth module does not yet
publish a user.deleted event. Plan 11.4 candidate when it does.
CallResolver invalidation deferred to Plan 12 (CallResolver
itself depends on recording metadata).

4 tests: status_changed triggers Invalidate, malformed payload
ticks parse_error and skips Invalidate, nil Subscriber/
ProjectInvalidate panic at construction.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Adapter inconsistent metric

**Files:**
- Modify: `internal/realtime/service/metrics.go` (add `realtime_resolver_adapter_inconsistent_total{type}` collector + observer)
- Modify: `cmd/api/realtime.go` (call observer in `projectResolverAdapter.Get`'s `(nil, nil)` defensive branch)
- Modify: `cmd/api/realtime_test.go` (add test if file exists; otherwise add coverage in an existing realtime test file)

**Background:** Plan 11.2 Task 5 review M-3 noted `projectResolverAdapter.Get` has a defensive guard for `proj == nil && err == nil` from `crm.ProjectService.Get`. The guard is correct (safety belt against a hypothetical service-layer bug) but currently invisible in dashboards — a real `(nil, nil)` regression would manifest only as cross-tenant rejections, which look identical to legitimate denials. Adding a metric tick surfaces the bug class.

The metric belongs in `internal/realtime/service/metrics.go` (the `*service.Metrics` is already wired through cmd/api via the ConnectionMetrics locator; we extend the same struct). cmd/api's `projectResolverAdapter` accepts a callback to bump the metric so cmd/api doesn't import internal/realtime/service directly (preserves the existing scope rule).

- [ ] **Step 1: Write the failing test**

Find or create `cmd/api/realtime_test.go`. Add:

```go
package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
)

// fakeProjectGetterReturnsNilNil reproduces the (nil, nil) defensive
// branch in projectResolverAdapter.Get. The branch should never fire
// in production (ProjectService.Get returns ErrProjectNotFound on
// miss); a fake that returns (nil, nil) lets the test exercise the
// guard path.
type fakeProjectGetterReturnsNilNil struct{}

func (fakeProjectGetterReturnsNilNil) Get(_ context.Context, _ uuid.UUID) (*crmapi.Project, error) {
	return nil, nil
}

// TestProjectResolverAdapter_NilNilTicksInconsistentMetric verifies
// the Plan 11.3 Task 4 contract: when crm.ProjectService.Get returns
// (nil, nil) (a service-layer bug), the projectResolverAdapter
// surfaces the bug class via realtime_resolver_adapter_inconsistent_total
// rather than failing silently.
func TestProjectResolverAdapter_NilNilTicksInconsistentMetric(t *testing.T) {
	t.Parallel()

	var inconsistentTicks int
	bumpInconsistent := func(adapterType string) {
		// Test seam: Task 4 will plumb this through the adapter
		// constructor as a callback so cmd/api stays free of
		// service-package imports.
		require.Equal(t, "project", adapterType)
		inconsistentTicks++
	}

	adapter := newProjectResolverAdapterWithMetrics(
		fakeProjectGetterReturnsNilNil{},
		bumpInconsistent,
	)
	_, err := adapter.Get(t.Context(), uuid.New().String())
	require.Error(t, err,
		"adapter must surface the (nil, nil) anomaly as an error")
	require.Equal(t, 1, inconsistentTicks,
		"metric callback must tick exactly once")
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test -race -run TestProjectResolverAdapter_NilNilTicksInconsistentMetric ./cmd/api/ -v
```

Expected: FAIL — `newProjectResolverAdapterWithMetrics` is undefined.

- [ ] **Step 3: Add metric + observer in `internal/realtime/service/metrics.go`**

Add to the `Metrics` struct (next to existing fields):

```go
	resolverAdapterInconsistent *prometheus.CounterVec
```

In `RegisterMetrics`, add:

```go
		resolverAdapterInconsistent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "realtime_resolver_adapter_inconsistent_total",
			Help: "Number of resolver-adapter (nil, nil) defensive-guard fires; non-zero indicates a service-layer bug returning (nil, nil) instead of (nil, ErrNotFound) — surfaces what would otherwise look like a legitimate cross-tenant rejection.",
		}, []string{"adapter_type"}),
```

Register in MustRegister:

```go
		m.resolverAdapterInconsistent,
```

Add observer:

```go
// observeResolverAdapterInconsistent ticks when a resolver adapter's
// (nil, nil) defensive branch fires. adapterType is "user" or
// "project" — bounded label cardinality preserved. nil-safe.
func (m *Metrics) observeResolverAdapterInconsistent(adapterType string) {
	if m == nil {
		return
	}
	m.resolverAdapterInconsistent.WithLabelValues(adapterType).Inc()
}
```

- [ ] **Step 4: Add the metric-injecting constructor in `cmd/api/realtime.go`**

Add an extended adapter constructor that takes a callback. Production wiring threads `service.Metrics.observeResolverAdapterInconsistent` through; tests inject their own callback.

In `cmd/api/realtime.go`, modify `projectResolverAdapter`:

```go
type projectResolverAdapter struct {
	svc                   crmProjectGetter
	bumpInconsistent      func(adapterType string)
}

// newProjectResolverAdapterWithMetrics is the metric-aware variant
// used by registerProjectResolver (Task 4 wiring). Tests use this
// directly to inject a counting fake callback.
func newProjectResolverAdapterWithMetrics(
	svc crmProjectGetter,
	bumpInconsistent func(adapterType string),
) *projectResolverAdapter {
	if svc == nil {
		panic("cmd/api: newProjectResolverAdapterWithMetrics: svc must be non-nil")
	}
	if bumpInconsistent == nil {
		bumpInconsistent = func(string) {} // nil-safe: no-op metric callback
	}
	return &projectResolverAdapter{
		svc:              svc,
		bumpInconsistent: bumpInconsistent,
	}
}

// newProjectResolverAdapter retains backwards-compat: no metrics
// callback (degraded boot uses this when the service.Metrics is
// not in the locator).
func newProjectResolverAdapter(svc crmProjectGetter) *projectResolverAdapter {
	return newProjectResolverAdapterWithMetrics(svc, nil)
}
```

Modify `Get`:

```go
func (a *projectResolverAdapter) Get(ctx context.Context, projectID string) (rtapi.ResolvedTenant, error) {
	id, err := uuid.Parse(projectID)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: parse project_id %q: %w", projectID, err)
	}
	proj, err := a.svc.Get(ctx, id)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: get project %s: %w", id, err)
	}
	if proj == nil {
		// Defensive: ProjectService.Get returns (nil, ErrProjectNotFound)
		// on miss; we handle the error path above. A nil-without-error
		// would be a service-layer bug — surface it via metric so the
		// regression doesn't hide as a legitimate cross-tenant rejection.
		a.bumpInconsistent("project")
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: get project %s: nil project returned without error", id)
	}
	return rtapi.ResolvedTenant{TenantID: proj.TenantID.String()}, nil
}
```

In `registerProjectResolver`, wire the production callback:

```go
func registerProjectResolver(locator modules.ServiceLocator, logger *zap.Logger) {
	v, ok := locator.Lookup("crm.ProjectService")
	if !ok {
		logger.Info("realtime: crm.ProjectService missing; ProjectResolver disabled (degraded boot)")
		return
	}
	svc, ok := v.(crmapi.ProjectService)
	if !ok {
		logger.Warn("realtime: crm.ProjectService registered with wrong type; ProjectResolver disabled",
			zap.String("got_type", fmt.Sprintf("%T", v)),
		)
		return
	}
	// Production wiring: thread realtime.ConnectionMetrics through
	// to the adapter so the (nil, nil) defensive branch lands on
	// realtime_resolver_adapter_inconsistent_total.
	var bump func(string)
	if metricsRaw, ok := locator.Lookup(rtapi.LocatorConnectionMetrics); ok {
		if m, ok := metricsRaw.(*service.Metrics); ok {
			bump = m.ObserveResolverAdapterInconsistent
		}
	}
	locator.Register(rtapi.LocatorProjectResolver,
		rtapi.ProjectResolver(newProjectResolverAdapterWithMetrics(svc, bump)))
	logger.Info("realtime: ProjectResolver registered from crm.ProjectService")
}
```

The observer must be exported (`ObserveResolverAdapterInconsistent` not `observeResolverAdapterInconsistent`) so cmd/api can take a method value. Update `internal/realtime/service/metrics.go`:

```go
// ObserveResolverAdapterInconsistent is the exported variant called
// from cmd/api adapters (which cannot reach the unexported observe
// method via reflection or method values). Functional alias.
func (m *Metrics) ObserveResolverAdapterInconsistent(adapterType string) {
	m.observeResolverAdapterInconsistent(adapterType)
}
```

Apply the same pattern for `userResolverAdapter` symmetrically (although `auth.UserService.Get` has no nil-without-error path, having the metric for consistency / future-proofing is cheap):

```go
// (Optional) — extend userResolverAdapter the same way for symmetry.
// auth.UserService.Get returns (User, error) — no nil-User path
// today (User is a value type). Skip the user-adapter metric tick
// and document the asymmetry.
```

Skip user-side: the value-type `auth.User` has no `(nil, nil)` path to guard.

- [ ] **Step 5: Run tests + build**

```bash
go test -race -count=3 ./cmd/api/ ./internal/realtime/...
go build ./...
golangci-lint run ./...
```

All clean.

- [ ] **Step 6: Commit**

```bash
git add internal/realtime/service/metrics.go cmd/api/realtime.go cmd/api/realtime_test.go
git commit -m "$(cat <<'EOF'
feat(realtime+cmd/api): adapter-inconsistent metric for projectResolverAdapter (Plan 11.3 Task 4)

Plan 11.2 Task 5 review M-3 carry-over. projectResolverAdapter.Get
has a defensive guard for crm.ProjectService.Get returning
(nil, nil) — should never fire (ProjectService.Get returns
ErrProjectNotFound on miss), but if a service-layer regression
does break that contract, the (nil, nil) path silently surfaces
as a cross-tenant rejection in TopicRBAC and is invisible in
dashboards.

Adds realtime_resolver_adapter_inconsistent_total{adapter_type}
counter + ObserveResolverAdapterInconsistent observer on
*service.Metrics. cmd/api/realtime.go's
newProjectResolverAdapterWithMetrics threads the production
callback through; degraded boot (no service.Metrics in locator)
falls back to a no-op so the boot path doesn't error out.

User-side adapter intentionally skipped — auth.User is a value
type, no (nil, nil) defensive guard needed there.

1 test: fake ProjectService returning (nil, nil) bumps the
metric callback exactly once.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review

**1. Spec coverage:**
- Wire-string scrub on FrameSubscribeErr — ✓ Task 1.
- Cache invalidation for project lifecycle events — ✓ Tasks 2 + 3.
- Adapter-inconsistent metric — ✓ Task 4.
- Auth user-deleted invalidation — explicitly out-of-scope (deferred to a future plan when the auth module publishes the event).
- CallResolver invalidation — explicitly out-of-scope (Plan 12 dependency).

**2. Placeholder scan:**
- Every step has explicit code blocks.
- Every test step has the exact `go test ...` command.
- Every commit step has the exact `git commit -m "..."` line.
- No "TBD", "TODO", "fill in", "similar to Task N".

**3. Type consistency:**
- `rtapi.ResolvedTenant{TenantID string}` — used uniformly.
- `*service.CachedProjectResolver` returned by `service.NewCachedProjectResolver(inner, 0)` — same signature across Tasks 2 + 3 wiring.
- `crmapi.ProjectStatusChangedEvent{ProjectID uuid.UUID, TenantID uuid.UUID, ...}` — verified against the actual api/events.go.
- `events.NewCacheInvalidator(events.CacheInvalidatorConfig{...})` — config struct fields match across Tasks 3.
- `m.ObserveResolverAdapterInconsistent("project")` — exported method, label value matches the test's `require.Equal(t, "project", adapterType)`.

---

## Acceptance criteria

- All 4 tasks committed with green tests + clean lint.
- `go test -race -count=3 ./internal/realtime/... ./cmd/api/...` passes.
- `make lint` clean.
- `make test` clean.
- New metrics surfaced on `/metrics`:
  - `realtime_cache_invalidations_total{result}`
  - `realtime_resolver_adapter_inconsistent_total{adapter_type}`
- New behavioural contract: an authenticated supervisor in tenant A subscribing to `operators.state` with a foreign `project_id` from tenant B (or with a non-existent `project_id`) receives `FrameSubscribeErr{Reason: "cross-tenant subscription denied"}` regardless of underlying cause.

## Carry-overs to Plan 11.4+

- **Auth user-deleted event**: when the auth module publishes `tenant.<t>.auth.user.deleted` (Plan 11.4 candidate or part of a future auth-domain plan), extend `*CacheInvalidator` to subscribe alongside the project subject and call `userResolver.Invalidate(user_id)`.
- **CallResolver cache invalidation**: tracked alongside the CallResolver introduction in Plan 12 (recording metadata). Same pattern.
- **Cross-replica metric reconciliation**: today, each replica's resolver cache invalidates independently; if one replica's `CacheInvalidator` is down (NATS connection drop), it falls back to TTL. A future plan could add a tenant-level "force flush" admin endpoint for ops to clear caches across the fleet manually.
