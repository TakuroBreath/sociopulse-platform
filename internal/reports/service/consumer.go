package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	auditapi "github.com/sociopulse/platform/internal/audit/api"
	storage "github.com/sociopulse/platform/internal/recording/storage"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	rptevents "github.com/sociopulse/platform/internal/reports/events"
	reportstore "github.com/sociopulse/platform/internal/reports/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// AsynqServer is the asynq.Server surface Consumer.Run needs. The
// production type is *asynq.Server (matches via Run + Shutdown); tests
// inject a tiny fake that signals Run via a channel so the lifecycle
// can be observed without touching Redis.
type AsynqServer interface {
	Run(handler asynq.Handler) error
	Shutdown()
}

// runningFlipFn / succeededFlipFn / failedFlipFn are the three state-
// flip closures Consumer.handleJobRun calls inside pool.WithTenant.
// Production wires reportstore.MarkRunningTx / MarkSucceededTx /
// MarkFailedTx; tests substitute fakes via SetConsumerFlipsForTest so
// the ErrStaleSkip / err-handling branches are exercisable without a
// real Postgres tx.
type runningFlipFn = func(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time) error
type succeededFlipFn = func(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time, bytesSize int64, filename, url string) error
type failedFlipFn = func(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time, errMsg string) error

// ConsumerDeps groups every port Consumer.Run needs. Production fills
// them in cmd/worker; tests construct the fakes inline.
type ConsumerDeps struct {
	// Server is the asynq.Server consumer.Run blocks on. Required.
	Server AsynqServer
	// Analytics is the read-only data surface renderByKind dispatches to.
	// Required.
	Analytics analyticsapi.ServiceRO
	// Pool opens the per-tenant transaction the state-flip + audit emit
	// + report-ready publish run inside. Required.
	Pool QueuePool
	// ObjectStore uploads the rendered artifact and signs the
	// presigned download URL. Required.
	ObjectStore storage.ObjectStore
	// Audit is the AuditEmitter that appends a tenant.<t>.audit.event
	// row inside the success/failure tx. Required.
	Audit *AuditEmitter
	// ReadyPub publishes tenant.<t>.reports.report.ready inside the
	// success tx. Required.
	ReadyPub *rptevents.ReportReadyPublisher

	// Bucket is the fully-resolved S3 bucket name the rendered artifact
	// lands in. Project convention is a single shared bucket per
	// environment (cfg.S3.Buckets.Reports — e.g. "sociopulse-dev-reports")
	// with tenant isolation enforced via a tenant-prefixed object key,
	// matching the recording module's S3Config.Recordings layout. Per-
	// tenant buckets were considered (Plan 13.3 Task 6 draft) but the
	// project does not provision them: 30 tenants × 2 buckets each =
	// 60 buckets to track in IAM, vs one bucket + RLS-equivalent key
	// prefix.
	Bucket string
	// PresignTTL is the lifetime of the GET URL we mint on succeed —
	// production passes 24h per §FR-I3.
	PresignTTL time.Duration

	// Logger is required for the failure-path zap entries.
	Logger *zap.Logger
	// Now is injectable for tests; defaults to time.Now when nil.
	Now func() time.Time
}

// Consumer is the asynq worker side of the async reports path. Owns
// the asynq.Server lifecycle (Run + Shutdown) and registers the
// TaskJobRun handler on a ServeMux. cmd/worker constructs one, calls
// Run, and the call blocks until ctx is cancelled.
type Consumer struct {
	d             ConsumerDeps
	runningFlip   runningFlipFn
	succeededFlip succeededFlipFn
	failedFlip    failedFlipFn
}

// NewConsumer constructs a Consumer with the supplied deps. Now
// defaults to time.Now. The state-flip closures default to the
// reportstore.Mark*Tx free functions and are only swapped out in unit
// tests via the export helper.
func NewConsumer(d ConsumerDeps) *Consumer {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Consumer{
		d:             d,
		runningFlip:   reportstore.MarkRunningTx,
		succeededFlip: reportstore.MarkSucceededTx,
		failedFlip:    reportstore.MarkFailedTx,
	}
}

// Compile-time guard.
var _ reportsapi.JobConsumer = (*Consumer)(nil)

// Run builds the asynq.ServeMux internally, registers handleJobRun on
// TaskJobRun, then calls Server.Run in a goroutine and waits for either
// ctx cancellation (→ graceful Shutdown) or Server.Run exit (→
// surface the error).
//
// Server.Run is graceful-only — it refuses new tasks and waits for
// in-flight handlers to drain on Shutdown. The standard cmd/worker
// pattern (analytics ingest + recording retention) is the template.
func (c *Consumer) Run(ctx context.Context) error {
	mux := asynq.NewServeMux()
	mux.HandleFunc(reportsapi.TaskJobRun, c.handleJobRun)

	errCh := make(chan error, 1)
	go func() { errCh <- c.d.Server.Run(mux) }()

	select {
	case <-ctx.Done():
		c.d.Server.Shutdown()
		<-errCh // wait for the goroutine to exit so we don't leak it
		return nil
	case err := <-errCh:
		if err == nil {
			return nil
		}
		return fmt.Errorf("reports.Consumer.Run: asynq server: %w", err)
	}
}

// handleJobRun is the asynq.HandlerFunc-shaped processor for TaskJobRun.
// Returns nil to ack; non-nil error triggers retry (or skip-retry when
// the error is wrapped via asynq.SkipRetry for permanent failures).
//
// Pipeline:
//
//	Phase 1: MarkRunningTx                                — pool.WithTenant
//	Phase 2: renderByKind (Task 5 dispatcher)             — pure compute
//	Phase 3: ObjectStore.Put + PresignedURL               — S3 round-trip
//	Phase 4: MarkSucceededTx + Audit.EmitTx + ReadyPub    — pool.WithTenant
//
// Permanent failures (ErrInvalidParams / ErrUnknownKind / ErrUnsupportedFmt
// / ErrTooLarge) wrap via asynq.SkipRetry. Transient failures (network,
// renderer panic, S3 5xx) bubble bare so asynq retries with backoff.
// ErrStaleSkip on the MarkRunning flip is the "already running / already
// terminal" case (duplicate task delivery) — handler returns nil to ack
// without further work.
func (c *Consumer) handleJobRun(ctx context.Context, task *asynq.Task) error {
	var in reportsapi.JobInput
	if err := json.Unmarshal(task.Payload(), &in); err != nil {
		c.d.Logger.Error("reports.consumer: bad payload", zap.Error(err))
		// Permanent — a malformed payload cannot be fixed by retry.
		return fmt.Errorf("reports.consumer: bad payload: %w: %w", err, asynq.SkipRetry)
	}
	jobID, _ := asynq.GetTaskID(ctx)
	tenantID := in.TenantID
	now := c.d.Now().UTC()

	// Phase 1: MarkRunning. ErrStaleSkip → ack without further work.
	// Capture the skip case via a flag rather than returning nil from
	// the closure, because the outer pipeline must distinguish
	// "transitioned successfully" from "row already terminal" (the
	// latter must NOT proceed to render+upload+succeed).
	var staleSkip bool
	if err := c.d.Pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		flipErr := c.runningFlip(ctx, tx, jobID, now)
		if errors.Is(flipErr, reportstore.ErrStaleSkip) {
			staleSkip = true
			return nil // empty tx commit is harmless
		}
		return flipErr
	}); err != nil {
		c.d.Logger.Error("reports.consumer: MarkRunning failed",
			zap.String("job", jobID), zap.Error(err))
		return err // transient — asynq retries
	}
	if staleSkip {
		return nil // ack — another worker already owns the row (or it's terminal)
	}

	// Phase 2: render.
	res, renderErr := renderByKind(ctx, c.d.Analytics, in.RenderInput)
	if renderErr != nil {
		return c.markFailed(ctx, tenantID, in, jobID, renderErr)
	}

	// Phase 3: upload + presign. Single shared bucket per environment
	// (cfg.S3.Buckets.Reports); tenant isolation rides on the key's
	// leading <tenant_uuid>/ component (mirrors recording's bucket +
	// key strategy — see internal/recording/service/upload.go).
	bucket := c.d.Bucket
	key := syntheticConsumerKey(tenantID, in.Kind, in.ActorID, res.Filename, c.d.Now())
	if err := c.d.ObjectStore.Put(ctx, bucket, key, res.Bytes, res.MIME); err != nil {
		return c.markFailed(ctx, tenantID, in, jobID, fmt.Errorf("upload: %w", err))
	}
	url, err := c.d.ObjectStore.PresignedURL(ctx, bucket, key, c.d.PresignTTL)
	if err != nil {
		return c.markFailed(ctx, tenantID, in, jobID, fmt.Errorf("presign: %w", err))
	}

	// Phase 4: MarkSucceededTx + Audit.EmitTx + ReadyPub.PublishReadyTx,
	// all in one pool.WithTenant for atomicity. The audit + outbox
	// appends share the tx with the row update so either everything
	// commits or nothing does (no split-brain).
	return c.d.Pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		flipErr := c.succeededFlip(ctx, tx, jobID, c.d.Now().UTC(),
			int64(len(res.Bytes)), res.Filename, url)
		if errors.Is(flipErr, reportstore.ErrStaleSkip) {
			// Race: the job was canceled (or somehow terminal) between
			// Phase 1 and Phase 4. Ack and tolerate the orphaned S3
			// object — v1 accepts this; a future plan adds an orphan-
			// sweeper to the retention worker.
			return nil
		}
		if flipErr != nil {
			return flipErr
		}
		if auditErr := c.d.Audit.EmitTx(ctx, tx, AuditExport{
			TenantID:  tenantID,
			ActorID:   in.ActorID,
			ActorKind: auditapi.ActorUser,
			JobID:     jobID,
			Kind:      string(in.Kind),
			Format:    string(in.Format),
			BytesSize: int64(len(res.Bytes)),
			Window:    AuditWindow{From: in.Window.From, To: in.Window.To},
			Params:    in.Params,
			Timestamp: c.d.Now().UTC(),
		}); auditErr != nil {
			return fmt.Errorf("audit emit: %w", auditErr)
		}
		return c.d.ReadyPub.PublishReadyTx(ctx, tx, tenantID, jobID, reportsapi.ReportReadyEvent{
			JobID:       jobID,
			TenantID:    tenantID.String(),
			Kind:        string(in.Kind),
			Format:      string(in.Format),
			Filename:    res.Filename,
			BytesSize:   int64(len(res.Bytes)),
			DownloadURL: url,
		})
	})
}

// markFailed transitions a job to failed via the *Tx variant + emits an
// audit-event in the same tx. Permanent failures (bad params, unknown
// kind, unsupported fmt, too large) wrap via asynq.SkipRetry; transient
// errors return as-is for asynq retry.
func (c *Consumer) markFailed(
	ctx context.Context, tenantID uuid.UUID, in reportsapi.JobInput, jobID string, cause error,
) error {
	msg := cause.Error()
	c.d.Logger.Error("reports.consumer: render failed",
		zap.String("job", jobID), zap.Error(cause))

	flipErr := c.d.Pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		err := c.failedFlip(ctx, tx, jobID, c.d.Now().UTC(), msg)
		if errors.Is(err, reportstore.ErrStaleSkip) {
			return nil // already terminal — no further work
		}
		if err != nil {
			return err
		}
		return c.d.Audit.EmitTx(ctx, tx, AuditExport{
			TenantID:  tenantID,
			ActorID:   in.ActorID,
			ActorKind: auditapi.ActorUser,
			JobID:     jobID,
			Kind:      string(in.Kind),
			Format:    string(in.Format),
			Window:    AuditWindow{From: in.Window.From, To: in.Window.To},
			Params:    in.Params,
			Timestamp: c.d.Now().UTC(),
		})
	})
	if flipErr != nil {
		c.d.Logger.Error("reports.consumer: MarkFailed flip failed",
			zap.String("job", jobID), zap.Error(flipErr))
	}

	// Permanent failures don't retry — asynq.SkipRetry archives the task.
	if isPermanentRenderError(cause) {
		return fmt.Errorf("reports.consumer: permanent: %w: %w", cause, asynq.SkipRetry)
	}
	return cause // transient — asynq retries with the configured RetryDelayFunc
}

// isPermanentRenderError returns true when the underlying error is a
// non-retryable input fault. Centralised so the classification rule is
// in one place (Task 7's HTTP layer maps the same sentinels to 4xx
// responses).
func isPermanentRenderError(err error) bool {
	return errors.Is(err, reportsapi.ErrInvalidParams) ||
		errors.Is(err, reportsapi.ErrUnknownKind) ||
		errors.Is(err, reportsapi.ErrUnsupportedFmt) ||
		errors.Is(err, reportsapi.ErrTooLarge)
}

// syntheticConsumerKey builds the S3 object key for a rendered artifact.
//
// Layout: "<tenant_uuid>/<kind>/<yyyy>/<mm>/<dd>/<actor>-<filename>".
//
// The leading <tenant_uuid>/ component is the tenant-isolation primitive
// for the shared reports bucket (Plan 13.3 Task 8 — see ConsumerDeps.Bucket
// doc for the rationale). Day-grain folder structure lets ops grep for a
// tenant's exports on a given day without listing the whole bucket. Within
// the day/actor folder the renderer-generated timestamp in <filename>
// disambiguates same-second runs (Task 4 renderers produce filenames like
// "operator_efficiency_2026-05-14T12-00-00Z.xlsx").
func syntheticConsumerKey(tenantID uuid.UUID, kind reportsapi.ReportKind, actor uuid.UUID, filename string, ts time.Time) string {
	return fmt.Sprintf("%s/%s/%s/%s-%s",
		tenantID, kind, ts.UTC().Format("2006/01/02"), actor, filename)
}
