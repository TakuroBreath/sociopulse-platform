package service

import (
	"time"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsevents "github.com/sociopulse/platform/internal/reports/events"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Service aggregates the reports module's runtime components.
//
// Plan 13.3 Task 5 wires Runner + Audit + ReadyPub. Task 6 added Queue
// (async-enqueue) and Consumer (asynq worker). The HTTP transport
// (Task 7) reads Runner + Queue; cmd/api Task 8 runs Consumer in the
// worker process.
//
// Queue / Consumer are nil-by-default in Build to keep Task 5 callers
// (sync-only tests) unchanged; cmd/api populates them after the asynq
// client + pool are constructed.
type Service struct {
	// Runner is the synchronous-path executor; HTTP handlers call its
	// Run method for the small/fast report path.
	Runner *Runner
	// Audit is the emitter the Consumer reuses inside its finalisation
	// transaction (same instance is fine — AuditEmitter holds no
	// per-request state).
	Audit *AuditEmitter
	// ReadyPub is the report-ready outbox publisher the Consumer
	// invokes when an artifact is successfully uploaded.
	ReadyPub *reportsevents.ReportReadyPublisher
	// Queue is the asynq-backed async-enqueue surface; the HTTP
	// transport calls Enqueue / Get on it. Populated by cmd/api once
	// the asynq client and reports store are constructed.
	Queue *Queue
	// Consumer is the asynq worker; cmd/worker calls Consumer.Run.
	// Populated by cmd/worker boot.
	Consumer *Consumer
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

// Build constructs a Service from raw deps. The Queue and Consumer
// fields stay nil here — cmd/api / cmd/worker attach them once their
// own collaborators (asynq client, pool, object store) are wired.
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
