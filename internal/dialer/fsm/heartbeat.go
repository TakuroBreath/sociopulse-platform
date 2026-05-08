package fsm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// Default tunables. Source: Plan 10 §"Heartbeat" — operators refresh
// presence:<tid>:user:<id> every 10s with a 30s TTL; the watchdog
// scans every 30s and forces operators offline whose presence key has
// expired.
const (
	defaultHeartbeatInterval    = 30 * time.Second
	defaultHeartbeatScanCount   = 200
	defaultHeartbeatPresenceTTL = 30 * time.Second
)

// HeartbeatMetrics groups the watchdog's Prometheus collectors. Field
// is exported so tests can read counters directly. Same RegisterMetrics
// pattern as the FSM's main Metrics; collectors are not init-time
// registered to avoid duplicate-registration panics across test
// imports.
type HeartbeatMetrics struct {
	// Forced counts every Force(target=offline, reason=heartbeat_lost)
	// the watchdog issues. Spikes correlate with operator-side network
	// flakes or pod restarts.
	Forced prometheus.Counter

	// ScanDuration is the per-tick wall clock of one full SCAN+forces
	// pass. Helps operators detect a Redis slowdown that would push
	// the scan past the next ticker fire.
	ScanDuration prometheus.Histogram

	// ScanErrors counts the per-tick SCAN failures (network, OOM).
	ScanErrors prometheus.Counter
}

// RegisterHeartbeatMetrics builds + registers the Heartbeat watchdog's
// collectors on the supplied registerer. Mirrors RegisterMetrics's
// shape — caller passes prometheus.NewRegistry() in tests, the
// project-wide Metrics.Registry in production.
func RegisterHeartbeatMetrics(reg prometheus.Registerer) *HeartbeatMetrics {
	if reg == nil {
		panic("fsm.RegisterHeartbeatMetrics: reg must be non-nil")
	}
	m := &HeartbeatMetrics{
		Forced: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dialer_heartbeat_force_total",
			Help: "Total Force(target=offline, reason=heartbeat_lost) issued by the watchdog.",
		}),
		ScanDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "dialer_heartbeat_scan_duration_seconds",
			Help:    "Wall clock duration of one full operator-key scan.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}),
		ScanErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dialer_heartbeat_scan_errors_total",
			Help: "Total SCAN failures observed by the heartbeat watchdog.",
		}),
	}
	reg.MustRegister(m.Forced, m.ScanDuration, m.ScanErrors)
	return m
}

// HeartbeatConfig bundles the dependencies + tunable values for a
// Heartbeat watchdog. Required fields (Redis, FSM) are documented
// per-field; nil-tolerated fields fall back to safe defaults so the
// constructor stays trivially wireable from tests.
type HeartbeatConfig struct {
	// Redis is the connection used for the SCAN + presence-key
	// EXISTS lookups. Required.
	Redis *redis.Client

	// FSM is the operator FSM used to issue Force(target=offline,
	// reason=heartbeat_lost) when a presence key has expired.
	// Required. Production wiring passes the same *fsm.Machine the
	// HTTP handlers use; tests pass a fake.
	FSM api.OperatorFSM

	// Interval is the SCAN tick cadence. 0 → defaultHeartbeatInterval (30s).
	Interval time.Duration

	// ScanCount is the COUNT hint passed to SCAN. 0 →
	// defaultHeartbeatScanCount (200). Higher values reduce the number
	// of round-trips on large fleets at the cost of one slow scan
	// step blocking the Redis main thread for longer.
	ScanCount int64

	// Logger receives per-tick diagnostics. nil → zap.NewNop(). Per
	// Plan 09 carry-forward, fields are typed and never carry PII —
	// the watchdog only logs tenant + operator UUIDs.
	Logger *zap.Logger

	// Clock returns the current time. Reserved; nil → time.Now. Not
	// consumed by the v1 SCAN-driven implementation but kept on the
	// Config for symmetry with the rest of the dialer subsystems and
	// for a future TTL-watcher variant.
	Clock func() time.Time

	// Metrics is the per-watchdog collector group. nil → no metrics
	// (the watchdog is fully functional without it).
	Metrics *HeartbeatMetrics
}

// Heartbeat is the watchdog goroutine that polls operator presence
// keys and forces operators offline when their presence:<t>:user:<o>
// TTL has expired without a corresponding state==offline transition.
//
// Lifecycle:
//
//   - cmd/api Module.Register builds one Heartbeat per process and
//     starts Run inside a goroutine.
//   - Run blocks until ctx is cancelled. On cancel, the ticker is
//     stopped and the function returns ctx.Err().
//   - Module.Stop cancels the parent ctx; the Run goroutine drains
//     within one Interval (default 30s).
//
// Failure modes:
//
//   - SCAN error → log at warn, increment ScanErrors metric, retry on
//     the next tick. Never panics.
//   - Per-key parse failure → log at debug + skip; the key likely
//     belongs to a future Plan 11 schema we haven't been taught.
//   - Per-key presence-EXISTS error → log at debug + skip. A flaky
//     Redis path doesn't paint the whole sweep red.
//   - Force error → log at warn + skip; the watchdog's job is best-
//     effort eviction, not strong consistency.
type Heartbeat struct {
	rdb       *redis.Client
	fsm       api.OperatorFSM
	interval  time.Duration
	scanCount int64
	log       *zap.Logger
	clock     func() time.Time
	metrics   *HeartbeatMetrics
}

// NewHeartbeat constructs a Heartbeat. Returns an error when a
// required dependency is missing; nil-tolerated fields are filled
// with defaults.
func NewHeartbeat(cfg HeartbeatConfig) (*Heartbeat, error) {
	if cfg.Redis == nil {
		return nil, errors.New("fsm.NewHeartbeat: Redis is required")
	}
	if cfg.FSM == nil {
		return nil, errors.New("fsm.NewHeartbeat: FSM is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	scanCount := cfg.ScanCount
	if scanCount <= 0 {
		scanCount = defaultHeartbeatScanCount
	}
	return &Heartbeat{
		rdb:       cfg.Redis,
		fsm:       cfg.FSM,
		interval:  interval,
		scanCount: scanCount,
		log:       logger,
		clock:     clock,
		metrics:   cfg.Metrics,
	}, nil
}

// Run blocks until ctx cancels. Each tick executes one Sweep pass.
// The first sweep runs immediately on entry so the watchdog doesn't
// sit idle for a full interval after boot.
func (h *Heartbeat) Run(ctx context.Context) error {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	h.log.Info("heartbeat watchdog starting",
		zap.Duration("interval", h.interval),
		zap.Int64("scan_count", h.scanCount),
	)

	h.Sweep(ctx)

	for {
		select {
		case <-ctx.Done():
			h.log.Info("heartbeat watchdog stopped", zap.Error(ctx.Err()))
			return ctx.Err()
		case <-ticker.C:
			h.Sweep(ctx)
		}
	}
}

// Sweep performs one full SCAN+forces pass. Exposed for tests; in
// production callers go through Run.
//
// Walking the keyspace via SCAN (rather than KEYS) keeps the Redis
// main thread responsive even with thousands of operator hashes.
// Each batch is processed serially — the per-operator forces are
// rate-limited by the SCAN cursor cadence, which is what we want for
// a non-bursty rollout of force-offlines.
func (h *Heartbeat) Sweep(ctx context.Context) {
	start := h.clock()
	defer func() {
		if h.metrics != nil && h.metrics.ScanDuration != nil {
			h.metrics.ScanDuration.Observe(h.clock().Sub(start).Seconds())
		}
	}()

	var cursor uint64
	for {
		// Bail on early ctx cancel so a long sweep doesn't block
		// shutdown.
		if err := ctx.Err(); err != nil {
			return
		}
		keys, next, err := h.rdb.Scan(ctx, cursor, "op:*:user:*", h.scanCount).Result()
		if err != nil {
			if h.metrics != nil && h.metrics.ScanErrors != nil {
				h.metrics.ScanErrors.Inc()
			}
			h.log.Warn("heartbeat: SCAN failed", zap.Error(err))
			return
		}
		for _, key := range keys {
			h.checkKey(ctx, key)
		}
		if next == 0 {
			return
		}
		cursor = next
	}
}

// checkKey runs the per-key check: parse the (tenant, operator) ids
// off the key, look up the operator's current state, EXISTS the
// matching presence key, and on miss issue Force(offline,
// heartbeat_lost). Errors at any step are logged and short-circuited
// — this is best-effort eviction, not strong consistency.
func (h *Heartbeat) checkKey(ctx context.Context, key string) {
	tenantID, operatorID, ok := parseOpKey(key)
	if !ok {
		h.log.Debug("heartbeat: skipping unrecognised key", zap.String("key", key))
		return
	}

	// Read the current state from the hash. A missing or unrecognised
	// state is a corrupt row — skip rather than force.
	state, ok, err := h.lookupState(ctx, key)
	if err != nil {
		h.log.Debug("heartbeat: state lookup failed",
			zap.String("key", key),
			zap.Error(err),
		)
		return
	}
	if !ok {
		// Hash missing — already implicitly offline. Nothing to do.
		return
	}
	if state == api.StateOffline {
		// Already offline — nothing to do.
		return
	}

	// Presence key alive? Same prefix family as the op hash so a
	// future Cluster deployment can hash-tag both into the same slot.
	presenceKey := PresenceKey(tenantID, operatorID)
	exists, err := h.rdb.Exists(ctx, presenceKey).Result()
	if err != nil {
		h.log.Debug("heartbeat: presence EXISTS failed",
			zap.Stringer("tenant_id", tenantID),
			zap.Stringer("operator_id", operatorID),
			zap.Error(err),
		)
		return
	}
	if exists > 0 {
		// Presence is fresh; nothing to do.
		return
	}

	// Force the operator offline. Use a detached context with a
	// reasonable timeout so a single slow Force doesn't block the
	// remainder of the sweep.
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := h.fsm.Force(fctx, tenantID, operatorID, api.StateOffline, api.ForceReasonHeartbeatLost); err != nil {
		// Don't escalate to error: the operator may have just
		// transitioned to offline themselves between the EXISTS
		// check and our Force, in which case Force returns the
		// (idempotent) current snapshot — but a transport-level
		// failure also lands here. Warn so operators notice patterns.
		h.log.Warn("heartbeat: force-offline failed",
			zap.Stringer("tenant_id", tenantID),
			zap.Stringer("operator_id", operatorID),
			zap.Error(err),
		)
		return
	}
	if h.metrics != nil && h.metrics.Forced != nil {
		h.metrics.Forced.Inc()
	}
	h.log.Info("heartbeat: forced operator offline (presence expired)",
		zap.Stringer("tenant_id", tenantID),
		zap.Stringer("operator_id", operatorID),
	)
}

// lookupState reads only the `state` field of the operator hash so
// the check is cheap (HGET, not HGETALL). Returns:
//
//   - state, true, nil  → hash present, state parseable.
//   - "",    false, nil → hash missing (entry was already evicted).
//   - "",    false, err → transport / parse failure.
func (h *Heartbeat) lookupState(ctx context.Context, key string) (api.State, bool, error) {
	v, err := h.rdb.HGet(ctx, key, "state").Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("hget state: %w", err)
	}
	if v == "" {
		return "", false, nil
	}
	s := api.State(v)
	if !s.Valid() {
		return "", false, fmt.Errorf("unrecognised state %q", v)
	}
	return s, true, nil
}

// parseOpKey extracts the tenant / operator UUIDs from a Redis key in
// the canonical form "op:<tenant>:user:<operator>". Returns ok=false
// for any other shape; callers skip such keys.
//
// The shape is mirrored by opKey in store.go — tightly coupled by
// design; a regression here surfaces as missing forces under load,
// which the watchdog metrics call out (Forced counter stays flat
// while operators visibly drop off).
func parseOpKey(key string) (uuid.UUID, uuid.UUID, bool) {
	const opPrefix = "op:"
	const userMid = ":user:"
	if !strings.HasPrefix(key, opPrefix) {
		return uuid.Nil, uuid.Nil, false
	}
	rest := key[len(opPrefix):]
	idx := strings.Index(rest, userMid)
	if idx <= 0 {
		return uuid.Nil, uuid.Nil, false
	}
	tenantStr := rest[:idx]
	operatorStr := rest[idx+len(userMid):]
	tid, err := uuid.Parse(tenantStr)
	if err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	oid, err := uuid.Parse(operatorStr)
	if err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	return tid, oid, true
}

// PresenceKey returns the canonical Redis key holding an operator's
// heartbeat presence. Exported so HTTP/WS handlers can refresh the
// key on every operator request via RefreshPresence.
//
// The prefix is symmetric with opKey — a Cluster deployment can
// hash-tag both keys into the same slot if needed. Plan 10 §"Heartbeat"
// pins the TTL at 30s; the operator UI refreshes every 10s.
func PresenceKey(tenantID, operatorID uuid.UUID) string {
	return "presence:" + tenantID.String() + ":user:" + operatorID.String()
}

// RefreshPresence sets / refreshes the operator presence key with the
// supplied TTL. Designed to be called from HTTP middleware or the WS
// keep-alive path on every operator-driven request. ttl <= 0 falls
// back to defaultHeartbeatPresenceTTL (30s) — the value that pairs
// with the watchdog's 30s sweep cadence.
//
// Best-effort: a Redis transport failure is returned to the caller so
// it can decide whether to log; the FSM hot path should NEVER block
// on presence-refresh failures (a missing presence merely means the
// watchdog will eventually evict the operator on its next sweep, by
// which point the operator's UI will surface the disconnect).
func RefreshPresence(ctx context.Context, rdb *redis.Client, tenantID, operatorID uuid.UUID, ttl time.Duration) error {
	if rdb == nil {
		return errors.New("fsm.RefreshPresence: rdb is required")
	}
	if ttl <= 0 {
		ttl = defaultHeartbeatPresenceTTL
	}
	if err := rdb.Set(ctx, PresenceKey(tenantID, operatorID), "1", ttl).Err(); err != nil {
		return fmt.Errorf("fsm.RefreshPresence: set: %w", err)
	}
	return nil
}
