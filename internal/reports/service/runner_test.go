package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	auditapi "github.com/sociopulse/platform/internal/audit/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	reportsvc "github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeTenantRunner runs every fn synchronously with a zero postgres.Tx —
// the AuditEmitter never reads from it, so we don't need to spin up a
// real database. The recorded tenant ids let tests confirm Runner.Run
// scoped the audit append to the request's tenant.
type fakeTenantRunner struct {
	tenants []uuid.UUID
}

func (f *fakeTenantRunner) WithTenant(_ context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error {
	f.tenants = append(f.tenants, tenantID)
	return fn(postgres.Tx{})
}

// Compile-time assertion: the fake matches the runner's narrow
// transaction-runner port.
var _ reportsvc.TenantRunner = (*fakeTenantRunner)(nil)

// Fixed UTC instants used by every runner test — independent of the
// wall clock so the audit timestamp / synthetic-job-id assertions are
// reproducible.
var (
	runnerFrom = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	runnerTo   = time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC) // 7-day window — under default 30d threshold
	runnerNow  = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
)

func runnerNowFn() time.Time { return runnerNow }

// fixedTenantID is the tenant every runner test pretends to act for.
var fixedTenantID = uuid.MustParse("11111111-1111-1111-1111-111111111111")

// fixedActorID is the user every runner test pretends to be.
var fixedActorID = uuid.MustParse("22222222-2222-2222-2222-222222222222")

// fixedProjectID is the project parameter the kinds that need it use.
var fixedProjectID = uuid.MustParse("33333333-3333-3333-3333-333333333333")

func newRunInput(kind reportsapi.ReportKind, format reportsapi.ExportFormat, params map[string]any) reportsapi.RunInput {
	return reportsapi.RunInput{
		Kind:     kind,
		Format:   format,
		Params:   params,
		Window:   analyticsapi.Window{From: runnerFrom, To: runnerTo},
		TenantID: fixedTenantID,
		ActorID:  fixedActorID,
	}
}

// buildRunner returns a Runner with the supplied fakes; useful from the
// majority of tests that share a 7-day window and the canonical 30-day
// threshold.
func buildRunner(t *testing.T, ana analyticsapi.ServiceRO, fw *fakeOutboxWriter, ftr *fakeTenantRunner) *reportsvc.Runner {
	t.Helper()
	return reportsvc.NewRunner(reportsvc.RunnerDeps{
		Analytics: ana,
		TxRunner:  ftr,
		Audit:     reportsvc.NewAuditEmitter(fw),
		Threshold: reportsvc.ThresholdConfig{AsyncPeriodDays: 30, AsyncRowThreshold: 100_000},
		Now:       runnerNowFn,
	})
}

// -----------------------------------------------------------------------
// Happy paths
// -----------------------------------------------------------------------

func TestRunner_Run_OperatorEfficiency_Xlsx_Happy(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{
		comparisons: []analyticsapi.OperatorComparisonRow{{
			OperatorID:   uuid.MustParse("44444444-4444-4444-4444-444444444444"),
			DisplayName:  "Иван Петров",
			CallsTotal:   42,
			SuccessRate:  0.5,
			AvgTalkSec:   180.0,
			PauseShare:   0.2,
			AboveTeamAvg: true,
		}},
	}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)

	res, err := r.Run(context.Background(),
		newRunInput(reportsapi.KindOperatorEfficiency, reportsapi.FormatXLSX,
			map[string]any{"project_id": fixedProjectID.String()}))
	require.NoError(t, err)
	require.Equal(t, common.MIMEXlsx, res.MIME)
	require.NotEmpty(t, res.Bytes)

	// XLSX bytes must be a valid workbook readable by excelize.
	f, err := excelize.OpenReader(bytes.NewReader(res.Bytes))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Audit was emitted with the correct subject + scoped to the request tenant.
	require.Len(t, ftr.tenants, 1)
	require.Equal(t, fixedTenantID, ftr.tenants[0])
	require.Len(t, fw.appended, 1)
	require.Equal(t, "tenant.11111111-1111-1111-1111-111111111111.audit.event", fw.appended[0].Subject)
}

func TestRunner_Run_CustomViaRunAlwaysAsyncRequired(t *testing.T) {
	t.Parallel()

	// Custom kind always trips IsAsyncRequired regardless of window /
	// row count — Run must return ErrAsyncRequired before any rendering
	// or audit happens. The renderer mapping for Custom (which projects
	// CustomData onto ProjectSummaryData) is exercised by the sibling
	// TestRunner_Run_CustomRendererPath_Direct via the RenderForTest
	// seam, since the async Consumer (Task 6) is what actually invokes
	// the renderer dispatcher for Custom in production.
	fa := &fakeAnalytics{
		overview: analyticsapi.OverviewResult{
			Calls: analyticsapi.CallsResult{Total: 123},
		},
	}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)
	_, err := r.Run(context.Background(),
		newRunInput(reportsapi.KindCustom, reportsapi.FormatXLSX, map[string]any{}))
	require.ErrorIs(t, err, reportsapi.ErrAsyncRequired,
		"KindCustom must always route to async per threshold rules")
	require.Empty(t, fw.appended, "no audit on async-required path")
}

// TestRunner_Run_CustomRendererPath_Direct exercises the Custom→
// project_summary mapping directly via the exported render dispatcher.
// The runner refuses Custom at the threshold gate, so the renderer
// dispatch is what the Task 6 async Consumer will invoke.
func TestRunner_Run_CustomRendererPath_Direct(t *testing.T) {
	t.Parallel()
	for _, format := range []reportsapi.ExportFormat{reportsapi.FormatXLSX, reportsapi.FormatCSV, reportsapi.FormatPDF} {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			fa := &fakeAnalytics{
				overview: analyticsapi.OverviewResult{
					Calls:          analyticsapi.CallsResult{Total: 100, Successful: 80},
					OperatorState:  analyticsapi.OperatorStateBreakdown{TalkSec: 3600},
					RegionProgress: []analyticsapi.RegionProgressRow{{RegionCode: "RU-MOW", Done: 5, Plan: 10, Progress: 0.5}},
				},
			}
			res, err := reportsvc.RenderForTest(context.Background(), fa,
				newRunInput(reportsapi.KindCustom, format, map[string]any{}))
			require.NoError(t, err)
			require.NotEmpty(t, res.Bytes)
		})
	}
}

// -----------------------------------------------------------------------
// Threshold refusal
// -----------------------------------------------------------------------

func TestRunner_Run_RefusesWhenAsyncRequired(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := reportsvc.NewRunner(reportsvc.RunnerDeps{
		Analytics: fa,
		TxRunner:  ftr,
		Audit:     reportsvc.NewAuditEmitter(fw),
		Threshold: reportsvc.ThresholdConfig{AsyncPeriodDays: 30, AsyncRowThreshold: 100_000},
		Now:       runnerNowFn,
	})

	// 31-day window trips the >30d period threshold.
	longWindow := analyticsapi.Window{From: runnerFrom, To: runnerFrom.Add(31 * 24 * time.Hour)}
	in := reportsapi.RunInput{
		Kind:     reportsapi.KindOperatorEfficiency,
		Format:   reportsapi.FormatXLSX,
		Params:   map[string]any{"project_id": fixedProjectID.String()},
		Window:   longWindow,
		TenantID: fixedTenantID,
		ActorID:  fixedActorID,
	}
	_, err := r.Run(context.Background(), in)
	require.ErrorIs(t, err, reportsapi.ErrAsyncRequired)
	require.Empty(t, fw.appended, "no audit on async-required refusal")
	require.Empty(t, ftr.tenants, "no transaction on async-required refusal")
}

// -----------------------------------------------------------------------
// Renderer errors
// -----------------------------------------------------------------------

func TestRunner_Run_BubblesRendererErrTooLarge(t *testing.T) {
	t.Parallel()

	// 5001 rows trips operator_efficiency PDF's PDFRowLimit cap.
	rows := make([]analyticsapi.OperatorComparisonRow, 0, common.PDFRowLimit+1)
	for range common.PDFRowLimit + 1 {
		rows = append(rows, analyticsapi.OperatorComparisonRow{
			OperatorID:  uuid.New(),
			DisplayName: "Op",
		})
	}
	fa := &fakeAnalytics{comparisons: rows}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)

	_, err := r.Run(context.Background(),
		newRunInput(reportsapi.KindOperatorEfficiency, reportsapi.FormatPDF,
			map[string]any{"project_id": fixedProjectID.String()}))
	require.ErrorIs(t, err, reportsapi.ErrTooLarge)
	require.Empty(t, fw.appended, "no audit on renderer failure")
}

// -----------------------------------------------------------------------
// Fetcher errors
// -----------------------------------------------------------------------

func TestRunner_Run_BubblesFetcherErrInvalidParams(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)

	// operator_efficiency requires project_id.
	_, err := r.Run(context.Background(),
		newRunInput(reportsapi.KindOperatorEfficiency, reportsapi.FormatXLSX, map[string]any{}))
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
	require.Empty(t, fw.appended, "no audit on fetcher failure")
}

// -----------------------------------------------------------------------
// Input validation
// -----------------------------------------------------------------------

func TestRunner_Run_RejectsInvalidWindow(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)

	in := reportsapi.RunInput{
		Kind:     reportsapi.KindOperatorEfficiency,
		Format:   reportsapi.FormatXLSX,
		Params:   map[string]any{"project_id": fixedProjectID.String()},
		Window:   analyticsapi.Window{From: runnerTo, To: runnerFrom}, // inverted
		TenantID: fixedTenantID,
		ActorID:  fixedActorID,
	}
	_, err := r.Run(context.Background(), in)
	require.ErrorIs(t, err, analyticsapi.ErrInvalidWindow,
		"bare ErrInvalidWindow per Plan 13.2 lesson #3")
}

func TestRunner_Run_RejectsZeroTenant(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)

	in := newRunInput(reportsapi.KindOperatorEfficiency, reportsapi.FormatXLSX,
		map[string]any{"project_id": fixedProjectID.String()})
	in.TenantID = uuid.Nil
	_, err := r.Run(context.Background(), in)
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
}

func TestRunner_Run_RejectsUnknownKind(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)

	_, err := r.Run(context.Background(), newRunInput("bogus-kind", reportsapi.FormatXLSX, map[string]any{}))
	require.ErrorIs(t, err, reportsapi.ErrUnknownKind)
}

func TestRunner_Run_RejectsUnsupportedFormat(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)

	_, err := r.Run(context.Background(),
		newRunInput(reportsapi.KindOperatorEfficiency, "tarball",
			map[string]any{"project_id": fixedProjectID.String()}))
	require.ErrorIs(t, err, reportsapi.ErrUnsupportedFmt)
}

// -----------------------------------------------------------------------
// Audit failure
// -----------------------------------------------------------------------

func TestRunner_Run_AuditEmitFailureBubblesUp(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{
		comparisons: []analyticsapi.OperatorComparisonRow{{
			OperatorID:  uuid.New(),
			DisplayName: "x",
		}},
	}
	sentinel := errors.New("outbox-down")
	fw := &fakeOutboxWriter{nextErr: sentinel}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)

	_, err := r.Run(context.Background(),
		newRunInput(reportsapi.KindOperatorEfficiency, reportsapi.FormatXLSX,
			map[string]any{"project_id": fixedProjectID.String()}))
	require.ErrorIs(t, err, sentinel, "outbox sentinel must survive %w through the audit wrap")
}

// -----------------------------------------------------------------------
// Compile-time guard: Runner.AuditExportTimestamp uses the injected Now.
// -----------------------------------------------------------------------

func TestRunner_Run_UsesInjectedNowForAudit(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{
		comparisons: []analyticsapi.OperatorComparisonRow{{
			OperatorID:  uuid.New(),
			DisplayName: "x",
		}},
	}
	fw := &fakeOutboxWriter{}
	ftr := &fakeTenantRunner{}
	r := buildRunner(t, fa, fw, ftr)

	_, err := r.Run(context.Background(),
		newRunInput(reportsapi.KindOperatorEfficiency, reportsapi.FormatXLSX,
			map[string]any{"project_id": fixedProjectID.String()}))
	require.NoError(t, err)
	require.Len(t, fw.appended, 1)

	// The audit event JSON payload carries ts = runnerNow (the injected
	// fixed instant). Unmarshal the event and assert the timestamp.
	var ev auditapi.Event
	require.NoError(t, json.Unmarshal(fw.appended[0].Payload, &ev))
	require.True(t, ev.Timestamp.Equal(runnerNow.UTC()), "audit ts must reflect deps.Now()")
}
