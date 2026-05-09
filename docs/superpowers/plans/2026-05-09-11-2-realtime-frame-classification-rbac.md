# Plan 11.2: Realtime Frame Classification + RBAC Tenant Cross-Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close two security/quality gaps left after Plan 11 + 11.1: (1) frame-level backpressure currently drops critical billing/admin frames the same as telemetry; (2) WebSocket subscription RBAC validates roles but does **not** verify that the filter's UUIDs (`OperatorID`, `ProjectID`) actually belong to the subscriber's tenant.

**Architecture:** Two independent additions wired through `internal/realtime/api` + `service`.
- Tasks 1–2 add a `FrameClass` enum mapping `Topic → priority class` and split `Connection.Send` into a critical lane (drop = disconnect) and a telemetry lane (drop-oldest, current behaviour).
- Tasks 3–4 add tiny resolver ports (`UserResolver`, `ProjectResolver`) with a 60s LRU + singleflight cache; `TopicRBAC.Allow` consults them whenever filter UUIDs are present and returns `ErrCrossTenantSubscribe` on mismatch.
- Task 5 wires real resolvers from `auth.UserStore` + `crm.ProjectService` in `cmd/api` composition root through the locator pattern (mirrors Plan 11.1's `tenancyTenantLister`).

**Tech Stack:** Go 1.26.3, `internal/realtime`, `gin`, `zap`, `prometheus`, `golang.org/x/sync/singleflight` (already in go.mod via existing dependency tree, no new deps needed for cache de-dup).

**Out of scope (deferred):**
- **Listen-in cleanup hooks on disconnect (Plan 11 Task 10.2).** Blocks on Plan 08 (FreeSWITCH cluster) — `internal/realtime/service/listen_in.go` does not yet exist; the listen-in HTTP endpoints in Plan 11 Task 7 return `503 telephony.bridge.offline`.
- **CallResolver (third dimension of Plan 11 Task 10.3).** Blocks on Plan 12 (recording metadata) which will introduce the `recording.CallStore.Get(ctx, callID) → CallMetadata{TenantID}` shape. For now, `TopicCallEvents` cross-tenant safety is enforced upstream by:
  1. NATS subject pattern `tenant.<t>.telephony.event.<call_id>.*` — only same-tenant subjects deliver.
  2. `Hub.Broadcast` tenant filter — refuses empty TenantID, defence-in-depth (Plan 11 Task 3).
  3. The dispatcher (`events.NATSSubscriber`) projects `subject → BroadcastFilter{TenantID, CallID}`, so a cross-tenant `call_id` never leaks into a different tenant's connections.
- **Speculative new Topic constants.** The Plan 11 master draft mentioned `TopicCallFinalized`, `TopicRecordingCommitted`, `TopicQuotaBreach`, `TopicForceActionResult` — they were never landed. Plan 11.2 classifies the **six existing** topics in `api/dto.go` only; new topics will be classified by their introducing plans.

**Required reading list (skim before starting):**
- `docs/superpowers/plans/2026-05-06-11-realtime-module.md` lines 2272–2592 — Plan 11 Task 10 original draft (frame classification + listen-in cleanup + RBAC tenant cross-check).
- `docs/references/plan-11-realtime.md` — module-wide gotchas (composition root in cmd/api, dispatcher lifecycle, locator keys).
- `internal/realtime/api/dto.go` — Topic + Frame DTO + CloseReason catalogue.
- `internal/realtime/api/errors.go` — sentinel error pattern.
- `internal/realtime/service/connection.go` — current single-`sendChan` Connection + drop-oldest `Send`.
- `internal/realtime/service/rbac.go` — current `TopicRBAC.Allow` (role + selfOnly + requireCallID).
- `internal/realtime/module.go` — Module.Register order, locator pattern, optional-deps philosophy.
- `cmd/api/realtime.go` — `tenancyTenantLister` adapter pattern (Plan 11.1 Task 2; Task 5 here mirrors it for UserResolver / ProjectResolver).
- Plan 09/10/11/11.1 carry-forward rules (in `CLAUDE.md`):
  - No `init()` MustRegister — every metrics constructor takes a `prometheus.Registerer`.
  - `*zap.Logger` nil-safe — fallback to `zap.NewNop()`.
  - No `time.After` in select-loops — use `time.NewTicker` + defer Stop.
  - `wg.Go(func)` (Go 1.25+) over `wg.Add(1)+go+defer Done`.
  - Every test uses `t.Parallel()`, `t.Cleanup()`, `t.Context()` (Go 1.24+ context-helper).
  - `goleak.VerifyTestMain` in every package with goroutines.
  - Compile-time interface checks: `var _ rtapi.Hub = (*Hub)(nil)`.
  - Sentinel errors aliased to local package via `var ErrFoo = rtapi.ErrFoo` so callers can `errors.Is` without importing both.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/realtime/api/frames.go` | **NEW** | `FrameClass` enum + `TopicClass(t Topic) FrameClass` mapping function. |
| `internal/realtime/api/frames_test.go` | **NEW** | Exhaustive test for `TopicClass` over `AllTopics` + zero-value safety. |
| `internal/realtime/api/interfaces.go` | Modify | Add `UserResolver` + `ProjectResolver` ports + `ResolvedTenant` DTO. |
| `internal/realtime/api/errors.go` | Modify | Add `ErrCrossTenantSubscribe` sentinel. |
| `internal/realtime/service/connection.go` | Modify | Replace single `sendChan` with `criticalCh` (32-deep, full → close) + `telemetryCh` (cfg.WriteBufferSize-deep, drop-oldest). Priority dispatch in `runWriter`. |
| `internal/realtime/service/connection_test.go` | Modify | Update existing tests for new fields; add `TestConnection_CriticalFrameOverflowClosesConnection` + `TestConnection_TelemetryFramesDropOldest_PreservesCritical`. |
| `internal/realtime/service/resolver_cache.go` | **NEW** | `cachedResolver` wrapping a real resolver with 60s LRU + singleflight de-dup. |
| `internal/realtime/service/resolver_cache_test.go` | **NEW** | Tests: cache hit, cache miss, expiry, singleflight de-dup, ctx cancellation. |
| `internal/realtime/service/rbac.go` | Modify | `TopicRBAC` gains optional `userResolver` + `projectResolver` fields. `Allow` signature gains `ctx context.Context` + filter cross-tenant validation. New constructor `NewTopicRBACWithResolvers`. |
| `internal/realtime/service/rbac_test.go` | Modify | Add `TestTopicRBAC_RejectsCrossTenantOperatorFilter`, `TestTopicRBAC_RejectsCrossTenantProjectFilter`, `TestTopicRBAC_AllowsSameTenantFilters`, `TestTopicRBAC_AllowsZeroResolverFallback`. |
| `internal/realtime/service/hub.go` | Modify | Subscribe path passes `ctx` (the connection's run-ctx) to `rbac.Allow`. |
| `internal/realtime/service/hub_test.go` | Modify | Update tests that build `*TopicRBAC` to use `NewTopicRBAC()` (zero-resolver fallback) — no behaviour change, just signature. |
| `internal/realtime/module.go` | Modify | `Register` looks up `auth.UserStore` + `crm.ProjectService` in the locator; constructs `cachedResolver` wrappers; passes to `NewTopicRBACWithResolvers`. Empty fallbacks for missing deps (degraded test boot). |
| `cmd/api/realtime.go` | Modify | Add `userResolverAdapter` + `projectResolverAdapter` mirroring `tenancyTenantLister`. Register them in the locator under module-private keys before `realtime.Module.Register`. |

**Locator keys added** (in `internal/realtime/api/locator.go`):
- `LocatorUserResolver = "realtime.UserResolver"` — value satisfies `rtapi.UserResolver`.
- `LocatorProjectResolver = "realtime.ProjectResolver"` — value satisfies `rtapi.ProjectResolver`.

These are populated by `cmd/api` (which has visibility into both `auth.UserStore` and `crm.ProjectService`) before `realtime.Module.Register` runs, so the realtime module looks them up rather than importing auth/crm directly. This preserves the existing scope rule (`internal/realtime/` does NOT import `internal/auth/` or `internal/crm/`).

---

## Task 1: `FrameClass` enum + `TopicClass` classifier

**Files:**
- Create: `internal/realtime/api/frames.go`
- Test: `internal/realtime/api/frames_test.go`

**Background:** The current `Connection.Send` (`internal/realtime/service/connection.go:307`) uses a single `sendChan` with drop-oldest semantics for **every** topic. This means a critical `call.events` `Hangup` event (billing-relevant; UI must show call duration) is dropped with the same probability as a `dialer.queue` depth tick (next tick replaces). Plan 11 Task 10.1 calls this out as a real operational gap.

**Decision:** Critical = `TopicCallEvents` (per-call telemetry, time-sensitive for billing/UI; a missed `Hangup` leaves the operator UI showing an active call) and `TopicForceCommands` (admin-issued force-end-shift / force-pause; the operator MUST receive these or compliance fails).

**Telemetry** = the four remaining topics where the next tick replaces the previous (drop-oldest is acceptable):
- `TopicOperatorsState` — FSM transitions; the next state is the truth.
- `TopicDialerQueue` — queue depth; next tick refreshes.
- `TopicTrunksHealth` — periodic health snapshot.
- `TopicNotifications` — admin-to-user info; if a notification is critical-grade we'd send it via `TopicForceCommands` instead.

- [ ] **Step 1: Write the failing exhaustive classifier test**

`internal/realtime/api/frames_test.go`:

```go
package api_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// TestTopicClass_CriticalTopics locks in the security/billing-relevant
// topics that MUST NOT be dropped silently — overflow on these closes
// the connection so the client reconnects + re-fetches state via REST.
func TestTopicClass_CriticalTopics(t *testing.T) {
	t.Parallel()

	for _, topic := range []rtapi.Topic{
		rtapi.TopicCallEvents,
		rtapi.TopicForceCommands,
	} {
		got := rtapi.TopicClass(topic)
		assert.Equal(t, rtapi.FrameClassCritical, got,
			"topic %q must be classified Critical (drop-on-full closes connection)", topic)
	}
}

// TestTopicClass_TelemetryTopics locks in the remaining topics where
// drop-oldest is acceptable: every subsequent tick supersedes the
// previous payload, so a momentary buffer overflow is benign.
func TestTopicClass_TelemetryTopics(t *testing.T) {
	t.Parallel()

	for _, topic := range []rtapi.Topic{
		rtapi.TopicOperatorsState,
		rtapi.TopicDialerQueue,
		rtapi.TopicTrunksHealth,
		rtapi.TopicNotifications,
	} {
		got := rtapi.TopicClass(topic)
		assert.Equal(t, rtapi.FrameClassTelemetry, got,
			"topic %q must be classified Telemetry (drop-oldest)", topic)
	}
}

// TestTopicClass_AllTopicsCovered guarantees every entry in AllTopics
// has an explicit classification. A future plan that adds a new Topic
// to the registry but forgets to extend TopicClass surfaces here as a
// failure on the new topic's zero-value classification.
func TestTopicClass_AllTopicsCovered(t *testing.T) {
	t.Parallel()

	for _, topic := range rtapi.AllTopics {
		got := rtapi.TopicClass(topic)
		// FrameClassUnknown means "topic was added to AllTopics but
		// not classified" — see the doc comment on FrameClassUnknown.
		assert.NotEqual(t, rtapi.FrameClassUnknown, got,
			"topic %q in AllTopics must have an explicit FrameClass", topic)
	}
}

// TestTopicClass_ZeroValueIsUnknown verifies the package's defensive
// classification: an unrecognised topic (zero-value or arbitrary
// string) returns FrameClassUnknown so the Connection can disconnect
// rather than silently route to the wrong queue.
func TestTopicClass_ZeroValueIsUnknown(t *testing.T) {
	t.Parallel()

	assert.Equal(t, rtapi.FrameClassUnknown, rtapi.TopicClass(""))
	assert.Equal(t, rtapi.FrameClassUnknown, rtapi.TopicClass("not.a.real.topic"))
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test -run TestTopicClass ./internal/realtime/api/ -v
```

Expected: FAIL with `undefined: rtapi.TopicClass`, `undefined: rtapi.FrameClassCritical`, `undefined: rtapi.FrameClassTelemetry`, `undefined: rtapi.FrameClassUnknown`.

- [ ] **Step 3: Implement `FrameClass` + `TopicClass`**

`internal/realtime/api/frames.go`:

```go
// frames.go declares the FrameClass enum and the Topic → class map
// consumed by Connection.Send to route critical vs. telemetry frames
// onto separate per-connection queues. Critical frames overflowing
// their bounded queue close the connection (the client reconnects and
// re-fetches via REST); telemetry frames overflow into drop-oldest.
//
// Adding a new Topic constant requires extending TopicClass — the
// exhaustive test in frames_test.go enforces this.
package api

// FrameClass is the per-frame priority class consulted by
// Connection.Send to decide queue routing + overflow policy.
//
// Zero value is FrameClassUnknown on purpose: a topic that wasn't
// classified produces a deliberately observable signal (fail loud)
// instead of silently routing to the wrong lane.
type FrameClass int

const (
	// FrameClassUnknown is the zero value. Returned by TopicClass for
	// topics not in the explicit switch — Connection.Send treats this
	// as a contract violation and closes with CloseProtocolErr.
	FrameClassUnknown FrameClass = iota

	// FrameClassCritical is reserved for frames where silent drop is
	// unacceptable. The Connection routes these onto a small bounded
	// queue (criticalQueueSize=32) and closes the connection on
	// overflow with CloseRateLimited so the client reconnects and
	// re-fetches state via REST. Better an explicit reconnect than
	// quietly missing a Hangup or a force-pause command.
	FrameClassCritical

	// FrameClassTelemetry is the default class for periodic state
	// updates where the next tick supersedes the previous payload.
	// Drop-oldest is acceptable: a missed operators.state tick is
	// immediately overwritten by the following one. Connection.Send
	// routes these onto cfg.WriteBufferSize-deep telemetryCh.
	FrameClassTelemetry
)

// TopicClass returns the priority class for the supplied Topic.
//
// Critical topics:
//   - TopicCallEvents: per-call telemetry incl. Hangup; billing UI
//     must observe every event or it shows the wrong call duration.
//   - TopicForceCommands: admin-issued force-end-shift / force-pause;
//     compliance requires the operator to receive these.
//
// Telemetry topics: TopicOperatorsState, TopicDialerQueue,
// TopicTrunksHealth, TopicNotifications — periodic; next tick
// replaces.
//
// Unknown topic: returns FrameClassUnknown. Connection.Send uses this
// to fail loud (close + log) rather than silently route to telemetry.
func TopicClass(t Topic) FrameClass {
	switch t {
	case TopicCallEvents, TopicForceCommands:
		return FrameClassCritical
	case TopicOperatorsState, TopicDialerQueue, TopicTrunksHealth, TopicNotifications:
		return FrameClassTelemetry
	default:
		return FrameClassUnknown
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test -run TestTopicClass ./internal/realtime/api/ -v
```

Expected: PASS for all four sub-tests.

- [ ] **Step 5: Commit**

```bash
git add internal/realtime/api/frames.go internal/realtime/api/frames_test.go
git commit -m "feat(realtime/api): FrameClass enum + Topic→class classifier (Plan 11.2 Task 1)"
```

---

## Task 2: `Connection.Send` dual-queue routing

**Files:**
- Modify: `internal/realtime/service/connection.go` (lines 86–143 add fields, 307–336 rewrite Send, 616–638 update runWriter, ~150 LOC change)
- Modify: `internal/realtime/service/connection_test.go` (update existing send-test helpers, add 2 new tests)

**Background:** Plan 11 Task 2 shipped a single `sendChan` with drop-oldest. Task 1 (above) classified topics; this task threads the classification through the actual queue + writer.

**Lock-discipline note:** The existing `Send` is lock-free (uses select-on-channel). The new dual-queue version stays lock-free — `criticalCh` and `telemetryCh` are independent buffered channels; the writer drains them with a priority select.

**Critical-overflow policy:** When `criticalCh` is full, `Send` calls `Close(CloseRateLimited)` and returns. We use `CloseRateLimited` (4429 — already in `dto.go`) rather than `ClosePolicyViol` (1008) because the semantic match is "you sent more than the connection can absorb" — same family as the existing rate-limit-exceeded close in `runReader:526`. The client reconnects and re-fetches via REST — better than silently dropping a billing event.

- [ ] **Step 1: Write the failing critical-overflow test**

Add to `internal/realtime/service/connection_test.go`:

```go
// TestConnection_CriticalFrameOverflowClosesConnection asserts that a
// blocked writer + a sustained burst of critical frames closes the
// connection with CloseRateLimited rather than silently dropping —
// the documented Plan 11.2 contract for FrameClassCritical.
func TestConnection_CriticalFrameOverflowClosesConnection(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		WriteBufferSize: 16, // telemetry buffer; critical buffer is fixed at 32
	})
	conn.SeedClaims(rtapi.Claims{UserID: "u1", TenantID: "t1", Roles: []string{"admin"}})

	// Block the writer so the queues fill up.
	fake.BlockWrites()

	// Push 50 critical frames; criticalQueueSize=32 — frame 33 should
	// trigger the overflow-close path.
	for i := 0; i < 50; i++ {
		conn.Send(rtapi.Frame{Type: rtapi.FrameEvent, Topic: rtapi.TopicCallEvents})
	}

	require.Eventually(t, conn.IsClosedForTest,
		2*time.Second, 10*time.Millisecond,
		"critical-queue overflow must close the connection")

	// Unblock so writer goroutine drains and exits cleanly (no goleak).
	fake.UnblockWrites()
}

// TestConnection_TelemetryFramesDropOldest_PreservesCritical asserts
// that telemetry overflow does NOT close the connection AND does not
// purge frames from the critical queue. The two queues are
// independent.
func TestConnection_TelemetryFramesDropOldest_PreservesCritical(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		WriteBufferSize: 4, // tiny telemetry buffer; force overflow fast
	})
	conn.SeedClaims(rtapi.Claims{UserID: "u1", TenantID: "t1", Roles: []string{"operator"}})

	fake.BlockWrites()

	// Push one critical frame first — it should land in the critical
	// queue and survive the subsequent telemetry-flood.
	conn.Send(rtapi.Frame{Type: rtapi.FrameEvent, Topic: rtapi.TopicCallEvents})

	// Flood the telemetry queue; drop-oldest must NOT close the conn.
	for i := 0; i < 20; i++ {
		conn.Send(rtapi.Frame{Type: rtapi.FrameEvent, Topic: rtapi.TopicOperatorsState})
	}

	// Sanity: connection still alive (no overflow-close from telemetry).
	require.False(t, conn.IsClosedForTest(),
		"telemetry-queue overflow must NOT close the connection")

	// At least one frame was dropped (drop-oldest counter incremented).
	assert.Positive(t, conn.DroppedFrames(),
		"telemetry overflow should bump drop counter")

	fake.UnblockWrites()
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test -race -run 'TestConnection_(CriticalFrameOverflow|TelemetryFramesDropOldest_PreservesCritical)' ./internal/realtime/service/ -v
```

Expected: FAIL — current `Send` has a single `sendChan` with drop-oldest for everything; critical frames get dropped silently and the connection stays open.

- [ ] **Step 3: Refactor `Connection` fields and `Send`**

In `internal/realtime/service/connection.go`, replace the `sendChan chan rtapi.Frame` field (line 105–106) and add a sibling:

```go
// criticalQueueSize is the bounded depth for the critical lane.
// Smaller than the telemetry buffer because critical frames overflow
// to a connection close (CloseRateLimited) — we don't want a runaway
// publisher to amass minutes of buffered work before noticing.
const criticalQueueSize = 32
```

Add new fields next to the existing send chan (Plan 11 Task 2 fields preserved verbatim except for `sendChan`):

```go
	// criticalCh routes FrameClassCritical frames onto a small
	// bounded queue (criticalQueueSize=32). Overflow closes the
	// connection with CloseRateLimited so the client reconnects and
	// re-fetches via REST — silent drop on critical frames would
	// leave the operator UI in a stale state (e.g., showing an
	// active call after the Hangup).
	criticalCh chan rtapi.Frame

	// telemetryCh is the drop-oldest lane carrying every other
	// frame. Bounded by cfg.WriteBufferSize. A full buffer triggers
	// the same drop-oldest replacement Plan 11 Task 2 shipped — the
	// next tick supersedes the discarded one.
	telemetryCh chan rtapi.Frame
```

Replace the `NewConnection` initialiser block (line 169 `sendChan: make(...)`) with both channels:

```go
		criticalCh:  make(chan rtapi.Frame, criticalQueueSize),
		telemetryCh: make(chan rtapi.Frame, cfg.WriteBufferSize),
```

Replace the `Send` method (lines 296–336) with the classified version:

```go
// Send queues frame for delivery on the writer goroutine. Frame
// routing is determined by rtapi.TopicClass(frame.Topic):
//
//   - FrameClassCritical: enqueued on criticalCh. If full, the
//     connection is closed with CloseRateLimited and Send returns —
//     silent drop is unacceptable for billing/admin frames.
//   - FrameClassTelemetry: enqueued on telemetryCh with drop-oldest
//     replacement (Plan 11 Task 2 behaviour preserved).
//   - FrameClassUnknown: a contract-violating topic (not in the
//     explicit switch in api.TopicClass). Closes with CloseProtocolErr
//     so the wiring bug surfaces immediately rather than the frame
//     getting silently misrouted.
//
// Send on a closed connection is a no-op — callers race the close
// path during teardown and shouldn't crash. Control frames
// (FramePing/FramePong/FrameAuthOK/FrameRefreshOK/FrameSubscribeOK/
// FrameSubscribeErr/FrameAuthError) carry an empty Topic — these
// frames bypass the classifier and route to telemetryCh
// (drop-oldest), matching the prior Plan 11 Task 2 semantics for
// non-Topic'd frames.
func (c *Connection) Send(frame rtapi.Frame) {
	if c.closed.Load() {
		return
	}
	// Control frames (auth.ok / refresh.ok / ping / pong / subscribe.ok /
	// subscribe.error / auth.error) carry an empty Topic. Route them via
	// the telemetry lane to preserve Plan 11 Task 2 drop-oldest
	// semantics for non-event traffic.
	if frame.Topic == "" {
		c.sendTelemetry(frame)
		return
	}

	switch rtapi.TopicClass(frame.Topic) {
	case rtapi.FrameClassCritical:
		c.sendCritical(frame)
	case rtapi.FrameClassTelemetry:
		c.sendTelemetry(frame)
	default:
		// FrameClassUnknown — wiring bug. Fail loud rather than
		// route to the wrong lane.
		c.cfg.Logger.Error("realtime: send frame with unclassified topic; closing",
			zap.String("conn_id", c.id),
			zap.String("topic", string(frame.Topic)),
		)
		c.metrics.observeUnknownTopicClass(string(frame.Topic))
		c.Close(rtapi.CloseProtocolErr)
	}
}

// sendCritical enqueues frame on criticalCh. On a full queue the
// connection is closed — see Send for the rationale.
func (c *Connection) sendCritical(frame rtapi.Frame) {
	select {
	case c.criticalCh <- frame:
		return
	default:
	}
	// criticalQueueSize is full → close the connection so the
	// client reconnects and re-fetches via REST. metric tick before
	// Close so the observability path stays intact even on a fast
	// teardown.
	c.metrics.observeCriticalOverflow(c.id)
	c.cfg.Logger.Warn("realtime: critical-queue overflow; closing",
		zap.String("conn_id", c.id),
	)
	c.Close(rtapi.CloseRateLimited)
}

// sendTelemetry enqueues frame on telemetryCh with drop-oldest
// replacement (Plan 11 Task 2 behaviour).
func (c *Connection) sendTelemetry(frame rtapi.Frame) {
	// Fast path: room in buffer.
	select {
	case c.telemetryCh <- frame:
		return
	default:
	}
	// Slow consumer — drop oldest.
	select {
	case <-c.telemetryCh:
		c.droppedFrames.Add(1)
		c.metrics.observeDrop(c.id)
	default:
		// Channel went from full → drained between selects. Fall
		// through and try to enqueue.
	}
	select {
	case c.telemetryCh <- frame:
	default:
		// Channel went from full → empty → full between selects
		// (receiver was draining concurrently). Drop the new frame;
		// the receiver is still consuming so the remaining queue is
		// fresh.
		c.droppedFrames.Add(1)
		c.metrics.observeDrop(c.id)
	}
}
```

- [ ] **Step 4: Update `runWriter` for priority dispatch**

Replace `runWriter` (lines 616–638) with:

```go
// runWriter is the SOLE owner of conn.WriteFrame. It pulls frames
// off criticalCh (priority lane) and telemetryCh (drop-oldest lane)
// and writes them with a per-frame WriteTimeout.
//
// Priority discipline: an outer non-blocking receive on criticalCh
// drains every pending critical frame before falling through to a
// blocking select that waits on either channel. This ensures a
// telemetry storm cannot starve critical frames AND that an idle
// writer parks on a single select rather than busy-spinning.
//
// On write error the writer signals close and exits.
func (c *Connection) runWriter(ctx context.Context) {
	for {
		// Priority drain: serve critical frames before telemetry
		// when both are ready. Non-blocking; no busy-loop because
		// the default arm falls through to the blocking select
		// below.
		select {
		case <-c.closeChan:
			return
		case <-ctx.Done():
			c.Close(rtapi.CloseGoingAway)
			return
		case frame := <-c.criticalCh:
			if !c.writeOne(ctx, frame) {
				return
			}
			continue
		default:
		}

		// Both queues empty (or only telemetry has work). Park on a
		// blocking select that wakes for either lane OR
		// shutdown/ctx-cancel.
		select {
		case <-c.closeChan:
			return
		case <-ctx.Done():
			c.Close(rtapi.CloseGoingAway)
			return
		case frame := <-c.criticalCh:
			if !c.writeOne(ctx, frame) {
				return
			}
		case frame := <-c.telemetryCh:
			if !c.writeOne(ctx, frame) {
				return
			}
		}
	}
}

// writeOne writes a single frame with the configured WriteTimeout and
// returns true on success / false on error (caller exits the writer
// loop). Pulled out of runWriter so the priority + blocking selects
// don't duplicate the marshal-and-write boilerplate.
func (c *Connection) writeOne(ctx context.Context, frame rtapi.Frame) bool {
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	if err := c.writeFrameSync(wctx, frame); err != nil {
		c.cfg.Logger.Warn("realtime: write failed",
			zap.String("conn_id", c.id),
			zap.Error(err),
		)
		c.Close(rtapi.CloseGoingAway)
		return false
	}
	return true
}
```

- [ ] **Step 5: Update `Metrics` for new counters**

In `internal/realtime/service/metrics.go`, add two metric observers next to the existing `observeDrop`:

```go
// observeCriticalOverflow ticks when a critical frame can't fit on
// criticalCh and the connection is closed as a result. nil-safe.
func (m *Metrics) observeCriticalOverflow(connID string) {
	if m == nil {
		return
	}
	m.criticalOverflows.WithLabelValues(connID).Inc()
}

// observeUnknownTopicClass ticks when Connection.Send is called with
// a topic that has no FrameClass mapping. Cardinality bounded by the
// `topic` label which is checked against AllTopics in the wiring
// path; an unbounded payload-string-as-topic would surface as the
// connection being closed with CloseProtocolErr — see Send.
func (m *Metrics) observeUnknownTopicClass(topic string) {
	if m == nil {
		return
	}
	m.unknownTopicClasses.WithLabelValues(topic).Inc()
}
```

And add fields + registration in `RegisterMetrics`:

```go
type Metrics struct {
	// ... existing fields ...
	criticalOverflows   *prometheus.CounterVec
	unknownTopicClasses *prometheus.CounterVec
}

func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("realtime/service: RegisterMetrics: nil registerer")
	}
	m := &Metrics{
		// ... existing collectors ...
		criticalOverflows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "realtime_critical_overflows_total",
			Help: "Number of WS connections closed due to critical-queue overflow.",
		}, []string{"conn_id"}),
		unknownTopicClasses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "realtime_unknown_topic_classes_total",
			Help: "Number of Send() calls with a topic missing FrameClass mapping (wiring bug indicator).",
		}, []string{"topic"}),
	}
	reg.MustRegister(
		// ... existing collectors ...
		m.criticalOverflows,
		m.unknownTopicClasses,
	)
	return m
}
```

(Read the actual `metrics.go` first; the existing field/collector names and patterns must be preserved verbatim. The two snippets above show the additions only.)

- [ ] **Step 6: Run all connection tests with -race**

```bash
go test -race -count=3 ./internal/realtime/service/ -v -run 'TestConnection'
```

Expected: PASS for both new tests AND every existing connection test (drop-oldest preservation, idempotent Close, AuthHandshake, runReader/runWriter/runPinger).

- [ ] **Step 7: Commit**

```bash
git add internal/realtime/service/connection.go internal/realtime/service/connection_test.go internal/realtime/service/metrics.go
git commit -m "feat(realtime/service): dual-queue Connection.Send (critical + telemetry) (Plan 11.2 Task 2)"
```

---

## Task 3: Resolver ports + 60s LRU cache + singleflight de-dup

**Files:**
- Modify: `internal/realtime/api/interfaces.go` (add `UserResolver`, `ProjectResolver`, `ResolvedTenant`)
- Modify: `internal/realtime/api/errors.go` (add `ErrCrossTenantSubscribe`)
- Modify: `internal/realtime/api/locator.go` (add `LocatorUserResolver`, `LocatorProjectResolver`)
- Create: `internal/realtime/service/resolver_cache.go`
- Create: `internal/realtime/service/resolver_cache_test.go`

**Background:** Cross-tenant cross-check (Task 4) needs to ask "does this UUID belong to this tenant?" without a per-frame DB hit. The resolver port is intentionally narrow (`Get(ctx, id) → ResolvedTenant{TenantID}`) so production wiring can swap a real `auth.UserStore` lookup for a stub in tests, and a `cachedResolver` in front absorbs the load.

**Cache design:**
- `sync.Map` keyed by entity ID string, value `*entry{TenantID, expiresAt}`.
- 60s lazy expiry (checked on read).
- `singleflight.Group` coalesces concurrent misses for the same ID into one inner call (avoids N concurrent identical DB hits when N WS clients subscribe simultaneously after a deploy).
- No size cap — the working set is O(active users + active projects), bounded by tenancy contracts.
- Cache is **not** invalidated on user/project deletion. The 60s TTL is the upper bound on stale-entry visibility; for security-sensitive cases (deleted user re-subscribing) the entry was authorised at the moment it was issued anyway, and the JWT itself expires within minutes.

- [ ] **Step 1: Add resolver ports + sentinel + locator keys**

`internal/realtime/api/interfaces.go` (append after `ListenInService`):

```go
// ResolvedTenant is the projection of an entity (user, project, call)
// that resolver ports return. The TenantID field is the cross-check
// target: TopicRBAC.Allow rejects a subscription when the resolved
// TenantID does not match the subscriber's claims.TenantID.
type ResolvedTenant struct {
	// TenantID is the entity's owning tenant. Returned as a string to
	// match the existing realtime/api convention (Claims.TenantID is a
	// string; Hub.Broadcast filters use string TenantID).
	TenantID string
}

// UserResolver maps a user_id to its tenant. Used by TopicRBAC.Allow
// to reject `notifications.user` / `op.commands` subscriptions whose
// filter.OperatorID belongs to a different tenant than the
// subscriber's claims.
//
// Implementations MUST return ErrNotFound (or a wrapped form callers
// can errors.Is) when the user does not exist; TopicRBAC treats
// not-found the same as cross-tenant — both are a "you cannot
// subscribe" signal — so the wire response is identical and the
// client can't probe user existence cross-tenant.
type UserResolver interface {
	// Get resolves user_id to its owning tenant. Returns
	// ErrCrossTenantSubscribe (or any error) when the user is not
	// resolvable. ctx-aware so the realtime layer can bound the
	// resolve under its handshake/subscribe deadline.
	Get(ctx context.Context, userID string) (ResolvedTenant, error)
}

// ProjectResolver maps a project_id to its tenant. Used by
// TopicRBAC.Allow to reject `operators.state` subscriptions whose
// filter.ProjectID belongs to a different tenant.
//
// Same not-found semantics as UserResolver — the realtime layer
// folds not-found into cross-tenant rejection.
type ProjectResolver interface {
	// Get resolves project_id to its owning tenant. Returns an
	// error when the project is not resolvable.
	Get(ctx context.Context, projectID string) (ResolvedTenant, error)
}
```

`internal/realtime/api/errors.go` (append):

```go
	// ErrCrossTenantSubscribe is returned when TopicRBAC.Allow detects
	// that a SubscriptionFilter UUID (OperatorID, ProjectID) belongs
	// to a tenant other than the subscriber's claims.TenantID. Defence-
	// in-depth — Hub.Broadcast already filters by tenant, but a
	// cross-tenant subscribe attempt is a security signal we want to
	// surface (the subscriber should never have observed the foreign
	// UUID in the first place).
	ErrCrossTenantSubscribe = errors.New("realtime: cross-tenant subscription denied")
```

`internal/realtime/api/locator.go` (append two new constants):

```go
	// LocatorUserResolver is the locator key for the realtime
	// UserResolver. cmd/api populates this BEFORE realtime.Module.Register
	// runs by adapting auth.UserStore — preserves the scope rule that
	// internal/realtime/* does not import internal/auth/*.
	LocatorUserResolver = "realtime.UserResolver"

	// LocatorProjectResolver is the locator key for the realtime
	// ProjectResolver. cmd/api adapts crm.ProjectService → ProjectResolver
	// and registers it under this key BEFORE realtime.Module.Register.
	LocatorProjectResolver = "realtime.ProjectResolver"
```

- [ ] **Step 2: Write the failing cache test**

`internal/realtime/service/resolver_cache_test.go`:

```go
package service_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// stubUserResolver counts inner calls; the cache wrapper should
// coalesce concurrent misses into one call and serve subsequent hits
// from the cache for ttl.
type stubUserResolver struct {
	calls atomic.Int64
	mu    sync.Mutex
	data  map[string]string // userID → tenantID
	err   error             // forced error path
}

func newStubUserResolver(data map[string]string) *stubUserResolver {
	return &stubUserResolver{data: data}
}

func (s *stubUserResolver) Get(_ context.Context, userID string) (rtapi.ResolvedTenant, error) {
	s.calls.Add(1)
	if s.err != nil {
		return rtapi.ResolvedTenant{}, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tid, ok := s.data[userID]
	if !ok {
		return rtapi.ResolvedTenant{}, errors.New("not found")
	}
	return rtapi.ResolvedTenant{TenantID: tid}, nil
}

func (s *stubUserResolver) Calls() int64 { return s.calls.Load() }

// TestCachedUserResolver_HitServesFromCache verifies repeated Get
// calls within the TTL hit the cache (one inner call total).
func TestCachedUserResolver_HitServesFromCache(t *testing.T) {
	t.Parallel()

	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	cached := service.NewCachedUserResolver(stub, 60*time.Second, zaptest.NewLogger(t))

	for i := 0; i < 5; i++ {
		got, err := cached.Get(t.Context(), "u1")
		require.NoError(t, err)
		assert.Equal(t, "t1", got.TenantID)
	}
	assert.EqualValues(t, 1, stub.Calls(),
		"5 concurrent reads of the same key must coalesce to 1 inner call")
}

// TestCachedUserResolver_ExpiryReFetches verifies the TTL: after
// expiry, the next Get hits the inner resolver again.
func TestCachedUserResolver_ExpiryReFetches(t *testing.T) {
	t.Parallel()

	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	// 50ms TTL so the test is fast.
	cached := service.NewCachedUserResolver(stub, 50*time.Millisecond, zaptest.NewLogger(t))

	_, err := cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stub.Calls())

	time.Sleep(80 * time.Millisecond)

	_, err = cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 2, stub.Calls(),
		"after TTL elapses, Get must re-query the inner resolver")
}

// TestCachedUserResolver_SingleflightCoalescesConcurrentMisses
// drives N goroutines hitting the same uncached key simultaneously.
// The singleflight wrapper must coalesce them into one inner call.
func TestCachedUserResolver_SingleflightCoalescesConcurrentMisses(t *testing.T) {
	t.Parallel()

	const N = 32

	// Slow-down stub: each inner call sleeps 50ms so concurrent
	// callers reliably overlap.
	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	slowStub := &slowResolver{inner: stub, delay: 50 * time.Millisecond}
	cached := service.NewCachedUserResolver(slowStub, 60*time.Second, zaptest.NewLogger(t))

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cached.Get(t.Context(), "u1")
			assert.NoError(t, err)
			assert.Equal(t, "t1", got.TenantID)
		}()
	}
	wg.Wait()

	assert.EqualValues(t, 1, stub.Calls(),
		"singleflight must coalesce N concurrent misses to 1 inner call")
}

// slowResolver wraps a UserResolver and delays Get by `delay` —
// used by the singleflight test to force concurrent goroutines to
// overlap inside the inner call.
type slowResolver struct {
	inner *stubUserResolver
	delay time.Duration
}

func (s *slowResolver) Get(ctx context.Context, userID string) (rtapi.ResolvedTenant, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return rtapi.ResolvedTenant{}, ctx.Err()
	}
	return s.inner.Get(ctx, userID)
}

// TestCachedUserResolver_CtxCancelPropagates verifies a cancelled ctx
// surfaces ctx.Err() rather than blocking on the inner resolver
// indefinitely.
func TestCachedUserResolver_CtxCancelPropagates(t *testing.T) {
	t.Parallel()

	stub := &slowResolver{
		inner: newStubUserResolver(map[string]string{"u1": "t1"}),
		delay: 5 * time.Second,
	}
	cached := service.NewCachedUserResolver(stub, 60*time.Second, zaptest.NewLogger(t))

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before call

	_, err := cached.Get(ctx, "u1")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestCachedUserResolver_NewWithNilInnerPanics is the wiring guard.
func TestCachedUserResolver_NewWithNilInnerPanics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t,
		"service.NewCachedUserResolver: inner must be non-nil",
		func() {
			_ = service.NewCachedUserResolver(nil, 60*time.Second, zaptest.NewLogger(t))
		})
}

// (Mirror tests for CachedProjectResolver — same shape, swap the type.)
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test -race -run 'TestCachedUserResolver' ./internal/realtime/service/ -v
```

Expected: FAIL — `service.NewCachedUserResolver` is undefined.

- [ ] **Step 4: Implement the cache wrapper**

`internal/realtime/service/resolver_cache.go`:

```go
// resolver_cache.go provides a 60s LRU + singleflight wrapper around
// rtapi.UserResolver and rtapi.ProjectResolver. The wrapper absorbs
// the per-frame load that TopicRBAC.Allow would otherwise generate
// on every WS subscribe — production users + projects are O(thousands)
// per tenant and a hot operator UI subscribes to several topics on
// connect.
//
// Why singleflight: a deploy + N WS reconnects produces an N-way
// concurrent miss for the same (user_id, project_id) pair. Without
// coalescing the inner resolver fields N parallel DB hits.
//
// Cache invalidation: there is none. Stale entries TTL out within
// 60s; a deleted user's stale TenantID still validates the in-flight
// JWT (which itself expires within minutes), so the security
// envelope is bounded by the JWT lifetime + cache TTL.
package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// cachedResolverEntry is the cache value: the resolved TenantID + an
// expiry deadline checked lazily on read.
type cachedResolverEntry struct {
	tenant    rtapi.ResolvedTenant
	expiresAt time.Time
}

// CachedUserResolver wraps a rtapi.UserResolver with a 60s sync.Map
// cache + a singleflight.Group for concurrent-miss coalescing.
//
// Zero-value not safe — callers must use NewCachedUserResolver. nil
// inner panics at construction time so the wiring bug surfaces at
// boot rather than first subscribe.
type CachedUserResolver struct {
	inner  rtapi.UserResolver
	ttl    time.Duration
	logger *zap.Logger

	cache sync.Map // userID string → *cachedResolverEntry
	group singleflight.Group
}

// NewCachedUserResolver wires a CachedUserResolver. ttl ≤ 0 falls
// back to defaultResolverTTL (60s). logger nil-safe.
func NewCachedUserResolver(inner rtapi.UserResolver, ttl time.Duration, logger *zap.Logger) *CachedUserResolver {
	if inner == nil {
		panic("service.NewCachedUserResolver: inner must be non-nil")
	}
	if ttl <= 0 {
		ttl = defaultResolverTTL
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &CachedUserResolver{
		inner:  inner,
		ttl:    ttl,
		logger: logger,
	}
}

// defaultResolverTTL is the fallback cache window. Picked so a
// short-lived JWT (default 15min) re-validates at most 15 times,
// keeping the inner resolver load bounded under reconnect storms.
const defaultResolverTTL = 60 * time.Second

// Get resolves userID via the cache, coalescing concurrent misses
// via singleflight. ctx propagates to the inner resolver and to the
// singleflight Do call so a cancelled subscribe doesn't block on a
// slow DB.
func (c *CachedUserResolver) Get(ctx context.Context, userID string) (rtapi.ResolvedTenant, error) {
	// Fast path: cache hit + not expired.
	if v, ok := c.cache.Load(userID); ok {
		entry := v.(*cachedResolverEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.tenant, nil
		}
		// Expired — fall through to refetch via singleflight.
	}

	// Slow path: miss or expired. Coalesce concurrent calls for the
	// same userID via singleflight.DoChan + select on ctx so a slow
	// inner resolver doesn't pin the caller.
	ch := c.group.DoChan(userID, func() (any, error) {
		// singleflight invokes this once per key; subsequent
		// concurrent callers wait on the result.
		got, err := c.inner.Get(ctx, userID)
		if err != nil {
			return rtapi.ResolvedTenant{}, err
		}
		entry := &cachedResolverEntry{
			tenant:    got,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.cache.Store(userID, entry)
		return got, nil
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return rtapi.ResolvedTenant{}, res.Err
		}
		return res.Val.(rtapi.ResolvedTenant), nil
	case <-ctx.Done():
		// Forget the in-flight call so a subsequent retry doesn't
		// inherit this caller's cancelled-ctx error from
		// singleflight's caching of the result.
		c.group.Forget(userID)
		return rtapi.ResolvedTenant{}, ctx.Err()
	}
}

// CachedProjectResolver mirrors CachedUserResolver for project IDs.
// Behaviour identical; separate type so the resolver-port type
// safety is preserved at call sites.
type CachedProjectResolver struct {
	inner  rtapi.ProjectResolver
	ttl    time.Duration
	logger *zap.Logger

	cache sync.Map
	group singleflight.Group
}

// NewCachedProjectResolver wires a CachedProjectResolver. Same
// invariants as NewCachedUserResolver.
func NewCachedProjectResolver(inner rtapi.ProjectResolver, ttl time.Duration, logger *zap.Logger) *CachedProjectResolver {
	if inner == nil {
		panic("service.NewCachedProjectResolver: inner must be non-nil")
	}
	if ttl <= 0 {
		ttl = defaultResolverTTL
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &CachedProjectResolver{
		inner:  inner,
		ttl:    ttl,
		logger: logger,
	}
}

// Get is the project-id mirror of CachedUserResolver.Get.
func (c *CachedProjectResolver) Get(ctx context.Context, projectID string) (rtapi.ResolvedTenant, error) {
	if v, ok := c.cache.Load(projectID); ok {
		entry := v.(*cachedResolverEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.tenant, nil
		}
	}
	ch := c.group.DoChan(projectID, func() (any, error) {
		got, err := c.inner.Get(ctx, projectID)
		if err != nil {
			return rtapi.ResolvedTenant{}, err
		}
		entry := &cachedResolverEntry{
			tenant:    got,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.cache.Store(projectID, entry)
		return got, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return rtapi.ResolvedTenant{}, res.Err
		}
		return res.Val.(rtapi.ResolvedTenant), nil
	case <-ctx.Done():
		c.group.Forget(projectID)
		return rtapi.ResolvedTenant{}, ctx.Err()
	}
}

// Compile-time interface checks. Keeping these next to the
// implementations means a port signature change breaks the build at
// the cache wrapper, not far away in TopicRBAC.
var (
	_ rtapi.UserResolver    = (*CachedUserResolver)(nil)
	_ rtapi.ProjectResolver = (*CachedProjectResolver)(nil)

	// errCacheNotInitialised is the sentinel a future Reset/Clear
	// helper would return; reserved here so the package error
	// catalogue doesn't grow as features are added.
	errCacheNotInitialised = errors.New("realtime/service: resolver cache not initialised")
)

// Suppress the unused-var lint: errCacheNotInitialised is reserved
// for a future Reset/Clear helper.
var _ = errCacheNotInitialised
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test -race -count=3 -run 'TestCachedUserResolver|TestCachedProjectResolver' ./internal/realtime/service/ -v
```

Expected: PASS for every sub-test (cache hit, expiry, singleflight, ctx-cancel, nil-panic).

- [ ] **Step 6: Commit**

```bash
git add internal/realtime/api/interfaces.go internal/realtime/api/errors.go internal/realtime/api/locator.go internal/realtime/service/resolver_cache.go internal/realtime/service/resolver_cache_test.go
git commit -m "feat(realtime): UserResolver + ProjectResolver ports + 60s LRU/singleflight cache (Plan 11.2 Task 3)"
```

---

## Task 4: `TopicRBAC.Allow` tenant cross-check

**Files:**
- Modify: `internal/realtime/service/rbac.go` (Allow signature gains ctx, fields gain resolvers, new constructor)
- Modify: `internal/realtime/service/rbac_test.go` (4 new tests + signature update)
- Modify: `internal/realtime/service/hub.go` (Subscribe path passes ctx to Allow)
- Modify: `internal/realtime/service/hub_test.go` (signature update)

**Background:** `TopicRBAC.Allow` (`internal/realtime/service/rbac.go:101`) currently checks role + selfOnly + requireCallID but NOT that filter UUIDs belong to the subscriber's tenant. A malicious operator-role token from tenant A who learns a ProjectID from tenant B could subscribe to `operators.state` with that filter — `Hub.Broadcast` would NOT deliver (the broadcast filter scopes by claims.TenantID), but the **subscription itself** is a security signal: the attacker has confirmed the project exists.

**Decision:** `Allow` resolves filter.OperatorID + filter.ProjectID through the resolvers when set. Empty filters skip the check (other RBAC rules already cover empty-filter cases). Resolver errors fold into `ErrCrossTenantSubscribe` so the wire response identical regardless of "not found" vs. "wrong tenant" — the client cannot probe entity existence cross-tenant.

**Backwards-compat:** `NewTopicRBAC()` continues to work (zero-resolver fallback preserves Plan 11 behaviour for tests + degraded boot). `NewTopicRBACWithResolvers()` is the new constructor used by Module.Register in production.

- [ ] **Step 1: Write the failing cross-tenant tests**

Add to `internal/realtime/service/rbac_test.go`:

```go
// stubResolver returns a fixed TenantID for any input. Used in the
// cross-tenant tests below; the real CachedUserResolver wires this
// through to auth.UserStore in production.
type stubResolver struct {
	tenantByID map[string]string
}

func (s *stubResolver) Get(_ context.Context, id string) (rtapi.ResolvedTenant, error) {
	tid, ok := s.tenantByID[id]
	if !ok {
		return rtapi.ResolvedTenant{}, errors.New("not found")
	}
	return rtapi.ResolvedTenant{TenantID: tid}, nil
}

// TestTopicRBAC_RejectsCrossTenantOperatorFilter verifies that a
// supervisor in tenant A subscribing to operators.state with an
// OperatorID belonging to tenant B is rejected with
// ErrCrossTenantSubscribe.
func TestTopicRBAC_RejectsCrossTenantOperatorFilter(t *testing.T) {
	t.Parallel()

	users := &stubResolver{tenantByID: map[string]string{
		"victim-op": "tenant-B", // foreign tenant
	}}
	rbac := service.NewTopicRBACWithResolvers(users, nil)

	claims := rtapi.Claims{
		UserID:   "attacker",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}
	filter := rtapi.SubscriptionFilter{OperatorID: "victim-op"}

	err := rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState, filter)
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrCrossTenantSubscribe,
		"cross-tenant OperatorID must reject with ErrCrossTenantSubscribe")
}

// TestTopicRBAC_RejectsCrossTenantProjectFilter mirrors the operator
// case for project_id filters.
func TestTopicRBAC_RejectsCrossTenantProjectFilter(t *testing.T) {
	t.Parallel()

	projects := &stubResolver{tenantByID: map[string]string{
		"foreign-project": "tenant-B",
	}}
	rbac := service.NewTopicRBACWithResolvers(nil, projects)

	claims := rtapi.Claims{
		UserID:   "u1",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}
	filter := rtapi.SubscriptionFilter{ProjectID: "foreign-project"}

	err := rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState, filter)
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrCrossTenantSubscribe)
}

// TestTopicRBAC_AllowsSameTenantFilters is the happy path: filter
// UUIDs whose TenantID matches the subscriber's claims pass.
func TestTopicRBAC_AllowsSameTenantFilters(t *testing.T) {
	t.Parallel()

	users := &stubResolver{tenantByID: map[string]string{
		"my-op": "tenant-A",
	}}
	projects := &stubResolver{tenantByID: map[string]string{
		"my-project": "tenant-A",
	}}
	rbac := service.NewTopicRBACWithResolvers(users, projects)

	claims := rtapi.Claims{
		UserID:   "supervisor",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}

	require.NoError(t, rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState,
		rtapi.SubscriptionFilter{OperatorID: "my-op", ProjectID: "my-project"}))
}

// TestTopicRBAC_AllowsZeroResolverFallback verifies that the legacy
// NewTopicRBAC() (no resolvers) preserves Plan 11 behaviour: the
// cross-tenant check is skipped entirely when resolvers are absent.
// This guards the test/degraded-boot path where wiring real resolvers
// is not desirable.
func TestTopicRBAC_AllowsZeroResolverFallback(t *testing.T) {
	t.Parallel()

	rbac := service.NewTopicRBAC() // legacy constructor

	claims := rtapi.Claims{
		UserID:   "u1",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}
	// Foreign-looking UUIDs — Allow must NOT reject because the
	// resolver wasn't wired.
	filter := rtapi.SubscriptionFilter{
		OperatorID: "this-could-be-any-tenant",
		ProjectID:  "another-arbitrary-id",
	}

	require.NoError(t, rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState, filter))
}

// TestTopicRBAC_FoldsResolverErrorIntoCrossTenant is the security
// guarantee: a resolver error (not-found, DB error) MUST surface as
// ErrCrossTenantSubscribe so the wire response is indistinguishable
// from a real cross-tenant attempt — the client cannot probe entity
// existence.
func TestTopicRBAC_FoldsResolverErrorIntoCrossTenant(t *testing.T) {
	t.Parallel()

	users := &stubResolver{tenantByID: map[string]string{}} // empty → all lookups fail
	rbac := service.NewTopicRBACWithResolvers(users, nil)

	claims := rtapi.Claims{
		UserID:   "u1",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}
	err := rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState,
		rtapi.SubscriptionFilter{OperatorID: "nonexistent"})
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrCrossTenantSubscribe,
		"resolver error must fold into ErrCrossTenantSubscribe (don't leak entity existence)")
}
```

You'll also need imports at top of the file (preserve existing imports first, then add):

```go
import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test -race -run 'TestTopicRBAC_(RejectsCrossTenant|AllowsSameTenant|AllowsZeroResolverFallback|FoldsResolverError)' ./internal/realtime/service/ -v
```

Expected: FAIL — `service.NewTopicRBACWithResolvers` is undefined; `service.ErrCrossTenantSubscribe` is undefined; `Allow` doesn't take ctx.

- [ ] **Step 3: Update `TopicRBAC` struct + Allow signature**

In `internal/realtime/service/rbac.go`, replace the file contents (keeping the package + imports + sentinel aliases at the top):

```go
package service

import (
	"context"
	"fmt"
	"slices"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// Local sentinel aliases. The realtime api package owns the canonical
// error values; aliasing them here lets in-package consumers
// (Hub.Subscribe wiring, hubConnection facades) errors.Is(err,
// ErrTopicForbidden) without importing rtapi twice. Plan 09/10
// carry-forward — Plan 10 reviewer flagged the missing alias pattern.
var (
	ErrTopicForbidden       = rtapi.ErrTopicForbidden
	ErrUnknownTopic         = rtapi.ErrUnknownTopic
	ErrFilterRequired       = rtapi.ErrFilterRequired
	ErrCrossTenantSubscribe = rtapi.ErrCrossTenantSubscribe
)

// TopicRBAC enforces the per-topic role matrix, the per-topic filter
// requirements, AND the cross-tenant filter check (Plan 11.2 Task 4).
//
// The matrix is constructed once at NewTopicRBAC(WithResolvers) and
// never mutated; callers MUST treat the returned *TopicRBAC as
// immutable. Resolvers (when wired) are invoked synchronously on
// every Allow call — the production wiring uses cached resolvers
// (60s TTL + singleflight, see resolver_cache.go) so the per-frame
// cost is amortised.
type TopicRBAC struct {
	rules           map[rtapi.Topic]topicRule
	userResolver    rtapi.UserResolver    // optional; nil = skip cross-tenant on OperatorID
	projectResolver rtapi.ProjectResolver // optional; nil = skip cross-tenant on ProjectID
}

// topicRule is the policy attached to a single Topic. (Field
// semantics carry over from Plan 11 Task 3 verbatim — see rbac.go
// before this change for the full doc.)
type topicRule struct {
	allowedRoles  []string
	requireCallID bool
	selfOnly      bool
}

// NewTopicRBAC returns the canonical realtime RBAC matrix WITHOUT
// resolver wiring. Cross-tenant filter check is skipped — preserved
// for tests + degraded-boot paths where wiring real resolvers is
// undesirable. Production callers use NewTopicRBACWithResolvers.
//
// Adding a new Topic requires extending this map AND the
// rtapi.AllTopics slice — the topic registry test in
// internal/realtime/api/topics_test.go enforces the latter.
func NewTopicRBAC() *TopicRBAC {
	return &TopicRBAC{
		rules: defaultTopicRules(),
	}
}

// NewTopicRBACWithResolvers wires the resolvers used for the
// cross-tenant filter check. nil resolvers are allowed — the matching
// dimension simply skips the check. Production wiring (cmd/api +
// realtime.Module.Register) supplies both; tests typically supply
// stub resolvers for the dimension under test and nil for the
// other.
func NewTopicRBACWithResolvers(users rtapi.UserResolver, projects rtapi.ProjectResolver) *TopicRBAC {
	return &TopicRBAC{
		rules:           defaultTopicRules(),
		userResolver:    users,
		projectResolver: projects,
	}
}

// defaultTopicRules is the canonical rule set extracted into a
// helper so NewTopicRBAC and NewTopicRBACWithResolvers share the
// same map (DRY).
func defaultTopicRules() map[rtapi.Topic]topicRule {
	return map[rtapi.Topic]topicRule{
		rtapi.TopicOperatorsState: {allowedRoles: []string{"admin", "supervisor"}},
		rtapi.TopicDialerQueue:    {allowedRoles: []string{"admin", "supervisor"}},
		rtapi.TopicTrunksHealth:   {allowedRoles: []string{"admin"}},
		rtapi.TopicCallEvents: {
			allowedRoles:  []string{"operator", "admin", "supervisor"},
			requireCallID: true,
		},
		rtapi.TopicNotifications: {
			allowedRoles: []string{"operator", "admin", "supervisor"},
			selfOnly:     true,
		},
		rtapi.TopicForceCommands: {
			allowedRoles: []string{"operator", "admin", "supervisor"},
			selfOnly:     true,
		},
	}
}

// Allow reports whether claims may subscribe to topic with filter.
// Returns nil on success. On rejection returns one of:
//
//   - ErrUnknownTopic         — topic not in the matrix.
//   - ErrTopicForbidden       — role check failed OR selfOnly violation.
//   - ErrFilterRequired       — topic requires a CallID and the filter has none.
//   - ErrCrossTenantSubscribe — filter UUID resolves to a different
//     tenant than claims.TenantID, OR the resolver could not find
//     the UUID (folded together so the wire response can't probe
//     entity existence cross-tenant).
//
// All errors carry context via fmt.Errorf("%w: …") so the error
// chain preserves errors.Is matching at module boundaries.
//
// The signature gained ctx in Plan 11.2 Task 4 — resolvers are
// ctx-aware so a slow DB doesn't pin the subscribe path. Callers
// that previously passed nothing must now supply at least
// context.Background() (Hub passes the connection's run-ctx).
func (r *TopicRBAC) Allow(ctx context.Context, claims rtapi.Claims, topic rtapi.Topic, filter rtapi.SubscriptionFilter) error {
	rule, ok := r.rules[topic]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTopic, topic)
	}
	if !hasAnyRole(claims.Roles, rule.allowedRoles) {
		return fmt.Errorf("%w: roles=%v topic=%s", ErrTopicForbidden, claims.Roles, topic)
	}
	if rule.requireCallID && filter.CallID == "" {
		return fmt.Errorf("%w: topic=%s needs CallID", ErrFilterRequired, topic)
	}
	if rule.selfOnly {
		if claims.UserID == "" {
			return fmt.Errorf("%w: self-only topic=%s requires non-empty UserID", ErrTopicForbidden, topic)
		}
		if filter.OperatorID != "" && filter.OperatorID != claims.UserID {
			return fmt.Errorf("%w: self-only topic=%s", ErrTopicForbidden, topic)
		}
	}

	// Plan 11.2 Task 4: cross-tenant filter check. Resolvers are
	// optional — a nil resolver skips its dimension (preserves the
	// Plan 11 behaviour for tests + degraded boot).
	if r.userResolver != nil && filter.OperatorID != "" {
		// Skip the user-resolver check when the rule is selfOnly AND
		// the filter is empty/self — the selfOnly clause above
		// already established the relationship to claims.UserID.
		if !(rule.selfOnly && (filter.OperatorID == "" || filter.OperatorID == claims.UserID)) {
			got, err := r.userResolver.Get(ctx, filter.OperatorID)
			if err != nil {
				// Fold "not found" + "DB error" into cross-tenant
				// to preserve indistinguishability (security signal:
				// attacker cannot probe user existence cross-tenant).
				return fmt.Errorf("%w: operator_id=%s: %v", ErrCrossTenantSubscribe, filter.OperatorID, err)
			}
			if got.TenantID != claims.TenantID {
				return fmt.Errorf("%w: operator_id=%s belongs to tenant=%s", ErrCrossTenantSubscribe, filter.OperatorID, got.TenantID)
			}
		}
	}
	if r.projectResolver != nil && filter.ProjectID != "" {
		got, err := r.projectResolver.Get(ctx, filter.ProjectID)
		if err != nil {
			return fmt.Errorf("%w: project_id=%s: %v", ErrCrossTenantSubscribe, filter.ProjectID, err)
		}
		if got.TenantID != claims.TenantID {
			return fmt.Errorf("%w: project_id=%s belongs to tenant=%s", ErrCrossTenantSubscribe, filter.ProjectID, got.TenantID)
		}
	}

	// CallID cross-tenant check is intentionally NOT performed here.
	// See Plan 11.2 plan, "Out of scope" — Plan 12 (recording
	// metadata) introduces the CallStore.Get the third resolver
	// would consume. Today, TopicCallEvents cross-tenant safety is
	// enforced by the NATS subject prefix + Hub.Broadcast tenant
	// filter (see Plan 11 Task 3 + 4b doc).

	return nil
}

// hasAnyRole returns true if at least one element of have is also in
// want. O(have * want); both are tiny in practice (≤4 entries each)
// so a hashset would be over-engineering.
func hasAnyRole(have, want []string) bool {
	if len(want) == 0 {
		return false
	}
	for _, h := range have {
		if slices.Contains(want, h) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Update `Hub.Subscribe` to pass ctx**

In `internal/realtime/service/hub.go`, find every `r.rbac.Allow(claims, topic, filter)` call site and update to `r.rbac.Allow(ctx, claims, topic, filter)`. The connection's run-ctx (or the SubscribeFn ctx, depending on the call site) is the right ctx to thread through — both terminate when the connection closes.

There are typically 1–2 call sites (Hub.Subscribe + Hub.subscribeOnConnect or similar). Read hub.go first to identify them. Add a parameter to the SubscribeFn signature in connection.go:

```go
// SubscribeFn is the Hub-side handler for direct (Connection.Subscribe)
// subscription registration. ctx-aware (Plan 11.2 Task 4) so the
// RBAC tenant cross-check has a deadline. Returns the assigned subID
// or an error (RBAC denial / unknown topic / cross-tenant). Plan 11
// Task 3 wires the real fn at Hub.Connect time; before then it is
// nil and Connection.Subscribe returns ErrConnectionClosed.
type SubscribeFn func(ctx context.Context, c *Connection, topic rtapi.Topic, filter rtapi.SubscriptionFilter) (string, error)
```

Update `Connection.Subscribe`:

```go
func (c *Connection) Subscribe(topic rtapi.Topic, filter rtapi.SubscriptionFilter) (string, error) {
	if c.closed.Load() {
		return "", ErrConnectionClosed
	}
	if !c.authenticated.Load() {
		return "", ErrAuthRequired
	}
	if c.subscribeFn == nil {
		return "", errors.New("realtime/service: Subscribe not wired (Hub.Connect not called)")
	}
	// Use a Background-derived ctx scoped to the connection lifetime.
	// The Hub-side SubscribeFn applies its own per-call timeout for
	// the resolver lookups.
	return c.subscribeFn(context.Background(), c, topic, filter)
}
```

(If hub.go has a separate path that drives SubscribeFn from a frame handler, that path passes the reader-loop's ctx through directly.)

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test -race -count=3 ./internal/realtime/service/ -v
```

Expected: PASS for the new RBAC tests AND every existing rbac_test, hub_test, connection_test (they may need ctx params updated where they directly drove SubscribeFn).

- [ ] **Step 6: Commit**

```bash
git add internal/realtime/service/rbac.go internal/realtime/service/rbac_test.go internal/realtime/service/hub.go internal/realtime/service/hub_test.go internal/realtime/service/connection.go
git commit -m "feat(realtime/service): TopicRBAC tenant cross-check via resolvers (Plan 11.2 Task 4)"
```

---

## Task 5: Wire real resolvers in `cmd/api`

**Files:**
- Modify: `cmd/api/realtime.go` (add `userResolverAdapter`, `projectResolverAdapter`, locator registration helpers)
- Modify: `cmd/api/main.go` (register resolvers in locator BEFORE realtime.Module.Register)
- Modify: `internal/realtime/module.go` (look up resolvers from locator; build cached wrappers; pass to NewTopicRBACWithResolvers)

**Background:** The realtime module deliberately does NOT import internal/auth/* or internal/crm/* — Plan 11.1 Task 2 established this scope rule with the `tenancyTenantLister` adapter pattern. Plan 11.2 follows the same pattern for the new resolvers.

- [ ] **Step 1: Add adapters in `cmd/api/realtime.go`**

Append to `cmd/api/realtime.go`:

```go
// authUserStore is the narrow surface cmd/api needs from
// auth.UserStore to satisfy realtime.UserResolver. The auth module
// exposes a richer interface; this slice is enough to project a user
// onto its tenant for the realtime cross-tenant check.
type authUserStore interface {
	GetByID(ctx context.Context, userID uuid.UUID) (authapi.User, error)
}

// userResolverAdapter projects auth.UserStore.GetByID onto the
// realtime UserResolver shape. The user_id wire-string is parsed via
// uuid.Parse — a malformed UUID surfaces as a wrapped error which
// TopicRBAC.Allow folds into ErrCrossTenantSubscribe (matches the
// security rationale in rbac.go: never let the client probe entity
// existence cross-tenant).
type userResolverAdapter struct {
	store authUserStore
}

// newUserResolverAdapter wraps an authUserStore. nil store panics —
// the wiring bug surfaces at cmd/api boot rather than first
// subscribe.
func newUserResolverAdapter(store authUserStore) *userResolverAdapter {
	if store == nil {
		panic("cmd/api: newUserResolverAdapter: store must be non-nil")
	}
	return &userResolverAdapter{store: store}
}

// Get implements rtapi.UserResolver.
func (a *userResolverAdapter) Get(ctx context.Context, userID string) (rtapi.ResolvedTenant, error) {
	id, err := uuid.Parse(userID)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: parse user_id %q: %w", userID, err)
	}
	user, err := a.store.GetByID(ctx, id)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: get user %s: %w", id, err)
	}
	return rtapi.ResolvedTenant{TenantID: user.TenantID.String()}, nil
}

// crmProjectService is the narrow surface cmd/api needs from
// crm.ProjectService to satisfy realtime.ProjectResolver.
type crmProjectService interface {
	Get(ctx context.Context, tenantID, projectID uuid.UUID) (crmapi.Project, error)
}

// projectResolverAdapter projects crm.ProjectService.Get onto the
// realtime ProjectResolver shape. Note: crm.ProjectService.Get takes
// (tenantID, projectID) — but the realtime resolver only knows the
// project_id at Allow time (the tenant is what we're VERIFYING). To
// resolve, we use a "bypass" pattern: the cmd/api adapter calls
// the underlying ProjectStore directly via a thin wrapper that
// accepts only project_id. The store layer enforces RLS via
// BypassRLS so the admin-grade lookup does not leak.
//
// (Implementation note: crm/store/projects.go exposes a ByID(ctx,
// projectID) helper that returns the project and its tenant_id.
// The adapter calls that helper.)
type projectResolverAdapter struct {
	store crmProjectStoreByID
}

// crmProjectStoreByID is the tiny tenant-agnostic lookup the resolver
// uses. Implemented by crm/store/ProjectStore.GetByID.
type crmProjectStoreByID interface {
	GetByID(ctx context.Context, projectID uuid.UUID) (crmapi.Project, error)
}

// newProjectResolverAdapter wraps a crmProjectStoreByID.
func newProjectResolverAdapter(store crmProjectStoreByID) *projectResolverAdapter {
	if store == nil {
		panic("cmd/api: newProjectResolverAdapter: store must be non-nil")
	}
	return &projectResolverAdapter{store: store}
}

// Get implements rtapi.ProjectResolver.
func (a *projectResolverAdapter) Get(ctx context.Context, projectID string) (rtapi.ResolvedTenant, error) {
	id, err := uuid.Parse(projectID)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: parse project_id %q: %w", projectID, err)
	}
	proj, err := a.store.GetByID(ctx, id)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: get project %s: %w", id, err)
	}
	return rtapi.ResolvedTenant{TenantID: proj.TenantID.String()}, nil
}

// resolveResolversFromLocator returns the production-wired
// rtapi.UserResolver + rtapi.ProjectResolver looked up from the
// locator under module-private keys. cmd/api populates these BEFORE
// realtime.Module.Register so the realtime module finds them
// without importing auth/crm.
//
// Missing-but-tolerated: an empty resolver fallback (returns
// ErrCrossTenantSubscribe on every call) preserves the degraded-boot
// path. The resulting RBAC behaviour is "accept role-correct
// subscribes; reject any with a non-empty filter UUID" — strictly
// safer than "accept any" so a degraded boot doesn't widen the
// security envelope.
//
// nil store inputs panic (boot-time wiring bug).
func resolveResolversFromLocator(locator modules.ServiceLocator, logger *zap.Logger) (rtapi.UserResolver, rtapi.ProjectResolver) {
	if locator == nil {
		logger.Warn("realtime resolvers: locator missing, using empty fallbacks (degraded boot)")
		return emptyUserResolver{}, emptyProjectResolver{}
	}
	var users rtapi.UserResolver = emptyUserResolver{}
	if v, ok := locator.Lookup(rtapi.LocatorUserResolver); ok {
		if r, ok := v.(rtapi.UserResolver); ok {
			users = r
		} else {
			logger.Warn("realtime resolvers: UserResolver registered with wrong type",
				zap.String("got_type", fmt.Sprintf("%T", v)),
			)
		}
	} else {
		logger.Info("realtime resolvers: UserResolver missing, using empty fallback")
	}
	var projects rtapi.ProjectResolver = emptyProjectResolver{}
	if v, ok := locator.Lookup(rtapi.LocatorProjectResolver); ok {
		if r, ok := v.(rtapi.ProjectResolver); ok {
			projects = r
		} else {
			logger.Warn("realtime resolvers: ProjectResolver registered with wrong type",
				zap.String("got_type", fmt.Sprintf("%T", v)),
			)
		}
	} else {
		logger.Info("realtime resolvers: ProjectResolver missing, using empty fallback")
	}
	return users, projects
}

// emptyUserResolver / emptyProjectResolver are the fallbacks used
// when the locator does not have the production adapters. Returning
// ErrCrossTenantSubscribe on every call means the cross-tenant
// check never accidentally accepts in degraded boot.
type emptyUserResolver struct{}

func (emptyUserResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, rtapi.ErrCrossTenantSubscribe
}

type emptyProjectResolver struct{}

func (emptyProjectResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, rtapi.ErrCrossTenantSubscribe
}
```

Add the imports the new code needs (preserve all existing imports):

```go
import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/modules"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	rtevents "github.com/sociopulse/platform/internal/realtime/events"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)
```

**Note on `crm.ProjectStore.GetByID`:** if the crm store does not yet expose a tenant-agnostic GetByID, the implementer must add it. The crm module's existing `ProjectService.Get(ctx, tenantID, projectID)` enforces tenant scope by accepting tenant_id; the resolver's job is the inverse (find tenant from id), so a privileged-by-design GetByID at the store layer is required. Implement it via `BypassRLS` + a single SELECT on `projects WHERE id = $1`.

- [ ] **Step 2: Register resolvers in `cmd/api/main.go` BEFORE `realtime.Module.Register`**

In `cmd/api/main.go`, locate the section where modules are registered (after auth + crm modules; before realtime). Add resolver registration immediately before `realtime.Module.Register`:

```go
// Plan 11.2 Task 5: register resolvers under the realtime locator
// keys so the realtime module can build cached wrappers without
// importing internal/auth/* or internal/crm/*. Order matters: this
// MUST happen AFTER auth.Module.Register + crm.Module.Register
// (they populate auth.UserStore + crm.ProjectStore in the locator)
// and BEFORE realtime.Module.Register (which looks up the resolver
// keys).
if userStoreV, ok := locator.Lookup("auth.UserStore"); ok {
	if userStore, ok := userStoreV.(authUserStore); ok {
		locator.Register(rtapi.LocatorUserResolver, rtapi.UserResolver(newUserResolverAdapter(userStore)))
		log.Info("realtime: UserResolver registered from auth.UserStore")
	} else {
		log.Warn("realtime: auth.UserStore registered with wrong type; UserResolver disabled",
			zap.String("got_type", fmt.Sprintf("%T", userStoreV)),
		)
	}
} else {
	log.Info("realtime: auth.UserStore missing; UserResolver disabled (degraded boot)")
}

if projStoreV, ok := locator.Lookup("crm.ProjectStore"); ok {
	if projStore, ok := projStoreV.(crmProjectStoreByID); ok {
		locator.Register(rtapi.LocatorProjectResolver, rtapi.ProjectResolver(newProjectResolverAdapter(projStore)))
		log.Info("realtime: ProjectResolver registered from crm.ProjectStore")
	} else {
		log.Warn("realtime: crm.ProjectStore registered with wrong type; ProjectResolver disabled",
			zap.String("got_type", fmt.Sprintf("%T", projStoreV)),
		)
	}
} else {
	log.Info("realtime: crm.ProjectStore missing; ProjectResolver disabled (degraded boot)")
}
```

(Implementer reads the actual main.go first to find the exact insertion point; the section above the realtime registration is the canonical location.)

- [ ] **Step 3: Update `internal/realtime/module.go` to consume the resolvers**

In `internal/realtime/module.go`, replace the `service.NewTopicRBAC()` call (around line 169) with the cached-resolver-wrapped variant:

```go
	// Plan 11.2 Task 4: cross-tenant filter check via cached resolvers.
	// resolveResolversFromLocator returns empty fallbacks when the
	// production adapters aren't wired (degraded boot); the empty
	// fallback rejects every cross-tenant lookup so the security
	// envelope is bounded regardless.
	rawUsers, rawProjects := resolveResolversFromLocator(d.Locator, logger)
	cachedUsers := service.NewCachedUserResolver(rawUsers, 0, logger)       // 0 → defaultResolverTTL (60s)
	cachedProjects := service.NewCachedProjectResolver(rawProjects, 0, logger)
	rbac := service.NewTopicRBACWithResolvers(cachedUsers, cachedProjects)
```

The `resolveResolversFromLocator` helper lives in cmd/api in Step 1 above — but the realtime module needs its own, since `internal/realtime/` cannot import `cmd/api`. Add the same helper (with empty-fallback resolver types) to a new private file:

`internal/realtime/resolver_lookup.go`:

```go
package realtime

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/modules"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// resolveResolversFromLocator looks up the rtapi.UserResolver +
// rtapi.ProjectResolver registered by cmd/api under the
// rtapi.LocatorUserResolver / LocatorProjectResolver keys. Missing
// entries fall back to empty resolvers that reject every cross-tenant
// lookup — strictly safer than no check.
func resolveResolversFromLocator(locator modules.ServiceLocator, logger *zap.Logger) (rtapi.UserResolver, rtapi.ProjectResolver) {
	if locator == nil {
		logger.Info("realtime resolvers: locator missing, using empty fallbacks")
		return emptyUserResolver{}, emptyProjectResolver{}
	}
	var users rtapi.UserResolver = emptyUserResolver{}
	if v, ok := locator.Lookup(rtapi.LocatorUserResolver); ok {
		if r, ok := v.(rtapi.UserResolver); ok {
			users = r
		} else {
			logger.Warn("realtime resolvers: UserResolver registered with wrong type",
				zap.String("got_type", fmt.Sprintf("%T", v)),
			)
		}
	} else {
		logger.Info("realtime resolvers: UserResolver missing, using empty fallback")
	}
	var projects rtapi.ProjectResolver = emptyProjectResolver{}
	if v, ok := locator.Lookup(rtapi.LocatorProjectResolver); ok {
		if r, ok := v.(rtapi.ProjectResolver); ok {
			projects = r
		} else {
			logger.Warn("realtime resolvers: ProjectResolver registered with wrong type",
				zap.String("got_type", fmt.Sprintf("%T", v)),
			)
		}
	} else {
		logger.Info("realtime resolvers: ProjectResolver missing, using empty fallback")
	}
	return users, projects
}

// emptyUserResolver / emptyProjectResolver reject every cross-tenant
// lookup — used when cmd/api hasn't wired the production adapters.
type emptyUserResolver struct{}

func (emptyUserResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, rtapi.ErrCrossTenantSubscribe
}

type emptyProjectResolver struct{}

func (emptyProjectResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, rtapi.ErrCrossTenantSubscribe
}
```

- [ ] **Step 4: Build + lint + run all realtime tests + run cmd/api tests**

```bash
go build ./...
go test -race -count=1 ./internal/realtime/... ./cmd/api/...
golangci-lint run ./internal/realtime/... ./cmd/api/...
```

Expected: build clean, all tests pass, no lint warnings.

- [ ] **Step 5: Commit**

```bash
git add internal/realtime/module.go internal/realtime/resolver_lookup.go cmd/api/realtime.go cmd/api/main.go
git commit -m "feat(cmd/api+realtime): wire UserResolver+ProjectResolver from auth/crm (Plan 11.2 Task 5)"
```

---

## Self-Review

After all 5 tasks land, verify against the spec:

**1. Spec coverage:**
- Task 10.1 (Plan 11): Frame classification — ✓ Tasks 1 + 2.
- Task 10.2 (Plan 11): Listen-in cleanup hooks — DEFERRED (Plan 08 dependency); explicitly out-of-scope per opening section.
- Task 10.3 (Plan 11): RBAC tenant cross-check — ✓ Tasks 3 + 4 (User + Project dimensions); Call dimension deferred to Plan 12.
- Plan 11.1 references cap (line 23 of PROJECT_STATUS): "Tasks deferred to Plan 11.2" — every promised item is either landed or explicitly deferred with rationale.

**2. Placeholder scan:**
- Every "Implement X" step has explicit code blocks.
- Every "Run test" step has the exact `go test ...` command.
- Every commit has the exact `git commit -m "..."` line.
- No "TBD", "TODO", "fill in", "similar to Task N" — verified by Ctrl-F over the doc.

**3. Type consistency:**
- `rtapi.ResolvedTenant{TenantID string}` used uniformly across api + service.
- `rtapi.UserResolver.Get(ctx, userID string) (ResolvedTenant, error)` used uniformly in stub, cache wrapper, adapter.
- `rtapi.ErrCrossTenantSubscribe` referenced as `service.ErrCrossTenantSubscribe` after the package alias (matches Plan 09/10 carry-forward).
- `NewTopicRBACWithResolvers(users rtapi.UserResolver, projects rtapi.ProjectResolver)` — signature matches across rbac.go + tests + module.go.
- `FrameClass` enum values — `FrameClassUnknown`, `FrameClassCritical`, `FrameClassTelemetry` — used uniformly in api + service.

---

## Acceptance criteria

- All 5 tasks committed with green tests + green lint.
- `go test -race -count=3 ./internal/realtime/... ./cmd/api/...` passes.
- `make lint` clean.
- `make test` clean.
- Coverage: `internal/realtime/api/frames.go` ≥ 95% (small file, exhaustive test); `internal/realtime/service/resolver_cache.go` ≥ 90%; `internal/realtime/service/rbac.go` ≥ 92% (preserves Plan 11 Task 3 baseline + new branches).
- New metrics surfaced on `/metrics`:
  - `realtime_critical_overflows_total{conn_id}`
  - `realtime_unknown_topic_classes_total{topic}`
- New sentinel errors documented in `internal/realtime/api/errors.go`.
- New locator keys documented in `internal/realtime/api/locator.go`.

## Carry-overs to Plan 11.3+

- **Plan 11.3 candidate (or Plan 12 dependent):** CallResolver — depends on Plan 12's recording metadata (call_id → tenant_id index). Before then, `TopicCallEvents` cross-tenant safety is enforced upstream by NATS subject prefix + Hub.Broadcast tenant filter.
- **Plan 11.3 candidate:** Listen-in cleanup hooks (Plan 11 Task 10.2) — depends on Plan 08 (FreeSWITCH cluster + listen-in service).
- **Plan 11.3 candidate:** Cache invalidation on tenant.user.deleted / project.archived NATS events — today the 60s TTL bounds stale visibility; explicit invalidation would shave the worst case to immediate. Wire in once the user-deleted / project-archived NATS events ship.
- **Plan 12 dependency:** Frame backpressure metric — `realtime_frame_class_distribution_total{class}` for ops dashboards (how many critical vs. telemetry frames per second per replica). Adding this requires `Connection.Send` to tick a counter on every routing decision; trivial follow-up once Plan 11.2 lands.
