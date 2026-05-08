// Package router selects the {fs_node, trunk} pair for a given operator + phone +
// strategy and enforces per-node line-capacity tracking via Redis.
//
// Plan 09 Task 5 ships the real implementation. Composition:
//
//   - cfg.Telephony.Trunks (config-only catalog) is materialised at New time
//     into a flat []Trunk. Plan 09 deliberately does NOT load the catalog
//     from a Postgres telephony_trunks table — adding a migration mid-Plan-9
//     expands scope. Hot-reload via cfg.Snapshot is a Plan 13/14 hardening.
//
//   - Strategy selection is by name (api.RoutingStrategy). req.Strategy is
//     consulted first, then DefaultStrategy from config, then LeastCost as
//     the global default. The four strategies live in strategy.go.
//
//   - FS-node selection intersects the chosen trunk's NodeAddrs with the
//     pool's HealthyNodes() set, then walks the intersection in
//     deterministic order (lex-sorted) trying TryAcquire on each. The first
//     success wins. The Pool dependency is injected as an interface so tests
//     can supply a stub without spinning up the real ESLPool fleet.
//
// Plan-spec deviation: the original plan draft (lines 1782-2308) opens a
// pgxpool and runs a 30-second refresh loop against telephony_trunks. We
// defer that to Plan 13/14 because:
//
//  1. cfg.Telephony.Trunks already exists in pkg/config and Helm values can
//     ship the catalog today — no operator workflow waits on this.
//  2. Migrating mid-plan blocks the dialer (Plan 10) on a schema change that
//     has no other consumer.
//  3. Hot-reload of trunk catalog is a Plan 13/14 concern (cfg.Snapshot
//     watches the file already; the router can subscribe later).
//
// The Start() method is therefore a no-op — preserved as the contract surface
// the composition root expects, ready for a future Postgres refresher.
package router

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/pkg/config"
)

// Pool is the subset of *pool.ESLPool the router consumes. Defining it here
// (rather than depending on the concrete *pool.ESLPool) lets tests inject a
// stub without standing up the real ESL fleet — and keeps the router
// decoupled from a concrete pool implementation should we add a second
// flavour (e.g. multi-region) later. The real *pool.ESLPool satisfies this
// interface implicitly.
type Pool interface {
	// HealthyNodes returns the addresses of FS nodes the pool currently
	// considers healthy, in deterministic order (lex-sorted by the real
	// implementation). Router.Select intersects this list with the chosen
	// trunk's NodeAddrs to find a dispatch target.
	HealthyNodes() []string
}

// Config holds the router's wiring. Field types stay compatible with the
// skeleton shipped in Plan 09 Task 1 so the composition root does not churn.
type Config struct {
	// Pool is the ESL fleet the router queries for healthy nodes. Required
	// — New returns an error if Pool is nil.
	Pool Pool

	// Redis is the project-wide Redis client used for the per-node
	// active-channel counter. Required — New returns an error if Redis is
	// nil.
	Redis *redis.Client

	// BackpressureCap is the per-node concurrent-call ceiling. Zero falls
	// back to NewBackpressure's default (60) so a misconfigured Helm value
	// does not silently disable the gate.
	BackpressureCap int

	// Trunks is the trunk catalog. Sourced from cfg.Telephony.Trunks in
	// production; tests build the slice directly. An empty slice is
	// tolerated — Router.Select will return ErrNoTrunkAvailable on every
	// call, which is the correct degraded behaviour while operators
	// finish wiring trunks.
	Trunks []config.TrunkConfig

	// DefaultStrategy is the api.RoutingStrategy name used when
	// req.Strategy is empty. Empty (or unknown) falls back to LeastCost.
	DefaultStrategy string

	// Logger is named for the router subsystem; nil-tolerated.
	Logger *zap.Logger

	// Metrics receives Selects/SelectDuration/BackpressureRejects updates;
	// nil-tolerated. The composition root builds metrics via
	// RegisterMetrics(metrics.Registry) and passes the result here.
	Metrics *Metrics
}

// Router selects {trunk, fs_node} for a given operator+region. It is the
// real implementation of api.Router.
type Router struct {
	pool         Pool
	bp           *Backpressure
	logger       *zap.Logger
	metrics      *Metrics
	defaultStrat string

	// trunks is the materialised catalog. Frozen at New time; protected by
	// trunksMu so a future hot-reload (Plan 13) can swap the slice
	// atomically.
	trunksMu sync.RWMutex
	trunks   []Trunk

	// strategies maps the api.RoutingStrategy string onto a Strategy
	// instance. RoundRobin is per-router-instance because its atomic
	// counter is value state — sharing a global instance across operators
	// would interleave their picks.
	strategies map[string]Strategy
}

// New constructs a Router. Returns an error on missing required wiring;
// validation here lets the composition root surface the misconfiguration at
// boot rather than at first Select call.
func New(cfg Config) (*Router, error) {
	if cfg.Pool == nil {
		return nil, errors.New("router: Pool is required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("router: Redis is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	// Materialise the config catalog into the strategy-facing Trunk shape.
	// Active mapping: a trunk is "active" iff it has at least one node
	// configured AND a positive capacity. CapacityChannels=0 is operator
	// shorthand for "drained — do not route" (matches Plan 13 stats
	// collector intent).
	trunks := make([]Trunk, 0, len(cfg.Trunks))
	for _, tc := range cfg.Trunks {
		// FSNode mapping: TrunkConfig has SIPGateway (the mod_sofia
		// gateway name) but no per-trunk NodeAddrs list. Plan 09 Task 5
		// lives in a world where every FS node has every gateway
		// configured (the dev/prod Helm chart deploys identical
		// freeswitch.xml.conf to each FS pod). We materialise NodeAddrs
		// lazily inside Select by trusting Pool.HealthyNodes() — a more
		// granular per-trunk node-affinity is Plan 13/14 work. For now,
		// store a sentinel empty slice; Select treats nil/empty as "any
		// healthy node".
		trunks = append(trunks, Trunk{
			ID:          tc.ID,
			GatewayName: tc.SIPGateway,
			NodeAddrs:   nil, // any healthy FS node — see comment above
			CostPerMin:  tc.CostPerMinuteRub,
			Weight:      tc.Weight,
			Active:      tc.SIPGateway != "" && tc.CapacityChannels > 0,
			FailureRate: 0, // Plan 13 stats collector populates this
			Priority:    0, // reserved
		})
	}

	r := &Router{
		pool:         cfg.Pool,
		bp:           NewBackpressure(cfg.Redis, cfg.BackpressureCap),
		logger:       logger,
		metrics:      cfg.Metrics,
		defaultStrat: cfg.DefaultStrategy,
		trunks:       trunks,
		strategies: map[string]Strategy{
			string(api.RouteLeastCost):             LeastCost{},
			string(api.RouteLeastCostWithFallback): LeastCostWithFallback{FailureThreshold: 0.5},
			string(api.RouteRoundRobin):            &RoundRobin{},
			string(api.RouteWeighted):              Weighted{},
		},
	}
	return r, nil
}

// Start is a no-op in v1 — see package comment for the deviation rationale.
// Reserved for future hot-reload of the trunk catalog from Postgres or a
// SIGHUP-triggered Snapshot reload.
func (r *Router) Start(_ context.Context) error {
	return nil
}

// Stop is a no-op in v1. Symmetrical with Start so the composition root's
// shutdown sequence does not need to special-case the router.
func (r *Router) Stop() {}

// Select implements api.Router. The flow:
//
//  1. Pick a Strategy by req.Strategy → defaultStrat → LeastCost.
//  2. Snapshot the trunk catalog under RLock; release before calling
//     Strategy.Pick (the strategy is pure; no need to hold the lock).
//  3. Strategy returns a Trunk or ErrNoTrunkAvailable.
//  4. Compute the candidate node set: trunk.NodeAddrs ∩ healthy if
//     NodeAddrs is non-empty; else just healthy. Sorted lex for
//     determinism.
//  5. Walk candidates calling TryAcquire on each — the first success wins.
//     Backpressure errors are logged and the loop continues (treat redis
//     transients as "this node is unavailable right now").
//  6. Build the api.SelectionResult; the dialer constructs the originate
//     URL from FSNode + TrunkID.
//
// Latency budget: every Select hits Redis at least once (TryAcquire). The
// fan-out across nodes is bounded by the healthy-node count (≤ small
// integer in production) so worst-case latency = N × redis-RTT.
func (r *Router) Select(ctx context.Context, req api.SelectRequest) (api.SelectionResult, error) {
	startedAt := time.Now()
	stratName := r.resolveStrategyName(req.Strategy)
	strategy := r.strategyByName(stratName)

	defer func() {
		if r.metrics != nil {
			r.metrics.SelectDuration.WithLabelValues(stratName).Observe(time.Since(startedAt).Seconds())
		}
	}()

	// Snapshot the catalog. RLock is released before Strategy.Pick so a
	// future hot-reload writer doesn't have to wait on strategy work.
	r.trunksMu.RLock()
	trunks := slices.Clone(r.trunks)
	r.trunksMu.RUnlock()

	if len(trunks) == 0 {
		r.observeResult(stratName, "no_trunk")
		return api.SelectionResult{}, ErrNoTrunkAvailable
	}

	trunk, err := strategy.Pick(trunks, "")
	if err != nil {
		r.observeResult(stratName, "no_trunk")
		return api.SelectionResult{}, err
	}

	candidates := r.candidateNodes(trunk)
	if len(candidates) == 0 {
		r.observeResult(stratName, "no_node")
		return api.SelectionResult{}, fmt.Errorf("router: no healthy node for trunk %s: %w", trunk.ID, ErrNoTrunkAvailable)
	}

	chosen, err := r.acquireFirstHealthy(ctx, candidates)
	if err != nil {
		r.observeResult(stratName, "err")
		return api.SelectionResult{}, err
	}
	if chosen == "" {
		r.observeResult(stratName, "no_node")
		return api.SelectionResult{}, fmt.Errorf("router: no node accepted backpressure for trunk %s: %w", trunk.ID, ErrNoTrunkAvailable)
	}

	r.observeResult(stratName, "ok")
	return api.SelectionResult{
		FSNode:  chosen,
		TrunkID: trunk.ID,
		// Reason carries the strategy name so consumers (logs, audit)
		// can trace why a particular trunk was picked. Plan 13 may
		// extend this to "fallback:<other-id>" when the strategy used
		// a fallback path.
		Reason: stratName,
	}, nil
}

// ReleaseChannel is called from nats_bridge when CHANNEL_HANGUP_COMPLETE
// arrives so the per-node counter drops back below cap. Idempotent on the
// Redis side (releaseScript clamps at 0) — over-release from NATS
// redelivery is a no-op.
func (r *Router) ReleaseChannel(ctx context.Context, nodeAddr string) error {
	if err := r.bp.Release(ctx, nodeAddr); err != nil {
		return fmt.Errorf("router: release channel %s: %w", nodeAddr, err)
	}
	return nil
}

// Backpressure exposes the Backpressure handle so Plan 09 Task 6's
// reconciler can call Get / SetActiveChannels without re-importing the
// Redis client. Read-only seam: callers MUST NOT swap the underlying
// counters — the router owns the cap.
func (r *Router) Backpressure() *Backpressure { return r.bp }

// resolveStrategyName picks the request's strategy name, falling back to
// the configured default and finally to least_cost. Returns the canonical
// string used to label metrics — not necessarily the same as req.Strategy
// (an unknown name is silently mapped to least_cost so a typo in the
// dialer doesn't crash a call).
func (r *Router) resolveStrategyName(reqStrat api.RoutingStrategy) string {
	name := strings.TrimSpace(string(reqStrat))
	if name == "" {
		name = strings.TrimSpace(r.defaultStrat)
	}
	if name == "" {
		name = string(api.RouteLeastCost)
	}
	if _, ok := r.strategies[name]; !ok {
		// Unknown strategy → least_cost. The label still records the
		// resolved name so dashboards aren't surprised.
		name = string(api.RouteLeastCost)
	}
	return name
}

// strategyByName is total: an unknown name (already filtered by
// resolveStrategyName) returns LeastCost. The lookup is read-only — the
// strategies map is built once in New and never mutated.
func (r *Router) strategyByName(name string) Strategy {
	if s, ok := r.strategies[name]; ok {
		return s
	}
	return LeastCost{}
}

// candidateNodes returns the FS-node candidates for the chosen trunk in
// deterministic order. trunk.NodeAddrs being nil/empty is the v1 default
// ("any healthy node") — every FS node in production has every gateway
// configured. A non-empty NodeAddrs list intersects with healthy.
func (r *Router) candidateNodes(trunk Trunk) []string {
	healthy := r.pool.HealthyNodes()
	if len(healthy) == 0 {
		return nil
	}
	if len(trunk.NodeAddrs) == 0 {
		// Defensive copy — the pool's slice is a fresh allocation per
		// HealthyNodes call, but be explicit about ownership so a
		// future caller that holds the slice does not see drift.
		out := slices.Clone(healthy)
		sort.Strings(out)
		return out
	}
	want := make(map[string]struct{}, len(trunk.NodeAddrs))
	for _, n := range trunk.NodeAddrs {
		want[n] = struct{}{}
	}
	out := make([]string, 0, len(healthy))
	for _, h := range healthy {
		if _, ok := want[h]; ok {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// acquireFirstHealthy walks candidates in order, attempting TryAcquire on
// each. The first success returns the node; per-call backpressure errors
// are logged + skipped (treat as "this node is unavailable now"). Returns
// (chosenNode, nil) on success; ("", nil) when every candidate was at cap;
// ("", err) only on a hard ctx cancellation observed mid-loop.
func (r *Router) acquireFirstHealthy(ctx context.Context, candidates []string) (string, error) {
	for _, node := range candidates {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("router: ctx cancelled mid-acquire: %w", err)
		}
		ok, err := r.bp.TryAcquire(ctx, node)
		if err != nil {
			r.logger.Warn("router: backpressure acquire failed; trying next node",
				zap.String("node", node),
				zap.Error(err),
			)
			continue
		}
		if !ok {
			if r.metrics != nil {
				r.metrics.BackpressureRejects.WithLabelValues(node).Inc()
			}
			continue
		}
		return node, nil
	}
	return "", nil
}

// observeResult bumps the SelectsTotal counter; nil-safe for tests that
// skip metrics wiring.
func (r *Router) observeResult(strategy, result string) {
	if r.metrics == nil {
		return
	}
	r.metrics.SelectsTotal.WithLabelValues(strategy, result).Inc()
}

// Compile-time check: Router implements api.Router. If the api signature
// drifts, compilation here fails — the alternative is a runtime "method
// not found" panic at first dispatch.
var _ api.Router = (*Router)(nil)
