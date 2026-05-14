package finance_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/reports/service"
	tmpl "github.com/sociopulse/platform/internal/reports/templates/finance"
)

func sample() service.FinanceData {
	return service.FinanceData{
		Window: analyticsapi.Window{
			From: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		},
		Calls:         analyticsapi.CallsResult{Total: 1000, TotalDurSec: 120000},
		PerMinuteRate: 3.5,
		TotalMinutes:  2000.0,
		TotalCostRub:  7000.0,
	}
}

func TestRenderXLSX_RoundTripsViaExcelize(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderXLSX(sample())
	require.NoError(t, err)
	require.NotEmpty(t, res.Bytes)
	require.Contains(t, res.Filename, "finance_")
	require.Equal(t, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", res.MIME)
	require.Len(t, res.SHA256, 64)

	f, err := excelize.OpenReader(bytes.NewReader(res.Bytes))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	v, err := f.GetCellValue("finance", "A1")
	require.NoError(t, err)
	require.Equal(t, "Metric", v)
	v, err = f.GetCellValue("finance", "A2")
	require.NoError(t, err)
	require.Equal(t, "TotalCalls", v)
}

func TestRenderCSV_BOMAndRowCount(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderCSV(sample())
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(res.Bytes, []byte{0xEF, 0xBB, 0xBF}), "expect UTF-8 BOM")
	require.Equal(t, "text/csv; charset=utf-8", res.MIME)
	// header + 5 data rows = 6 newlines
	require.Equal(t, 6, bytes.Count(res.Bytes, []byte("\n")))
}

func TestRenderPDF_ValidPrefix(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderPDF(sample())
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(res.Bytes, []byte("%PDF-")), "expect PDF magic")
	require.Equal(t, "application/pdf", res.MIME)
}
