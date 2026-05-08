package queue_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/queue"
)

// queueFixture wires miniredis + RedisQueue + a frozen clock + a fresh
// metrics registry. miniredis interprets cjson + ZADD/ZPOPMIN exactly the
// same way real Redis does for our use case (the integration test in
// redis_zset_integration_test.go is the authoritative pass).
type queueFixture struct {
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	clock   *fakeClock
	metrics *queue.Metrics
	reg     *prometheus.Registry
	q       *queue.RedisQueue
}

// fakeClock is a frozen clock fed into Config.Clock. Tests advance it
// explicitly via Set / Add so EnqueuedAt / score values are deterministic.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newFixture(t *testing.T) *queueFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	reg := prometheus.NewRegistry()
	m := queue.RegisterMetrics(reg)
	q, err := queue.New(queue.Config{
		Redis:   rdb,
		Logger:  zaptest.NewLogger(t),
		Clock:   clk.Now,
		Metrics: m,
	})
	require.NoError(t, err)
	return &queueFixture{
		mr: mr, rdb: rdb, clock: clk, metrics: m, reg: reg, q: q,
	}
}

// counterValue is a tiny helper that gathers the registry and sums the
// counter values matching name + label values. Returns 0 when no
// metric matches.
func (f *queueFixture) counterValue(t *testing.T, name string, labelMatch map[string]string) float64 {
	t.Helper()
	mfs, err := f.reg.Gather()
	require.NoError(t, err)
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.GetMetric() {
			if !labelsMatch(met.GetLabel(), labelMatch) {
				continue
			}
			if c := met.GetCounter(); c != nil {
				sum += c.GetValue()
			}
		}
	}
	return sum
}

func labelsMatch(have []*dto.LabelPair, want map[string]string) bool {
	if want == nil {
		return true
	}
	got := make(map[string]string, len(have))
	for _, lp := range have {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// TestNew_RequiresRedis — Config.Redis nil = constructor error. Every
// other field is nil-tolerated.
func TestNew_RequiresRedis(t *testing.T) {
	t.Parallel()
	_, err := queue.New(queue.Config{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Redis")
}

// TestNew_DefaultsApply — nil Logger / Clock / Metrics + zero TTL all
// fall back to safe defaults; the queue is fully usable.
func TestNew_DefaultsApply(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	q, err := queue.New(queue.Config{Redis: rdb})
	require.NoError(t, err)
	require.NotNil(t, q)

	tenantID, projectID := uuid.New(), uuid.New()
	ok, err := q.EnqueueRespondent(context.Background(), api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: uuid.New(),
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	})
	require.NoError(t, err)
	require.True(t, ok)
}

// TestEnqueue_HappyPath — single enqueue lands in both ZSET and dedup SET
// with TTL refreshed on each.
func TestEnqueue_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	ok, err := f.q.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	})
	require.NoError(t, err)
	require.True(t, ok)

	zKey := "q:" + tenantID.String() + ":project:" + projectID.String()
	dKey := "qd:" + tenantID.String() + ":project:" + projectID.String()
	zCard, err := f.rdb.ZCard(ctx, zKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), zCard)

	isMember, err := f.rdb.SIsMember(ctx, dKey, respID.String()).Result()
	require.NoError(t, err)
	require.True(t, isMember)

	require.InDelta(t, 1.0, f.counterValue(t, "dialer_queue_enqueue_total", map[string]string{"result": "ok"}), 0.0)
}

// TestEnqueue_DuplicateReturnsFalseNoError — the API contract says
// EnqueueRespondent returns ok=false (not an error) on duplicate. The
// metric must record a "duplicate" outcome rather than "ok".
func TestEnqueue_DuplicateReturnsFalseNoError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	req := api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	}
	ok, err := f.q.EnqueueRespondent(ctx, req)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = f.q.EnqueueRespondent(ctx, req)
	require.NoError(t, err)
	require.False(t, ok)

	// ZSET still has only one member.
	zKey := "q:" + tenantID.String() + ":project:" + projectID.String()
	zCard, err := f.rdb.ZCard(ctx, zKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), zCard)

	require.InDelta(t, 1.0, f.counterValue(t, "dialer_queue_enqueue_total", map[string]string{"result": "ok"}), 0.0)
	require.InDelta(t, 1.0, f.counterValue(t, "dialer_queue_enqueue_total", map[string]string{"result": "duplicate"}), 0.0)
}

// TestPickNext_OrdersByPriorityThenFIFO — three items at different
// priorities + same-priority older/newer pop in the documented order:
// lowest priority first; among ties, the older one.
func TestPickNext_OrdersByPriorityThenFIFO(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	// Enqueue priority-5 first (older), then priority-3, then priority-5
	// again (newer). Expected pop order: priority 3 → priority 5 (older)
	// → priority 5 (newer).
	rA := uuid.New()
	rB := uuid.New()
	rC := uuid.New()

	enqueueAt := func(at time.Time, p uint8, r uuid.UUID) {
		f.clock.now = at
		ok, err := f.q.EnqueueRespondent(ctx, api.EnqueueRequest{
			TenantID:     tenantID,
			ProjectID:    projectID,
			RespondentID: r,
			Phone:        "+79991234567",
			Region:       "RU-MOW",
			Priority:     p,
		})
		require.NoError(t, err)
		require.True(t, ok)
	}

	t0 := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	enqueueAt(t0, 5, rA)                    // priority 5, older
	enqueueAt(t0.Add(time.Second), 3, rB)   // priority 3, newer than rA
	enqueueAt(t0.Add(2*time.Second), 5, rC) // priority 5, newest

	// First pop: priority 3 wins regardless of age.
	item, err := f.q.PickNext(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, rB, item.RespondentID)
	require.Equal(t, uint8(3), item.Priority)

	// Second pop: priority 5, older (rA).
	item, err = f.q.PickNext(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, rA, item.RespondentID)

	// Third pop: priority 5, newer (rC).
	item, err = f.q.PickNext(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, rC, item.RespondentID)
}

// TestPickNext_ReturnsErrQueueEmpty — empty queue surfaces the sentinel.
// errors.Is is the public-API contract.
func TestPickNext_ReturnsErrQueueEmpty(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.q.PickNext(ctx, tenantID, projectID)
	require.ErrorIs(t, err, api.ErrQueueEmpty)
	require.InDelta(t, 1.0, f.counterValue(t, "dialer_queue_pickup_total", map[string]string{"result": "empty"}), 0.0)
}

// TestPickNext_RemovesFromDedupSet — successful pop removes the
// respondent from the dedup SET so a subsequent EnqueueRespondent for
// the same id succeeds.
func TestPickNext_RemovesFromDedupSet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	ok, err := f.q.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	})
	require.NoError(t, err)
	require.True(t, ok)

	_, err = f.q.PickNext(ctx, tenantID, projectID)
	require.NoError(t, err)

	dKey := "qd:" + tenantID.String() + ":project:" + projectID.String()
	isMember, err := f.rdb.SIsMember(ctx, dKey, respID.String()).Result()
	require.NoError(t, err)
	require.False(t, isMember, "PickNext must SREM the popped respondent")

	// Re-enqueue same respondent: should succeed.
	ok, err = f.q.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	})
	require.NoError(t, err)
	require.True(t, ok, "re-enqueue after pop must succeed")
}

// TestRequeue_AppliesDelay — Requeue with a 5-minute delay places the
// item at a future timestamp; the same item is NOT immediately popped
// when the queue's current clock is the original time.
func TestRequeue_AppliesDelay(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	t0 := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f.clock.now = t0

	ok, err := f.q.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	})
	require.NoError(t, err)
	require.True(t, ok)

	item, err := f.q.PickNext(ctx, tenantID, projectID)
	require.NoError(t, err)

	// Requeue with 5 minutes delay. After requeue the item is at score
	// = priority*1e9 + (t0 + 5min).UnixMilli().
	require.NoError(t, f.q.Requeue(ctx, item, 5*time.Minute))

	// Inspect the score directly via miniredis.
	zKey := "q:" + tenantID.String() + ":project:" + projectID.String()
	scores, err := f.rdb.ZRangeWithScores(ctx, zKey, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, scores, 1)

	expectedAt := t0.Add(5 * time.Minute)
	expectedScore := float64(5)*1e9 + float64(expectedAt.UnixMilli())
	require.InDelta(t, expectedScore, scores[0].Score, 0.5)

	require.InDelta(t, 1.0, f.counterValue(t, "dialer_queue_requeue_total", nil), 0.0)
}

// TestRequeue_CapsPriorityAtMax — even if a caller smuggles a Priority
// of 250 into the QueueItem, Requeue clamps it to maxPriority (9).
func TestRequeue_CapsPriorityAtMax(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	item := api.QueueItem{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Priority:     250,
		EnqueuedAt:   time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
		Phone:        "+79991234567",
		Region:       "RU-MOW",
	}
	require.NoError(t, f.q.Requeue(ctx, item, 0))

	out, err := f.q.PickNext(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, uint8(9), out.Priority)
}

// TestRequeue_NegativeDelayBecomesZero — a defensive guard: a negative
// delay (clock skew, rounding) is treated as 0 rather than placing the
// item arbitrarily far in the past.
func TestRequeue_NegativeDelayBecomesZero(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	t0 := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f.clock.now = t0

	item := api.QueueItem{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Priority:     5,
		EnqueuedAt:   t0.Add(-time.Hour),
		Phone:        "+79991234567",
		Region:       "RU-MOW",
	}
	require.NoError(t, f.q.Requeue(ctx, item, -time.Minute))

	zKey := "q:" + tenantID.String() + ":project:" + projectID.String()
	scores, err := f.rdb.ZRangeWithScores(ctx, zKey, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, scores, 1)
	expectedScore := float64(5)*1e9 + float64(t0.UnixMilli())
	require.InDelta(t, expectedScore, scores[0].Score, 0.5)
}

// TestSize_ReturnsZSetCardinality — Size returns the live ZCARD, and
// observes the gauge for the (tenant, project) pair.
func TestSize_ReturnsZSetCardinality(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	n, err := f.q.Size(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Zero(t, n)

	for range 3 {
		_, err := f.q.EnqueueRespondent(ctx, api.EnqueueRequest{
			TenantID:     tenantID,
			ProjectID:    projectID,
			RespondentID: uuid.New(),
			Phone:        "+79991234567",
			Region:       "RU-MOW",
			Priority:     5,
		})
		require.NoError(t, err)
	}

	n, err = f.q.Size(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, int64(3), n)

	// Verify the gauge.
	mfs, err := f.reg.Gather()
	require.NoError(t, err)
	var seen bool
	for _, mf := range mfs {
		if mf.GetName() != "dialer_queue_size" {
			continue
		}
		for _, met := range mf.GetMetric() {
			if labelsMatch(met.GetLabel(), map[string]string{
				"tenant":  tenantID.String(),
				"project": projectID.String(),
			}) {
				require.InDelta(t, 3.0, met.GetGauge().GetValue(), 0.0)
				seen = true
			}
		}
	}
	require.True(t, seen, "Size must update dialer_queue_size gauge")
}

// TestRemove_EvictsRespondent — Remove drops the entry from both ZSET
// and dedup SET; idempotent on a missing entry.
func TestRemove_EvictsRespondent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	ok, err := f.q.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	})
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, f.q.Remove(ctx, tenantID, projectID, respID))

	zKey := "q:" + tenantID.String() + ":project:" + projectID.String()
	dKey := "qd:" + tenantID.String() + ":project:" + projectID.String()
	zCard, err := f.rdb.ZCard(ctx, zKey).Result()
	require.NoError(t, err)
	require.Zero(t, zCard, "Remove must ZREM the queued item")

	isMember, err := f.rdb.SIsMember(ctx, dKey, respID.String()).Result()
	require.NoError(t, err)
	require.False(t, isMember, "Remove must SREM the dedup entry")

	// Idempotent — removing an absent entry is fine.
	require.NoError(t, f.q.Remove(ctx, tenantID, projectID, respID))
}

// TestRemove_LeavesOtherRespondentsAlone — Remove must only evict the
// targeted respondent. Two queued items, one removal, one item left.
func TestRemove_LeavesOtherRespondentsAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	rA := uuid.New()
	rB := uuid.New()
	ctx := context.Background()

	enq := func(r uuid.UUID) {
		ok, err := f.q.EnqueueRespondent(ctx, api.EnqueueRequest{
			TenantID:     tenantID,
			ProjectID:    projectID,
			RespondentID: r,
			Phone:        "+79991234567",
			Region:       "RU-MOW",
			Priority:     5,
		})
		require.NoError(t, err)
		require.True(t, ok)
	}
	enq(rA)
	enq(rB)

	require.NoError(t, f.q.Remove(ctx, tenantID, projectID, rA))

	zKey := "q:" + tenantID.String() + ":project:" + projectID.String()
	zCard, err := f.rdb.ZCard(ctx, zKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), zCard)

	// rB still pops cleanly.
	item, err := f.q.PickNext(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, rB, item.RespondentID)
}

// TestPerTenantIsolation — two tenants enqueue under the same project
// uuid. Each tenant's queue is independent; PickNext on one does NOT
// drain the other.
func TestPerTenantIsolation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantA, tenantB, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	respA := uuid.New()
	respB := uuid.New()

	enq := func(tenant, resp uuid.UUID) {
		ok, err := f.q.EnqueueRespondent(ctx, api.EnqueueRequest{
			TenantID:     tenant,
			ProjectID:    projectID,
			RespondentID: resp,
			Phone:        "+79991234567",
			Region:       "RU-MOW",
			Priority:     5,
		})
		require.NoError(t, err)
		require.True(t, ok)
	}
	enq(tenantA, respA)
	enq(tenantB, respB)

	// Pop tenantA — must only see respA.
	item, err := f.q.PickNext(ctx, tenantA, projectID)
	require.NoError(t, err)
	require.Equal(t, respA, item.RespondentID)

	// tenantB queue still has respB.
	n, err := f.q.Size(ctx, tenantB, projectID)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

// TestEnqueue_RespectsTTLRefresh — every enqueue must EXPIRE both keys
// to the configured TTL. We assert the TTL via miniredis.
func TestEnqueue_RespectsTTLRefresh(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	q, err := queue.New(queue.Config{
		Redis:  rdb,
		Logger: zaptest.NewLogger(t),
		Clock:  clk.Now,
		TTL:    30 * time.Minute,
	})
	require.NoError(t, err)

	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()
	_, err = q.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	})
	require.NoError(t, err)

	zKey := "q:" + tenantID.String() + ":project:" + projectID.String()
	dKey := "qd:" + tenantID.String() + ":project:" + projectID.String()
	require.Equal(t, 30*time.Minute, mr.TTL(zKey))
	require.Equal(t, 30*time.Minute, mr.TTL(dKey))
}

// TestPickNext_ErrorOnCorruptBlob — if a malformed JSON ends up in the
// ZSET (e.g. a future schema migration left a legacy entry), PickNext
// surfaces a wrapped decode error. Defence-in-depth — the encoder is
// the only legitimate writer, so this path is hit only on real
// corruption.
func TestPickNext_ErrorOnCorruptBlob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	// Plant a bogus member directly into the ZSET. Note: it must still
	// be valid JSON (Lua's cjson.decode crashes on garbage and surfaces
	// the script error). Use a JSON document with a non-uuid
	// respondent_id so the Lua side passes but the Go decoder rejects.
	zKey := "q:" + tenantID.String() + ":project:" + projectID.String()
	dKey := "qd:" + tenantID.String() + ":project:" + projectID.String()
	bogus := `{"tenant_id":"00000000-0000-0000-0000-000000000000","project_id":"00000000-0000-0000-0000-000000000000","respondent_id":"not-a-uuid","priority":1,"enqueued_at_ms":0,"attempt_n":0,"phone":"","region":""}`
	require.NoError(t, f.rdb.ZAdd(ctx, zKey, redis.Z{Score: 1, Member: bogus}).Err())
	require.NoError(t, f.rdb.SAdd(ctx, dKey, "not-a-uuid").Err())

	_, err := f.q.PickNext(ctx, tenantID, projectID)
	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "decode") || strings.Contains(err.Error(), "respondent_id"),
		"want decode-related error, got %q", err.Error(),
	)
	require.InDelta(t, 1.0, f.counterValue(t, "dialer_queue_pickup_total", map[string]string{"result": "error"}), 0.0)
}

// TestRedisQueue_NilMetrics — a queue built without Metrics still
// works. nil-tolerant by contract; ensures no observe* path panics for
// any of the four state-changing operations.
func TestRedisQueue_NilMetrics(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	q, err := queue.New(queue.Config{Redis: rdb})
	require.NoError(t, err)

	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()
	_, err = q.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	})
	require.NoError(t, err)

	item, err := q.PickNext(ctx, tenantID, projectID)
	require.NoError(t, err)

	// Requeue + Size both flow through nil-metrics observers.
	require.NoError(t, q.Requeue(ctx, item, time.Second))
	n, err := q.Size(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	require.NoError(t, q.Remove(ctx, tenantID, projectID, respID))

	// Empty pop → ErrQueueEmpty path with nil metrics.
	_, err = q.PickNext(ctx, tenantID, projectID)
	require.ErrorIs(t, err, api.ErrQueueEmpty)
}

// TestRegisterMetrics_NilPanics — wiring error: passing nil to
// RegisterMetrics fails fast with a remediation message.
func TestRegisterMetrics_NilPanics(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t,
		"queue.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests",
		func() { queue.RegisterMetrics(nil) })
}

// TestQueueOps_PropagateRedisTransportErrors — every method must wrap the
// underlying redis error rather than swallow it. We force the error by
// closing the client BEFORE issuing the call. The miniredis server stays
// running; the closed client surfaces a transport-level error from
// go-redis. The test exercises every method (and therefore every metric
// "error" branch).
func TestQueueOps_PropagateRedisTransportErrors(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	reg := prometheus.NewRegistry()
	m := queue.RegisterMetrics(reg)
	q, err := queue.New(queue.Config{
		Redis:   rdb,
		Logger:  zaptest.NewLogger(t),
		Metrics: m,
	})
	require.NoError(t, err)

	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	// Close the client; subsequent calls fail with a transport error.
	require.NoError(t, rdb.Close())

	_, err = q.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
		Priority:     5,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "queue/enqueue")

	_, err = q.PickNext(ctx, tenantID, projectID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "queue/pick")

	require.Error(t, q.Requeue(ctx, api.QueueItem{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Priority:     5,
		EnqueuedAt:   time.Now().UTC(),
		Phone:        "+79991234567",
		Region:       "RU-MOW",
	}, 0))

	_, err = q.Size(ctx, tenantID, projectID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "queue/size")

	require.Error(t, q.Remove(ctx, tenantID, projectID, respID))

	// Verify the error metrics fired.
	gather := func(name string, label map[string]string) float64 {
		mfs, err := reg.Gather()
		require.NoError(t, err)
		for _, mf := range mfs {
			if mf.GetName() != name {
				continue
			}
			for _, met := range mf.GetMetric() {
				if labelsMatch(met.GetLabel(), label) {
					return met.GetCounter().GetValue()
				}
			}
		}
		return 0
	}
	require.GreaterOrEqual(t, gather("dialer_queue_enqueue_total", map[string]string{"result": "error"}), 1.0)
	require.GreaterOrEqual(t, gather("dialer_queue_pickup_total", map[string]string{"result": "error"}), 1.0)
}
