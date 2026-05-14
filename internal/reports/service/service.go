package service

import (
	"time"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsevents "github.com/sociopulse/platform/internal/reports/events"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Service aggregates the reports module's runtime components.
//
// Plan 13.3 Task 5 wires Runner + Audit + ReadyPub. Task 6 will set
// the Queue + Consumer fields when the async path lands.
type Service struct {
	// Runner is the synchronous-path executor; HTTP handlers call its
	// Run method for the small/fast report path.
	Runner *Runner
	// Audit is the emitter Task 6's async Consumer reuses inside its
	// finalisation transaction (same instance is fine — AuditEmitter
	// holds no per-request state).
	Audit *AuditEmitter
	// ReadyPub is the report-ready outbox publisher Task 6's async
	// Consumer invokes when an artifact is successfully uploaded.
	ReadyPub *reportsevents.ReportReadyPublisher
	// Queue / Consumer added in Task 6.
}

// Config groups the configurable knobs.
type Config struct {
	// Threshold drives the sync-vs-async decision; loaded from
	// pkg/config.ReportsConfig at construction time.
	Threshold ThresholdConfig
	// Now is injectable for tests; nil falls back to time.Now inside
	// NewRunner.
	Now func() time.Time
}

// Build constructs a Service from raw deps. Task 6 will add the Queue
// + Consumer fields; this constructor is the seam.
//
// The pool argument satisfies the runner's TenantRunner port via its
// WithTenant method.
func Build(
	ana analyticsapi.ServiceRO,
	pool *postgres.Pool,
	audit *AuditEmitter,
	readyPub *reportsevents.ReportReadyPublisher,
	cfg Config,
) *Service {
	return &Service{
		Runner: NewRunner(RunnerDeps{
			Analytics: ana,
			TxRunner:  pool,
			Audit:     audit,
			Threshold: cfg.Threshold,
			Now:       cfg.Now,
		}),
		Audit:    audit,
		ReadyPub: readyPub,
	}
}
