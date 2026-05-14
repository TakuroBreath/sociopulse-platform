// Package service owns the analytics module's long-running runtime —
// the IngestPipeline that drains the ANALYTICS JetStream into ClickHouse
// via per-subject batched inserts, and (Plan 13.2 Task 4+) the
// MetricsQuery implementation that backs the dashboard HTTP layer.
//
// Lifecycle: cmd/worker constructs an IngestPipeline and calls Run in
// its errgroup. cmd/api constructs a MetricsQuery via analytics.Module.
// The two paths share no state — they communicate ONLY through
// ClickHouse, which is the canonical durable boundary.
//
// The IngestPipeline is NOT leader-elected (per Plan 13.2 Q11 + the
// design: NATS push-consumer queue groups already provide horizontal
// scaling — each replica owns a subset of the message load). The
// pipeline's only ambient state is the per-subject dedup LRU defined in
// this file.
package service

import (
	"container/list"
	"sync"

	"github.com/google/uuid"
)

// DedupLRU is a fixed-capacity, goroutine-safe LRU keyed by uuid.UUID.
//
// The ingest pipeline keeps one DedupLRU per subject so a redelivered
// event (NATS at-least-once semantics) is dropped before it reaches the
// batch buffer. Capacity (typically 100_000 per Plan 13.2 spec) bounds
// the memory cost; once the LRU is full the LEAST-RECENTLY-INSERTED-OR-
// SEEN id is evicted to make room for the new one.
//
// Backing store: container/list (a doubly-linked list — O(1) push-to-
// front, remove-from-back, splice) + map[uuid.UUID]*list.Element for
// O(1) lookup. NOT sync.Map — that's for read-heavy workloads with
// stable keys; the ingester writes every Add, which would invalidate
// sync.Map's amortised-O(1) advantage.
type DedupLRU struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List
	index map[uuid.UUID]*list.Element
}

// NewDedupLRU constructs a DedupLRU with the supplied capacity. Capacity
// MUST be > 0 — a zero-capacity LRU is a programmer error (every Add
// would either trivially evict or wedge the eviction branch), so we
// panic at construction time. The IngestPipeline validates this at
// boot via its IngestConfig.Validate, so a panic here surfaces only
// when a test passes 0 directly.
func NewDedupLRU(capacity int) *DedupLRU {
	if capacity <= 0 {
		panic("analytics/service: DedupLRU capacity must be positive")
	}
	return &DedupLRU{
		cap:   capacity,
		ll:    list.New(),
		index: make(map[uuid.UUID]*list.Element, capacity),
	}
}

// AddResult is the rich return value of DedupLRU.Add. It surfaces two
// "best-effort missing-from-dedup-history" signals that the
// IngestPipeline's dedup_miss_total counter consumes:
//
//   - Dup    — id was already present (a true dedup hit; the row should
//     be dropped).
//   - WasEmpty — the LRU had zero entries BEFORE this Add. Reflects the
//     cold-start case where a consumer just restarted with no
//     in-memory history; any incoming row could be a redelivery of a
//     row already in ClickHouse. The signal fires exactly once per
//     freshly-constructed LRU (after the first non-dup Add the LRU is
//     non-empty), which is enough for the cold-restart probe.
//   - Evicted — this Add forced an eviction of the oldest entry to
//     make room. Reflects the LRU-saturation case where the evicted
//     id is no longer tracked, so a future redelivery of that
//     particular id would slip past the in-memory dedup. Best-effort
//     under-counting: it does not fire for the id that just got
//     evicted, but for the new id whose insertion caused the eviction
//     — close enough for "LRU is undersized, tune it" alerting.
//
// Callers that ignore the signals and only want the Dup bit should
// read result.Dup; the legacy boolean contract is preserved this way.
type AddResult struct {
	Dup      bool
	WasEmpty bool
	Evicted  bool
}

// Add inserts id into the LRU and returns an AddResult describing the
// outcome. See AddResult for the full semantics of the three flags.
//
// Both Dup and non-Dup paths promote id to MRU — a dup-hit refreshes
// the id's recency, and a newly-inserted id is by construction the
// most-recent. The IngestPipeline observes the return value to bump
// the per-subject "deduped" / "dedup-miss" metrics without ever
// touching the buffer.
func (l *DedupLRU) Add(id uuid.UUID) AddResult {
	l.mu.Lock()
	defer l.mu.Unlock()

	if el, ok := l.index[id]; ok {
		l.ll.MoveToFront(el)
		return AddResult{Dup: true}
	}

	res := AddResult{WasEmpty: l.ll.Len() == 0}

	if l.ll.Len() >= l.cap {
		oldest := l.ll.Back()
		if oldest != nil {
			// Values are written ONLY by Add (this file) and the type
			// is invariant. The comma-ok keeps the linter happy and
			// defends against a future refactor that accidentally
			// pushes the wrong type into the list.
			if oldestID, ok := oldest.Value.(uuid.UUID); ok {
				delete(l.index, oldestID)
			}
			l.ll.Remove(oldest)
			res.Evicted = true
		}
	}
	el := l.ll.PushFront(id)
	l.index[id] = el
	return res
}

// Has reports whether id is currently in the LRU. It is a read-only
// probe — it does NOT promote the id. Used by tests; the production
// hot-path uses Add and inspects its return value.
//
// Tests rely on the "no-promotion" contract — see
// TestDedupLRU_HasDoesNotPromote.
func (l *DedupLRU) Has(id uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	_, ok := l.index[id]
	return ok
}

// Len returns the current number of entries in the LRU (≤ capacity).
// Goroutine-safe.
func (l *DedupLRU) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ll.Len()
}
