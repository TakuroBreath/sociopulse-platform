package project_summary_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	tmpl "github.com/sociopulse/platform/internal/reports/templates/project_summary"
)

func sample() service.ProjectSummaryData {
	return service.ProjectSummaryData{
		Window: analyticsapi.Window{
			From: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		},
		Project: uuid.New(),
		Calls: analyticsapi.CallsResult{
			Total: 1000, Successful: 500, Failed: 300, Refusals: 200,
			AvgDurSec: 95.5, TotalDurSec: 95500,
			ByStatus: []analyticsapi.StatusBucket{{Status: "success", Count: 500}},
		},
		State: analyticsapi.OperatorStateBreakdown{
			TalkSec: 60000, PauseSec: 15000, ReadySec: 12000, WrapSec: 8000,
		},
		Regions: []analyticsapi.RegionProgressRow{
			{RegionCode: "RU-MOW", Done: 100, Plan: 200, Progress: 0.5},
			{RegionCode: "Регион-Север", Done: 70, Plan: 150, Progress: 0.4667},
		},
	}
}

func TestRenderXLSX_RoundTripsViaExcelize(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderXLSX(sample())
	require.NoError(t, err)
	require.NotEmpty(t, res.Bytes)
	require.Contains(t, res.Filename, "project_summary_")
	require.Equal(t, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", res.MIME)
	require.Len(t, res.SHA256, 64)

	f, err := excelize.OpenReader(bytes.NewReader(res.Bytes))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	v, err := f.GetCellValue("Summary", "A1")
	require.NoError(t, err)
	require.Equal(t, "Metric", v)
	v, err = f.GetCellValue("Regions", "A2")
	require.NoError(t, err)
	require.Equal(t, "RU-MOW", v)
}

func TestRenderCSV_BOMAndSections(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderCSV(sample())
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(res.Bytes, []byte{0xEF, 0xBB, 0xBF}), "expect UTF-8 BOM")
	require.Equal(t, "text/csv; charset=utf-8", res.MIME)
	// Summary(7) + blank + State(5) + blank + Regions(3) = 17 newlines.
	require.Equal(t, 17, bytes.Count(res.Bytes, []byte("\n")))
}

// TestRenderCSV_CyrillicRoundTrip pins the UTF-8 BOM + payload encoding
// for non-ASCII region codes — Cyrillic must survive the BOM-prefixed
// csv.Writer pipeline. (PDF Cyrillic is exercised end-to-end via the
// operator_efficiency renderer test, which uses Cyrillic operator names;
// gopdf's content-stream encoding makes PDF substring assertions
// impractical, so we don't repeat that here.)
func TestRenderCSV_CyrillicRoundTrip(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderCSV(sample())
	require.NoError(t, err)
	require.True(t, bytes.Contains(res.Bytes, []byte("Регион-Север")),
		"Cyrillic region code must round-trip through CSV unchanged")
}

func TestRenderPDF_ValidPrefix(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderPDF(sample())
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(res.Bytes, []byte("%PDF-")), "expect PDF magic")
	require.Equal(t, "application/pdf", res.MIME)
}

func TestRenderPDF_TooManyRowsReturnsErrTooLarge(t *testing.T) {
	t.Parallel()
	d := sample()
	d.Regions = make([]analyticsapi.RegionProgressRow, 5001)
	_, err := tmpl.RenderPDF(d)
	require.ErrorIs(t, err, reportsapi.ErrTooLarge)
}
