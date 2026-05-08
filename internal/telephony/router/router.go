// Package router selects the {fs_node, trunk} pair for a given operator + phone +
// strategy and enforces per-node line-capacity tracking via Redis.
//
// TODO(plan-09-task-5): Plan 09 Task 5 fills in the real selector — round
// robin, weighted, and least-cost-with-fallback strategies — backed by the
// trunk catalog in pkg/config plus a refresh loop that re-reads
// telephony_trunks rows from Postgres. The skeleton ships with stubs sized
// just to compile cmd/telephony-bridge.
//
// Why a skeleton ships in Task 1: the composition root needs a typed
// *Router to plumb into nats_bridge and to call .Start/.Stop in the bootstrap
// sequence. Stubbing each dependent package every plan task floods the diff;
// shipping the boundary first lets Task 5 land its body without churn.
package router

import (
	"context"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/telephony/pool"
)

// Config holds the router's wiring — every field stays compatible with the
// signatures Task 5 will introduce. Optional dependencies (Redis, Postgres)
// are passed via interfaces or `any` so the skeleton does not pull in the
// real client packages prematurely.
type Config struct {
	// Pool is the ESL connection fleet used to reach FreeSWITCH nodes. The
	// router never dials directly — every command flows through Pool.Get().
	Pool *pool.ESLPool

	// Redis is the project-wide Redis client used for backpressure tokens
	// and the per-node active-channel counter. Typed as any so this package
	// does not pull in github.com/redis/go-redis/v9 ahead of Task 5.
	Redis any

	// BackpressureCap is the per-node concurrent-call ceiling. The router
	// rejects originates that would push a node above this number.
	BackpressureCap int

	// PostgresDSN is the source of truth for telephony_trunks rows. Empty in
	// dev/test; Task 5 opens a real *postgres.Pool from this DSN.
	PostgresDSN string

	// Logger is named for the router subsystem; nil-tolerated.
	Logger *zap.Logger
}

// Router is the composition-root handle. The real implementation owns the
// trunk-refresh loop and the strategy state machines; the skeleton is empty.
type Router struct {
	logger *zap.Logger
}

// New constructs a Router skeleton. It cannot fail today — every validation
// will move here in Task 5.
func New(cfg Config) *Router {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Router{logger: logger}
}

// Start spawns the background refresh loop. Skeleton no-ops; the signature
// matches what Task 5 will introduce so the composition root stays unchanged.
func (r *Router) Start(_ context.Context) error {
	return nil
}

// Stop tears down the refresh loop. Skeleton no-ops.
func (r *Router) Stop() {
	// no goroutines to stop yet
}
