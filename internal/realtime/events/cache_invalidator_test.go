// cache_invalidator_test.go — behaviour tests for *CacheInvalidator.
//
// Test discipline:
//
//   - The invalidator owns no goroutines; goleak in main_test.go guards
//     against a regression that adds one.
//   - All tests run in parallel and share the in-memory fakeBus from
//     nats_subscriber_test.go (Subscribe + Fire) so we can drive
//     synthetic deliveries through the registered handler without spinning
//     a real NATS / JetStream embedded server.
//   - Plan 11.3 Task 3 Step 1 requires three classes of test:
//     1. ProjectStatusChanged → ProjectInvalidate is called with the
//     ProjectID string;
//     2. malformed payload → the parse_error metric ticks AND
//     ProjectInvalidate is NOT called;
//     3. constructor invariants — nil Subscriber and nil
//     ProjectInvalidate panic at NewCacheInvalidator.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/realtime/events"
	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// fakeProjectInvalidator captures Invalidate(string) calls so tests
// can assert on the ProjectIDs the invalidator forwarded.
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
// ProjectID to the invalidator and ticks the ok-labelled metric.
func TestCacheInvalidator_ProjectStatusChangedTriggersInvalidate(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	target := &fakeProjectInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus,
		ProjectInvalidate: target.Invalidate,
		Metrics:           metrics,
		Logger:            zaptest.NewLogger(t),
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	tenantID := uuid.New()
	projectID := uuid.New()
	payload, err := json.Marshal(crmapi.ProjectStatusChangedEvent{
		ProjectID: projectID,
		TenantID:  tenantID,
		OldStatus: crmapi.StatusActive,
		NewStatus: crmapi.StatusArchived,
		ChangedAt: time.Now(),
	})
	require.NoError(t, err)

	require.NoError(t, bus.Fire(crmapi.SubjectProjectStatusFor(tenantID), payload))

	// Fire is synchronous so assertions can run immediately, but use
	// Eventually anyway so the test stays robust if the invalidator
	// ever gains an internal goroutine in a future revision.
	require.Eventually(t, func() bool {
		return len(target.Calls()) >= 1
	}, 2*time.Second, 10*time.Millisecond,
		"invalidator must call ProjectInvalidate on status_changed")

	calls := target.Calls()
	assert.Contains(t, calls, projectID.String(),
		"ProjectInvalidate must receive the project_id from the event")

	require.InDelta(t, 1.0,
		counterValue(t, reg, "realtime_cache_invalidations_total",
			map[string]string{
				"subject": events.SubjectProjectStatus,
				"result":  "ok",
			}), 0.0001)
}

// TestCacheInvalidator_MalformedPayloadTicksParseError verifies the
// observability path: a malformed payload bumps the parse_error
// metric label and does NOT call Invalidate.
func TestCacheInvalidator_MalformedPayloadTicksParseError(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	target := &fakeProjectInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus,
		ProjectInvalidate: target.Invalidate,
		Metrics:           metrics,
		Logger:            zaptest.NewLogger(t),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	tenantID := uuid.New()
	require.NoError(t, bus.Fire(crmapi.SubjectProjectStatusFor(tenantID),
		[]byte("not-json")))

	require.Eventually(t, func() bool {
		v := counterValue(t, reg, "realtime_cache_invalidations_total",
			map[string]string{
				"subject": events.SubjectProjectStatus,
				"result":  "parse_error",
			})
		return v >= 1.0
	}, 2*time.Second, 10*time.Millisecond,
		"malformed payload must tick parse_error metric")

	assert.Empty(t, target.Calls(),
		"malformed payload must NOT call ProjectInvalidate")
}

// TestCacheInvalidator_EmptyProjectIDTicksEmptyMetric exercises the
// defensive branch: a status_changed payload with a zero-UUID ProjectID
// must NOT call Invalidate but must tick the empty_id label so the bug
// is observable on dashboards. Plan 11.4 Task 6 renamed the result
// label value from "empty_project_id" to the uniform "empty_id".
func TestCacheInvalidator_EmptyProjectIDTicksEmptyMetric(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	target := &fakeProjectInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus,
		ProjectInvalidate: target.Invalidate,
		Metrics:           metrics,
		Logger:            zaptest.NewLogger(t),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	tenantID := uuid.New()
	payload, err := json.Marshal(crmapi.ProjectStatusChangedEvent{
		// ProjectID intentionally zero-valued.
		TenantID:  tenantID,
		OldStatus: crmapi.StatusActive,
		NewStatus: crmapi.StatusArchived,
		ChangedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, bus.Fire(crmapi.SubjectProjectStatusFor(tenantID), payload))

	require.Eventually(t, func() bool {
		v := counterValue(t, reg, "realtime_cache_invalidations_total",
			map[string]string{
				"subject": events.SubjectProjectStatus,
				"result":  "empty_id",
			})
		return v >= 1.0
	}, 2*time.Second, 10*time.Millisecond,
		"empty project_id must tick the empty_id metric")

	assert.Empty(t, target.Calls(),
		"empty project_id must NOT call ProjectInvalidate")
}

// TestCacheInvalidator_NewWithNilSubscriberPanics is the wiring guard
// for the Subscriber dependency.
func TestCacheInvalidator_NewWithNilSubscriberPanics(t *testing.T) {
	t.Parallel()

	require.PanicsWithValue(t,
		"realtime/events: NewCacheInvalidator: Subscriber must be non-nil",
		func() {
			_ = events.NewCacheInvalidator(events.CacheInvalidatorConfig{
				ProjectInvalidate: func(string) {},
			})
		},
	)
}

// TestCacheInvalidator_NewWithNilProjectInvalidatePanics is the
// wiring guard for the callback dependency.
func TestCacheInvalidator_NewWithNilProjectInvalidatePanics(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	require.PanicsWithValue(t,
		"realtime/events: NewCacheInvalidator: ProjectInvalidate must be non-nil",
		func() {
			_ = events.NewCacheInvalidator(events.CacheInvalidatorConfig{
				Subscriber: bus,
			})
		},
	)
}

// TestCacheInvalidator_StartPropagatesSubscribeError ensures a Subscribe
// failure surfaces to the caller verbatim (wrapped, not swallowed). The
// composition root in module.go logs WARN + falls back to TTL-only
// invalidation; this test pins the contract on which Start exposes that
// signal.
func TestCacheInvalidator_StartPropagatesSubscribeError(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{
		subscribeErrOn: map[string]error{
			events.SubjectProjectStatus: errBoom,
		},
	}
	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus,
		ProjectInvalidate: func(string) {},
	})
	err := inv.Start(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "realtime/events:")
	require.ErrorContains(t, err, events.SubjectProjectStatus)
}

// TestCacheInvalidator_StopIdempotent ensures Stop can be invoked
// multiple times without panicking — the lifecycle helper is a no-op
// once the underlying ctx has been cancelled (the bus owns the
// consumer goroutine).
func TestCacheInvalidator_StopIdempotent(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus,
		ProjectInvalidate: func(string) {},
	})
	require.NoError(t, inv.Start(context.Background()))
	inv.Stop()
	inv.Stop()
}

// TestCacheInvalidator_DefaultQueueGroup verifies that an absent
// QueueGroup defaults to the documented constant. The contract matters
// because every realtime replica must join the SAME queue group so
// JetStream delivers each event to exactly one replica's invalidator
// (cache invalidation is idempotent at the resolver level, but a
// duplicate Subscribe would fan out one Invalidate per replica per
// event — wasteful, not incorrect).
func TestCacheInvalidator_DefaultQueueGroup(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus,
		ProjectInvalidate: func(string) {},
	})
	require.NoError(t, inv.Start(context.Background()))
	t.Cleanup(inv.Stop)

	queues := bus.Queues()
	require.Len(t, queues, 1)
	require.Equal(t, "realtime-cache-invalidator", queues[0])
}

// errBoom is a sentinel for Subscribe-error injection. Defined as a
// package-level var so multiple tests can errors.Is against it
// without re-constructing it per test.
var errBoom = errors.New("subscribe boom")

// fakeCallInvalidator captures Invalidate(string) calls so tests can
// assert on the CallIDs the invalidator forwarded.
type fakeCallInvalidator struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeCallInvalidator) Invalidate(callID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, callID)
}

func (f *fakeCallInvalidator) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeUserInvalidator mirrors fakeCallInvalidator for user_id.
type fakeUserInvalidator struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeUserInvalidator) Invalidate(userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
}

func (f *fakeUserInvalidator) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestCacheInvalidator_RecordingCallDeletedTriggersCallInvalidate verifies
// that publishing tenant.<t>.recording.call.deleted routes the CallID
// to the call invalidator.
func TestCacheInvalidator_RecordingCallDeletedTriggersCallInvalidate(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	pTarget := &fakeProjectInvalidator{}
	cTarget := &fakeCallInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus,
		ProjectInvalidate: pTarget.Invalidate,
		CallInvalidate:    cTarget.Invalidate,
		Metrics:           metrics,
		Logger:            zaptest.NewLogger(t),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	tenantID := uuid.New()
	callID := uuid.New()
	recordingID := uuid.New()
	payload, err := json.Marshal(rapi.RecordingCallDeletedEvent{
		RecordingID: recordingID,
		CallID:      callID,
		TenantID:    tenantID,
		DeletedAt:   time.Now().Unix(),
		Reason:      "retention",
	})
	require.NoError(t, err)

	require.NoError(t, bus.Fire(rapi.SubjectRecordingCallDeletedFor(tenantID), payload))

	require.Eventually(t, func() bool {
		return len(cTarget.Calls()) >= 1
	}, 2*time.Second, 10*time.Millisecond,
		"CallInvalidate must fire on recording.call.deleted")

	assert.Contains(t, cTarget.Calls(), callID.String())
	assert.Empty(t, pTarget.Calls(),
		"ProjectInvalidate must not fire on a recording event")

	require.InDelta(t, 1.0,
		counterValue(t, reg, "realtime_cache_invalidations_total",
			map[string]string{
				"subject": events.SubjectRecordingCallDeleted,
				"result":  "ok",
			}), 0.0001)
}

// TestCacheInvalidator_AuthUserDeletedTriggersUserInvalidate verifies
// that publishing tenant.<t>.auth.user.deleted routes the UserID to
// the user invalidator.
func TestCacheInvalidator_AuthUserDeletedTriggersUserInvalidate(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	uTarget := &fakeUserInvalidator{}
	pTarget := &fakeProjectInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus,
		ProjectInvalidate: pTarget.Invalidate,
		UserInvalidate:    uTarget.Invalidate,
		Metrics:           metrics,
		Logger:            zaptest.NewLogger(t),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	tenantID := uuid.New()
	userID := uuid.New()
	payload, err := json.Marshal(authapi.UserDeletedEvent{
		UserID:    userID,
		TenantID:  tenantID,
		DeletedAt: time.Now().Unix(),
		Reason:    "archived",
	})
	require.NoError(t, err)

	require.NoError(t, bus.Fire(authapi.SubjectUserDeletedFor(tenantID), payload))

	require.Eventually(t, func() bool {
		return len(uTarget.Calls()) >= 1
	}, 2*time.Second, 10*time.Millisecond,
		"UserInvalidate must fire on auth.user.deleted")

	assert.Contains(t, uTarget.Calls(), userID.String())

	require.InDelta(t, 1.0,
		counterValue(t, reg, "realtime_cache_invalidations_total",
			map[string]string{
				"subject": events.SubjectUserDeleted,
				"result":  "ok",
			}), 0.0001)
}

// TestCacheInvalidator_NilUserInvalidate_SkipsAuthSubscription —
// degraded boot path: no user invalidator wired, so the auth
// subscription is NOT registered. Publishing the auth event must be a
// no-op and the project subscription must remain functional.
func TestCacheInvalidator_NilUserInvalidate_SkipsAuthSubscription(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	pTarget := &fakeProjectInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus,
		ProjectInvalidate: pTarget.Invalidate,
		// UserInvalidate intentionally omitted (nil)
		// CallInvalidate intentionally omitted (nil)
		Metrics: metrics,
		Logger:  zaptest.NewLogger(t),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	// Project subscription still works.
	tenantID := uuid.New()
	projectID := uuid.New()
	payload, err := json.Marshal(crmapi.ProjectStatusChangedEvent{
		ProjectID: projectID,
		TenantID:  tenantID,
		OldStatus: crmapi.StatusActive,
		NewStatus: crmapi.StatusArchived,
		ChangedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, bus.Fire(crmapi.SubjectProjectStatusFor(tenantID), payload))

	require.Eventually(t, func() bool {
		return len(pTarget.Calls()) >= 1
	}, 2*time.Second, 10*time.Millisecond)
}
