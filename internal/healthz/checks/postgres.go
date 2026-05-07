// Package checks implements concrete Checker types for healthz/readiness probes.
package checks

import (
	"context"
	"time"
)

// Pinger is the minimal Postgres surface readiness needs. Plan 03 will provide
// *pgxpool.Pool which satisfies this trivially via a Ping wrapper.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PostgresCheck returns a healthz.Checker that pings Postgres.
type PostgresCheck struct {
	Pool Pinger
}

// Name reports the dependency identifier surfaced in /readyz output.
func (PostgresCheck) Name() string { return "postgres" }

// Check pings the pool, honouring the deadline already set on ctx.
func (p PostgresCheck) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return p.Pool.Ping(cctx)
}
