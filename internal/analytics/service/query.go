package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/metrics"
	"github.com/sociopulse/platform/internal/analytics/store"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
)

// ErrInvalidQueryConfig is returned by QueryConfig.Validate when a
// required TTL or threshold field is zero or negative. Failures wrap
// the sentinel so callers can errors.Is against it.
var ErrInvalidQueryConfig = errors.New("analytics/service: invalid query config")

// StoreReader is the narrow read-side port the QueryService depends
// on. Mirrors the IngestPipeline's StoreWriter port pattern.
//
// Production: *StoreReaderAdapter satisfies it by delegating to the
// package-level store.*ByMV helpers. Tests: a fakeStoreReader (see
// query_test.go) records calls for assertion.
//
// Keeping the store helpers as package-level functions (not methods
// on *store.Conn) lets the schema-shape tests in internal/analytics/store
// remain independent of port-interface drift in this package.
type StoreReader interface {
	CallsByMV(ctx context.Context, q apianalytics.CallsQuery) (apianalytics.CallsResult, error)
	OperatorStateByMV(ctx context.Context, q apianalytics.OperatorStateQuery) (apianalytics.OperatorStateBreakdown, error)
	RegionProgressDoneByMV(ctx context.Context, q apianalytics.RegionProgressQuery) (map[string]uint64, error)
	HourlyByMV(ctx context.Context, q apianalytics.HourlyQuery) ([]apianalytics.HourlyBucket, error)
	OperatorComparisonsByMV(ctx context.Context, q apianalytics.OperatorComparisonsQuery) ([]apianalytics.OperatorComparisonRow, error)
}

// StoreReaderAdapter wraps a *store.Conn and dispatches to the
// package-level store.*ByMV helpers. The production composition root
// constructs one of these and passes it to NewQueryService.
type StoreReaderAdapter struct {
	Conn *store.Conn
}

// Compile-time interface check: *StoreReaderAdapter must satisfy
// StoreReader. Catches signature drift at build time.
var _ StoreReader = (*StoreReaderAdapter)(nil)

// CallsByMV delegates to store.CallsByMV.
func (a *StoreReaderAdapter) CallsByMV(ctx context.Context, q apianalytics.CallsQuery) (apianalytics.CallsResult, error) {
	return store.CallsByMV(ctx, a.Conn, q)
}

// OperatorStateByMV delegates to store.OperatorStateByMV.
func (a *StoreReaderAdapter) OperatorStateByMV(ctx context.Context, q apianalytics.OperatorStateQuery) (apianalytics.OperatorStateBreakdown, error) {
	return store.OperatorStateByMV(ctx, a.Conn, q)
}

// RegionProgressDoneByMV delegates to store.RegionProgressDoneByMV.
func (a *StoreReaderAdapter) RegionProgressDoneByMV(ctx context.Context, q apianalytics.RegionProgressQuery) (map[string]uint64, error) {
	return store.RegionProgressDoneByMV(ctx, a.Conn, q)
}

// HourlyByMV delegates to store.HourlyByMV.
func (a *StoreReaderAdapter) HourlyByMV(ctx context.Context, q apianalytics.HourlyQuery) ([]apianalytics.HourlyBucket, error) {
	return store.HourlyByMV(ctx, a.Conn, q)
}

// OperatorComparisonsByMV delegates to store.OperatorComparisonsByMV.
func (a *StoreReaderAdapter) OperatorComparisonsByMV(ctx context.Context, q apianalytics.OperatorComparisonsQuery) ([]apianalytics.OperatorComparisonRow, error) {
	return store.OperatorComparisonsByMV(ctx, a.Conn, q)
}

// CrmReader is the narrow analytics-side port for the crm module.
// Satisfied by the production *crm.api.ProjectService implementation
// (resolved via Deps.Locator at module.Register time). Carries ONLY
// the GetProgress method the analytics service needs — the wider
// ProjectService surface (Create/Archive/List/...) is irrelevant here.
//
// When the locator has no crm registration at boot, QueryService is
// constructed with crm=nil and RegionProgress falls back to Plan=0
// (Plan 13.2 § Q12).
type CrmReader interface {
	GetProgress(ctx context.Context, projectID uuid.UUID) (*crmapi.ProjectProgress, error)
}

// QueryConfig is the validated runtime configuration for cache TTL
// policy. All three fields must be > 0.
//
// CacheShortTTL applies when window.To-From ≤ LongWindowThreshold —
// typically 30 s for hot dashboards.
// CacheLongTTL applies when window.To-From > LongWindowThreshold —
// typically 5 min for archival look-backs.
// LongWindowThreshold is the boundary between the two — typically 24 h.
type QueryConfig struct {
	CacheShortTTL       time.Duration
	CacheLongTTL        time.Duration
	LongWindowThreshold time.Duration
}

// Validate returns nil iff every required field is > 0. Failures wrap
// ErrInvalidQueryConfig so callers can errors.Is against the sentinel.
func (c QueryConfig) Validate() error {
	if c.CacheShortTTL <= 0 {
		return fmt.Errorf("%w: CacheShortTTL must be > 0 (got %s)", ErrInvalidQueryConfig, c.CacheShortTTL)
	}
	if c.CacheLongTTL <= 0 {
		return fmt.Errorf("%w: CacheLongTTL must be > 0 (got %s)", ErrInvalidQueryConfig, c.CacheLongTTL)
	}
	if c.LongWindowThreshold <= 0 {
		return fmt.Errorf("%w: LongWindowThreshold must be > 0 (got %s)", ErrInvalidQueryConfig, c.LongWindowThreshold)
	}
	return nil
}

// QueryService implements api.MetricsQuery + api.ServiceRO. Reads
// AggregatingMergeTree MVs via the StoreReader port, wraps with
// read-through Redis cache, and joins RegionProgress.Plan from the
// crm.api.ProjectService port.
//
// The cache contract is non-fatal: a Get error is treated as a miss;
// a Set error is logged at debug + swallowed. This keeps the dashboard
// live when Redis is degraded.
type QueryService struct {
	store   StoreReader
	cache   Cache
	crm     CrmReader
	logger  *zap.Logger
	metrics *metrics.QueryMetrics
	cfg     QueryConfig
}

// Compile-time interface checks. *QueryService must satisfy both the
// narrow MetricsQuery surface AND the wider ServiceRO (= MetricsQuery
// + Overview).
var (
	_ apianalytics.MetricsQuery = (*QueryService)(nil)
	_ apianalytics.ServiceRO    = (*QueryService)(nil)
)

// NewQueryService constructs a *QueryService. storeReader is required;
// cache is optional (nil → no caching, every call hits the store); crm
// is optional (nil → RegionProgress.Plan=0 per Q12); logger
// nil-falls-back to a nop; m is nil-safe.
//
// cfg.Validate runs synchronously — an invalid config surfaces at
// boot, not at first query.
func NewQueryService(
	storeReader StoreReader,
	cache Cache,
	crm CrmReader,
	logger *zap.Logger,
	m *metrics.QueryMetrics,
	cfg QueryConfig,
) (*QueryService, error) {
	if storeReader == nil {
		return nil, errors.New("analytics/service: NewQueryService: store reader must be non-nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if cache == nil {
		// Use a no-op cache so callers don't need to special-case nil
		// everywhere. noopCache lives in cache.go (this package).
		cache = noopCache{}
	}
	return &QueryService{
		store:   storeReader,
		cache:   cache,
		crm:     crm,
		logger:  logger,
		metrics: m,
		cfg:     cfg,
	}, nil
}

// Calls implements api.MetricsQuery. Read-through cache: GET → if hit,
// return; if miss → query CH → cache result → return. Validates
// TenantID + Window before any I/O.
func (s *QueryService) Calls(ctx context.Context, q apianalytics.CallsQuery) (apianalytics.CallsResult, error) {
	const method = "calls"
	if q.TenantID == uuid.Nil {
		return apianalytics.CallsResult{}, apianalytics.ErrTenantRequired
	}
	if err := q.Window.Validate(); err != nil {
		return apianalytics.CallsResult{}, err
	}

	start := time.Now()
	defer func() { s.metrics.ObserveDuration(method, time.Since(start).Seconds()) }()

	key := CacheKey(q.TenantID, method, q)
	if raw, hit := s.cacheGet(ctx, key); hit {
		var cached apianalytics.CallsResult
		if err := json.Unmarshal(raw, &cached); err == nil {
			s.metrics.IncCacheHit(method)
			return cached, nil
		}
		s.logger.Debug("analytics: stale cache entry — falling through to CH", zap.String("key", key))
	}
	s.metrics.IncCacheMiss(method)

	res, err := s.store.CallsByMV(ctx, q)
	if err != nil {
		return apianalytics.CallsResult{}, fmt.Errorf("analytics/service: query calls: %w", err)
	}

	s.cacheSet(ctx, key, res, s.ttl(q.Window), method)
	return res, nil
}

// OperatorState implements api.MetricsQuery.
func (s *QueryService) OperatorState(ctx context.Context, q apianalytics.OperatorStateQuery) (apianalytics.OperatorStateBreakdown, error) {
	const method = "operator_state"
	if q.TenantID == uuid.Nil {
		return apianalytics.OperatorStateBreakdown{}, apianalytics.ErrTenantRequired
	}
	if err := q.Window.Validate(); err != nil {
		return apianalytics.OperatorStateBreakdown{}, err
	}

	start := time.Now()
	defer func() { s.metrics.ObserveDuration(method, time.Since(start).Seconds()) }()

	key := CacheKey(q.TenantID, method, q)
	if raw, hit := s.cacheGet(ctx, key); hit {
		var cached apianalytics.OperatorStateBreakdown
		if err := json.Unmarshal(raw, &cached); err == nil {
			s.metrics.IncCacheHit(method)
			return cached, nil
		}
		s.logger.Debug("analytics: stale cache entry — falling through to CH", zap.String("key", key))
	}
	s.metrics.IncCacheMiss(method)

	res, err := s.store.OperatorStateByMV(ctx, q)
	if err != nil {
		return apianalytics.OperatorStateBreakdown{}, fmt.Errorf("analytics/service: query operator_state: %w", err)
	}

	s.cacheSet(ctx, key, res, s.ttl(q.Window), method)
	return res, nil
}

// RegionProgress implements api.MetricsQuery. Special case: zips the
// store's done counts with the crm port's Plan total. Nil crm reader
// or crm error → Plan=0 (Q12 fallback).
func (s *QueryService) RegionProgress(ctx context.Context, q apianalytics.RegionProgressQuery) ([]apianalytics.RegionProgressRow, error) {
	const method = "region_progress"
	if q.TenantID == uuid.Nil {
		return nil, apianalytics.ErrTenantRequired
	}
	if err := q.Window.Validate(); err != nil {
		return nil, err
	}

	start := time.Now()
	defer func() { s.metrics.ObserveDuration(method, time.Since(start).Seconds()) }()

	key := CacheKey(q.TenantID, method, q)
	if raw, hit := s.cacheGet(ctx, key); hit {
		var cached []apianalytics.RegionProgressRow
		if err := json.Unmarshal(raw, &cached); err == nil {
			s.metrics.IncCacheHit(method)
			return cached, nil
		}
		s.logger.Debug("analytics: stale cache entry — falling through to CH", zap.String("key", key))
	}
	s.metrics.IncCacheMiss(method)

	done, err := s.store.RegionProgressDoneByMV(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("analytics/service: query region_progress: %w", err)
	}

	// Resolve quota plan from crm port. Best-effort: nil reader or any
	// error → Plan=0 (Q12 documented fallback).
	var plan uint64
	if s.crm != nil {
		progress, crmErr := s.crm.GetProgress(ctx, q.ProjectID)
		if crmErr != nil {
			s.logger.Debug("analytics: crm.GetProgress failed — Plan=0",
				zap.String("tenant_id", q.TenantID.String()),
				zap.String("project_id", q.ProjectID.String()),
				zap.Error(crmErr))
		} else if progress != nil && progress.TargetCount >= 0 {
			plan = uint64(progress.TargetCount)
		}
	}

	rows := make([]apianalytics.RegionProgressRow, 0, len(done))
	for region, n := range done {
		row := apianalytics.RegionProgressRow{
			RegionCode: region,
			Done:       n,
			Plan:       plan,
		}
		if plan > 0 {
			row.Progress = float64(n) / float64(plan)
		}
		rows = append(rows, row)
	}

	s.cacheSet(ctx, key, rows, s.ttl(q.Window), method)
	return rows, nil
}

// Hourly implements api.MetricsQuery.
func (s *QueryService) Hourly(ctx context.Context, q apianalytics.HourlyQuery) ([]apianalytics.HourlyBucket, error) {
	const method = "hourly"
	if q.TenantID == uuid.Nil {
		return nil, apianalytics.ErrTenantRequired
	}
	if err := q.Window.Validate(); err != nil {
		return nil, err
	}

	start := time.Now()
	defer func() { s.metrics.ObserveDuration(method, time.Since(start).Seconds()) }()

	key := CacheKey(q.TenantID, method, q)
	if raw, hit := s.cacheGet(ctx, key); hit {
		var cached []apianalytics.HourlyBucket
		if err := json.Unmarshal(raw, &cached); err == nil {
			s.metrics.IncCacheHit(method)
			return cached, nil
		}
		s.logger.Debug("analytics: stale cache entry — falling through to CH", zap.String("key", key))
	}
	s.metrics.IncCacheMiss(method)

	buckets, err := s.store.HourlyByMV(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("analytics/service: query hourly: %w", err)
	}

	s.cacheSet(ctx, key, buckets, s.ttl(q.Window), method)
	return buckets, nil
}

// OperatorComparisons implements api.MetricsQuery. The store returns
// raw per-operator stats; this method computes the team average and
// flips AboveTeamAvg for each row that beats it. DisplayName is left
// empty in v1 — a user-resolver port is not yet wired (TODO Plan 13.3
// or follow-up).
func (s *QueryService) OperatorComparisons(ctx context.Context, q apianalytics.OperatorComparisonsQuery) ([]apianalytics.OperatorComparisonRow, error) {
	const method = "operator_comparisons"
	if q.TenantID == uuid.Nil {
		return nil, apianalytics.ErrTenantRequired
	}
	if err := q.Window.Validate(); err != nil {
		return nil, err
	}

	start := time.Now()
	defer func() { s.metrics.ObserveDuration(method, time.Since(start).Seconds()) }()

	key := CacheKey(q.TenantID, method, q)
	if raw, hit := s.cacheGet(ctx, key); hit {
		var cached []apianalytics.OperatorComparisonRow
		if err := json.Unmarshal(raw, &cached); err == nil {
			s.metrics.IncCacheHit(method)
			return cached, nil
		}
		s.logger.Debug("analytics: stale cache entry — falling through to CH", zap.String("key", key))
	}
	s.metrics.IncCacheMiss(method)

	rows, err := s.store.OperatorComparisonsByMV(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("analytics/service: query operator_comparisons: %w", err)
	}

	// Compute team-avg success rate and flip AboveTeamAvg.
	if len(rows) > 0 {
		var sum float64
		for _, r := range rows {
			sum += r.SuccessRate
		}
		teamAvg := sum / float64(len(rows))
		for i := range rows {
			rows[i].AboveTeamAvg = rows[i].SuccessRate > teamAvg
		}
	}

	s.cacheSet(ctx, key, rows, s.ttl(q.Window), method)
	return rows, nil
}

// Overview implements api.ServiceRO. Composes four sub-queries
// (Calls, OperatorState, RegionProgress, Hourly) into one round-trip
// response. Each sub-query independently re-uses its own cache entry,
// so the hot-dashboard pattern (repeated Overview calls) hits cached
// sub-results on every refresh.
//
// ProjectID is optional on OverviewQuery, but RegionProgress requires
// a concrete ProjectID — if nil, we skip the RegionProgress section
// entirely (returns empty slice).
func (s *QueryService) Overview(ctx context.Context, q apianalytics.OverviewQuery) (apianalytics.OverviewResult, error) {
	if q.TenantID == uuid.Nil {
		return apianalytics.OverviewResult{}, apianalytics.ErrTenantRequired
	}
	if err := q.Window.Validate(); err != nil {
		return apianalytics.OverviewResult{}, err
	}

	var res apianalytics.OverviewResult

	calls, err := s.Calls(ctx, apianalytics.CallsQuery{
		TenantID:  q.TenantID,
		ProjectID: q.ProjectID,
		Window:    q.Window,
	})
	if err != nil {
		return apianalytics.OverviewResult{}, fmt.Errorf("analytics/service: overview calls: %w", err)
	}
	res.Calls = calls

	opState, err := s.OperatorState(ctx, apianalytics.OperatorStateQuery{
		TenantID:  q.TenantID,
		ProjectID: q.ProjectID,
		Window:    q.Window,
	})
	if err != nil {
		return apianalytics.OverviewResult{}, fmt.Errorf("analytics/service: overview operator_state: %w", err)
	}
	res.OperatorState = opState

	if q.ProjectID != nil {
		regions, err := s.RegionProgress(ctx, apianalytics.RegionProgressQuery{
			TenantID:  q.TenantID,
			ProjectID: *q.ProjectID,
			Window:    q.Window,
		})
		if err != nil {
			return apianalytics.OverviewResult{}, fmt.Errorf("analytics/service: overview region_progress: %w", err)
		}
		res.RegionProgress = regions
	}

	hourly, err := s.Hourly(ctx, apianalytics.HourlyQuery{
		TenantID:  q.TenantID,
		ProjectID: q.ProjectID,
		Window:    q.Window,
	})
	if err != nil {
		return apianalytics.OverviewResult{}, fmt.Errorf("analytics/service: overview hourly: %w", err)
	}
	res.Hourly = hourly

	return res, nil
}

// ttl picks short or long TTL based on window span vs. configured
// threshold. Short for hot dashboards (window ≤ 24h); long for
// archival look-backs.
func (s *QueryService) ttl(w apianalytics.Window) time.Duration {
	if w.To.Sub(w.From) <= s.cfg.LongWindowThreshold {
		return s.cfg.CacheShortTTL
	}
	return s.cfg.CacheLongTTL
}

// cacheGet wraps s.cache.Get with the non-fatal contract: a transport
// error is logged at debug + treated as miss. The cached value (if any)
// is returned as raw bytes; the caller json.Unmarshals into its own
// concrete DTO type.
func (s *QueryService) cacheGet(ctx context.Context, key string) ([]byte, bool) {
	raw, hit, err := s.cache.Get(ctx, key)
	if err != nil {
		s.logger.Debug("analytics: cache get failed", zap.String("key", key), zap.Error(err))
		return nil, false
	}
	return raw, hit
}

// cacheSet wraps s.cache.Set with the best-effort contract: a marshal
// or transport error is logged at debug + swallowed. The method label
// is passed through for observability (which sub-query produced the
// stale write).
func (s *QueryService) cacheSet(ctx context.Context, key string, v any, ttl time.Duration, method string) {
	raw, err := json.Marshal(v)
	if err != nil {
		s.logger.Debug("analytics: cache marshal failed",
			zap.String("key", key), zap.String("method", method), zap.Error(err))
		return
	}
	if err := s.cache.Set(ctx, key, raw, ttl); err != nil {
		s.logger.Debug("analytics: cache set failed",
			zap.String("key", key), zap.String("method", method), zap.Error(err))
	}
}

// CacheKey builds "analytics:{tenant}:{method}:{q_hash}" where q_hash
// is the first 32 hex chars (16 bytes) of SHA-256 over canonical-JSON
// of the query struct. SHA-256 is used (NOT MD5, NOT FNV) per the
// project depguard rule blocking weak hashes.
//
// Exported so the unit tests in query_test.go can construct the same
// key shape when pre-warming the cache.
//
// The hash includes the tenant_id implicitly (it is a field on every
// query DTO) AND explicitly in the key prefix, so a per-tenant
// invalidation pattern (Q6 follow-up: SCAN + DEL on
// `analytics:{tenant_id}:*`) is straightforward to add later.
func CacheKey(tenantID uuid.UUID, method string, q any) string {
	raw, err := json.Marshal(q)
	if err != nil {
		// Fallback: stable key without the query hash. This should never
		// happen in practice (every DTO marshals cleanly), but a hash
		// failure must not panic the request path.
		return fmt.Sprintf("analytics:%s:%s:invalid", tenantID, method)
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("analytics:%s:%s:%s", tenantID, method, hex.EncodeToString(sum[:16]))
}
