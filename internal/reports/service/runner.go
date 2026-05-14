package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	auditapi "github.com/sociopulse/platform/internal/audit/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/templates/calls_by_status"
	"github.com/sociopulse/platform/internal/reports/templates/finance"
	"github.com/sociopulse/platform/internal/reports/templates/hourly_activity"
	"github.com/sociopulse/platform/internal/reports/templates/operator_efficiency"
	"github.com/sociopulse/platform/internal/reports/templates/project_summary"
	"github.com/sociopulse/platform/internal/reports/templates/quality_control"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TenantRunner is the narrow transaction-runner port Runner.Run needs.
// *postgres.Pool satisfies this interface via its WithTenant method;
// tests substitute an in-memory implementation that invokes fn with a
// zero postgres.Tx so we avoid spinning up a real database for the
// happy path.
//
// Defined consumer-side per project convention (07-go-coding-standards
// § Interfaces): the producer (*postgres.Pool) returns a concrete
// struct; the consumer narrows it to the methods it actually needs.
type TenantRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
}

// Compile-time guard: *postgres.Pool must continue to satisfy the
// TenantRunner contract. Catches a refactor of pool.WithTenant signature
// at the right module instead of at cmd/api Build() construction site.
var _ TenantRunner = (*postgres.Pool)(nil)

// RunnerDeps is the set of ports the sync Runner needs.
//
// Note: NO ObjectStore here — sync path returns bytes inline; no S3
// upload happens. The async Consumer (Task 6) is where S3 + presigned
// URLs enter the flow.
type RunnerDeps struct {
	// Analytics is the read-only analytics surface the data fetchers
	// consult. Required.
	Analytics analyticsapi.ServiceRO
	// TxRunner opens the per-tenant transaction the audit emit runs
	// inside. Required. Production passes *postgres.Pool; tests pass a
	// fake that simply invokes fn with a zero postgres.Tx.
	TxRunner TenantRunner
	// Audit is the AuditEmitter that appends a tenant.<t>.audit.event
	// row inside the tx. Required.
	Audit *AuditEmitter
	// Threshold controls when Run refuses the synchronous path with
	// reportsapi.ErrAsyncRequired. Zero-value config falls back to the
	// 30-day / 100k-row defaults from threshold.go.
	Threshold ThresholdConfig
	// Now is injectable for tests; defaults to time.Now when nil.
	Now func() time.Time
}

// Runner is the synchronous-path executor implementing
// reportsapi.ReportRunner.
type Runner struct{ deps RunnerDeps }

// NewRunner constructs a Runner. The constructor fills in a default
// time.Now so deps.Now is always callable inside Run.
func NewRunner(d RunnerDeps) *Runner {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Runner{deps: d}
}

// Compile-time guard: Runner implements the reports API contract.
var _ reportsapi.ReportRunner = (*Runner)(nil)

// Run executes a synchronous report.
//
// Returns:
//   - reportsapi.ErrAsyncRequired when the request trips the async
//     threshold. The HTTP handler converts to a 202+JobID by
//     enqueueing.
//   - reportsapi.ErrInvalidParams (wrapped) for bad input (tenant_id,
//     unknown kind, ...).
//   - analyticsapi.ErrInvalidWindow bare (Plan 13.2 lesson #3).
//   - reportsapi.ErrUnknownKind / ErrUnsupportedFmt for bad enum
//     values.
//   - reportsapi.ErrTooLarge bubbled from a renderer that exceeds its
//     caps.
//   - other errors wrapped via %w.
//
// On success: emits a tenant.<t>.audit.event row via outbox inside a
// TxRunner.WithTenant tx, then returns the rendered bytes inline
// (no S3 upload, no reports_jobs row).
func (r *Runner) Run(ctx context.Context, in reportsapi.RunInput) (reportsapi.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return reportsapi.RunResult{}, err
	}
	if err := validateRunInput(in); err != nil {
		return reportsapi.RunResult{}, err
	}
	if IsAsyncRequired(r.deps.Threshold, in.Window, estimateRows(in.Kind), in.Kind) {
		return reportsapi.RunResult{}, fmt.Errorf("runner.Run: %w", reportsapi.ErrAsyncRequired)
	}
	res, err := renderByKind(ctx, r.deps.Analytics, in)
	if err != nil {
		return reportsapi.RunResult{}, fmt.Errorf("runner.Run: %w", err)
	}
	if err := r.deps.TxRunner.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		return r.deps.Audit.EmitTx(ctx, tx, AuditExport{
			TenantID:  in.TenantID,
			ActorID:   in.ActorID,
			ActorKind: auditapi.ActorUser,
			JobID:     syntheticSyncJobID(in.Kind, in.ActorID, r.deps.Now()),
			Kind:      string(in.Kind),
			Format:    string(in.Format),
			BytesSize: int64(len(res.Bytes)),
			Window:    AuditWindow{From: in.Window.From, To: in.Window.To},
			Params:    in.Params,
			Timestamp: r.deps.Now().UTC(),
		})
	}); err != nil {
		return reportsapi.RunResult{}, fmt.Errorf("runner.Run: audit: %w", err)
	}
	return res, nil
}

// validateRunInput rejects malformed RunInput before any analytics call
// or audit write. Window validation returns bare
// analyticsapi.ErrInvalidWindow (Plan 13.2 lesson #3) so HTTP layer can
// map to 400 "window_invalid".
func validateRunInput(in reportsapi.RunInput) error {
	if in.TenantID == uuid.Nil {
		return fmt.Errorf("runner: %w: tenant_id missing", reportsapi.ErrInvalidParams)
	}
	if err := in.Window.Validate(); err != nil {
		return err // bare ErrInvalidWindow per Plan 13.2 lesson #3
	}
	if !knownKind(in.Kind) {
		return fmt.Errorf("runner: %w: %s", reportsapi.ErrUnknownKind, in.Kind)
	}
	if !knownFormat(in.Format) {
		return fmt.Errorf("runner: %w: %s", reportsapi.ErrUnsupportedFmt, in.Format)
	}
	return nil
}

// knownKind returns true when k is one of the seven preset/custom kinds.
// Centralises the enum-check so adding a kind only touches this file +
// the dispatcher.
func knownKind(k reportsapi.ReportKind) bool {
	switch k {
	case reportsapi.KindOperatorEfficiency, reportsapi.KindProjectSummary,
		reportsapi.KindCallsByStatus, reportsapi.KindFinance,
		reportsapi.KindQualityControl, reportsapi.KindHourlyActivity,
		reportsapi.KindCustom:
		return true
	}
	return false
}

// knownFormat returns true when fm is one of the three supported
// formats (XLSX, CSV, PDF).
func knownFormat(fm reportsapi.ExportFormat) bool {
	switch fm {
	case reportsapi.FormatXLSX, reportsapi.FormatCSV, reportsapi.FormatPDF:
		return true
	}
	return false
}

// estimateRows is a v1 stub returning 0 for all kinds. Future plans may
// add a per-kind probe query to inform the threshold check; for now the
// threshold's window-span and KindCustom guard carry the decision.
func estimateRows(reportsapi.ReportKind) int { return 0 }

// syntheticSyncJobID is a "sync:<kind>:<actor>:<unix>" composite used
// only as the audit Target. The sync path has no persistent reports_jobs
// row, so the audit-event's Target is purely informational.
func syntheticSyncJobID(kind reportsapi.ReportKind, actor uuid.UUID, ts time.Time) string {
	return fmt.Sprintf("sync:%s:%s:%d", kind, actor, ts.UnixNano())
}

// renderByKind dispatches to the right Fetch + Render combo. All 21
// (kind, format) tuples are valid in v1.
//
// KindCustom is mapped to the project_summary renderers via a
// CustomData → ProjectSummaryData conversion (v1 hack; future plans may
// add a real templates/custom). Note: the runner refuses Custom at the
// threshold gate, so this branch only executes via the async Consumer
// in Task 6 — but the dispatcher must handle it because the same
// dispatcher serves both paths.
func renderByKind(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (reportsapi.RenderResult, error) {
	switch in.Kind {
	case reportsapi.KindOperatorEfficiency:
		data, err := FetchOperatorEfficiency(ctx, ana, in)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
		return dispatchByFormat(in.Format,
			func() (reportsapi.RenderResult, error) { return operator_efficiency.RenderXLSX(data) },
			func() (reportsapi.RenderResult, error) { return operator_efficiency.RenderCSV(data) },
			func() (reportsapi.RenderResult, error) { return operator_efficiency.RenderPDF(data) },
		)

	case reportsapi.KindProjectSummary:
		data, err := FetchProjectSummary(ctx, ana, in)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
		return dispatchByFormat(in.Format,
			func() (reportsapi.RenderResult, error) { return project_summary.RenderXLSX(data) },
			func() (reportsapi.RenderResult, error) { return project_summary.RenderCSV(data) },
			func() (reportsapi.RenderResult, error) { return project_summary.RenderPDF(data) },
		)

	case reportsapi.KindCallsByStatus:
		data, err := FetchCallsByStatus(ctx, ana, in)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
		return dispatchByFormat(in.Format,
			func() (reportsapi.RenderResult, error) { return calls_by_status.RenderXLSX(data) },
			func() (reportsapi.RenderResult, error) { return calls_by_status.RenderCSV(data) },
			func() (reportsapi.RenderResult, error) { return calls_by_status.RenderPDF(data) },
		)

	case reportsapi.KindFinance:
		data, err := FetchFinance(ctx, ana, in)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
		return dispatchByFormat(in.Format,
			func() (reportsapi.RenderResult, error) { return finance.RenderXLSX(data) },
			func() (reportsapi.RenderResult, error) { return finance.RenderCSV(data) },
			func() (reportsapi.RenderResult, error) { return finance.RenderPDF(data) },
		)

	case reportsapi.KindQualityControl:
		data, err := FetchQualityControl(ctx, ana, in)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
		return dispatchByFormat(in.Format,
			func() (reportsapi.RenderResult, error) { return quality_control.RenderXLSX(data) },
			func() (reportsapi.RenderResult, error) { return quality_control.RenderCSV(data) },
			func() (reportsapi.RenderResult, error) { return quality_control.RenderPDF(data) },
		)

	case reportsapi.KindHourlyActivity:
		data, err := FetchHourlyActivity(ctx, ana, in)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
		return dispatchByFormat(in.Format,
			func() (reportsapi.RenderResult, error) { return hourly_activity.RenderXLSX(data) },
			func() (reportsapi.RenderResult, error) { return hourly_activity.RenderCSV(data) },
			func() (reportsapi.RenderResult, error) { return hourly_activity.RenderPDF(data) },
		)

	case reportsapi.KindCustom:
		// v1: KindCustom has no dedicated template; project the CustomData
		// (Overview-shaped) onto a ProjectSummaryData and pass through the
		// project_summary renderers. Future plans may add a real custom
		// template if business asks.
		data, err := FetchCustom(ctx, ana, in)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
		ps := ProjectSummaryData{
			Window:  data.Window,
			Project: uuid.Nil,
			Calls:   data.OV.Calls,
			State:   data.OV.OperatorState,
			Regions: data.OV.RegionProgress,
		}
		return dispatchByFormat(in.Format,
			func() (reportsapi.RenderResult, error) { return project_summary.RenderXLSX(ps) },
			func() (reportsapi.RenderResult, error) { return project_summary.RenderCSV(ps) },
			func() (reportsapi.RenderResult, error) { return project_summary.RenderPDF(ps) },
		)
	}
	return reportsapi.RenderResult{}, fmt.Errorf("renderByKind: %w: %s", reportsapi.ErrUnknownKind, in.Kind)
}

// dispatchByFormat picks the right renderer thunk by format. The
// argument order (xlsx, csv, pdf) mirrors the ExportFormat enum
// declaration; a missing format returns ErrUnsupportedFmt — defensive
// guard, normally caught by validateRunInput.
func dispatchByFormat(
	f reportsapi.ExportFormat,
	xlsx, csv, pdf func() (reportsapi.RenderResult, error),
) (reportsapi.RenderResult, error) {
	switch f {
	case reportsapi.FormatXLSX:
		return xlsx()
	case reportsapi.FormatCSV:
		return csv()
	case reportsapi.FormatPDF:
		return pdf()
	}
	return reportsapi.RenderResult{}, fmt.Errorf("dispatchByFormat: %w: %s", reportsapi.ErrUnsupportedFmt, f)
}
