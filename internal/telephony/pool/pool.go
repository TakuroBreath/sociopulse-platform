// Package pool owns the ESL connection fleet to the configured FreeSWITCH
// nodes. It is the only consumer of internal/telephony/esl.Client (Plan 09
// Task 2) and the only producer of healthy-node selection signals consumed by
// internal/telephony/router (Plan 09 Task 5) and the readyz handler in
// cmd/telephony-bridge.
//
// TODO(plan-09-task-4): Plan 09 Task 4 fills in the real pool. The current
// surface is a deliberate skeleton — just enough for cmd/telephony-bridge to
// compile, boot, and answer /healthz / /readyz. Methods that need real
// behaviour (Get, Originate, channel reconciliation) return a clear
// "not yet implemented" error.
//
// Why a skeleton ships in Task 1: the composition root of cmd/telephony-bridge
// already needs a typed *ESLPool to wire into its readyz check, the router,
// and the nats-bridge. Stubbing every dependent package every plan task would
// flood the diff; instead we ship the package boundary first (Task 1) and let
// Task 4 fill in the body.
package pool

import (
	"context"
	"errors"
	"slices"
	"sync"

	"go.uber.org/zap"
)

// errNotImplemented is returned by every behavioural method on the skeleton.
// Plan 09 Task 4 swaps these out for real implementations; until then any
// caller that lands on Get explicitly knows it is hitting an unfinished path.
var errNotImplemented = errors.New("telephony/pool: not yet implemented (Plan 09 Task 4)")

// Config configures the pool. Keep this struct small — Task 4 will extend it
// with timeouts, mTLS material, and per-node password lookup.
type Config struct {
	// Nodes is the list of FreeSWITCH ESL endpoints (host:port). Must be
	// non-empty; New rejects an empty slice so a misconfigured Helm chart
	// fails loudly at boot rather than at first dial.
	Nodes []string

	// Password is the shared ESL password. Per-node mTLS replaces this in
	// production deployments (Plan 09 Task 4); the field is kept here so the
	// composition root has somewhere to plumb the dev/test secret.
	Password string

	// Logger is a structured zap logger named for the pool subsystem. The
	// caller is expected to .Named("telephony.pool") before passing it in.
	// May be nil — the skeleton tolerates a nil logger.
	Logger *zap.Logger
}

// ESLPool is the typed handle the composition root holds. The real
// implementation will own goroutine state (per-node runners, healthcheck
// loop, reconciliation worker); the skeleton holds only the nodes the
// constructor was given so HealthyNodes can return a deterministic answer.
type ESLPool struct {
	mu     sync.RWMutex
	nodes  []string
	logger *zap.Logger
}

// New constructs an ESLPool from cfg. It validates that at least one node is
// configured — every other validation is the responsibility of Task 4.
//
// The ctx parameter is reserved for the real implementation (which will dial
// each node before returning); the skeleton ignores it.
func New(_ context.Context, cfg Config) (*ESLPool, error) {
	if len(cfg.Nodes) == 0 {
		return nil, errors.New("telephony/pool: at least one FreeSWITCH node must be configured")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	// Defensive copy so the caller's slice cannot be mutated through us.
	nodes := slices.Clone(cfg.Nodes)
	return &ESLPool{nodes: nodes, logger: logger}, nil
}

// Get returns the *esl.Client for the named node.
//
// TODO(plan-09-task-4): Plan 09 Task 4 returns a real client; until then the
// skeleton returns errNotImplemented so unfinished call sites surface during
// development.
func (p *ESLPool) Get(_ string) (any, error) {
	return nil, errNotImplemented
}

// AnyHealthy reports whether at least one configured node passes its periodic
// healthcheck.
//
// TODO(plan-09-task-4): Plan 09 Task 4 will track real healthcheck state. The
// skeleton always returns true so /readyz can succeed during boot/integration
// development without a healthcheck loop running. This is the intentional
// choice: pretending healthy lets the rest of the stack come up; pretending
// unhealthy would deadlock cmd/telephony-bridge readyz behind a stub.
func (p *ESLPool) AnyHealthy() bool {
	return true
}

// HealthyNodes returns the addresses of nodes considered healthy. The router
// uses this to constrain its candidate set when selecting an FS node for an
// originate.
//
// TODO(plan-09-task-4): the real implementation will exclude nodes the
// healthcheck loop has marked down. The skeleton returns the full configured
// list so router/nats_bridge skeletons can iterate something concrete.
func (p *ESLPool) HealthyNodes() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.nodes)
}

// Close tears down the pool — stops the healthcheck loop, drains in-flight
// commands, closes every per-node connection. Safe to call multiple times.
//
// TODO(plan-09-task-4): the skeleton has no goroutines to stop and no
// connections to close, so it is a no-op. The signature stays in place so the
// composition root's defer pool.Close() compiles unchanged after Task 4.
func (p *ESLPool) Close() error {
	return nil
}
