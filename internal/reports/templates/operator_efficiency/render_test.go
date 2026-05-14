package operator_efficiency_test

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
	tmpl "github.com/sociopulse/platform/internal/reports/templates/operator_efficiency"
)

func sample() service.OperatorEfficiencyData {
	return service.OperatorEfficiencyData{
		Window: analyticsapi.Window{
			From: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		},
		Rows: []service.OperatorEfficiencyRow{
			{OperatorID: uuid.New(), DisplayName: "Алиса", CallsTotal: 120, SuccessRate: 0.65, AvgTalkSec: 180, PauseShare: 0.12, AboveTeamAvg: true},
			{OperatorID: uuid.New(), DisplayName: "Боб", CallsTotal: 90, SuccessRate: 0.50, AvgTalkSec: 150, PauseShare: 0.18, AboveTeamAvg: false},
		},
	}
}

func TestRenderXLSX_RoundTripsViaExcelize(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderXLSX(sample())
	require.NoError(t, err)
	require.NotEmpty(t, res.Bytes)
	require.Contains(t, res.Filename, "operator_efficiency_")
	require.Equal(t, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", res.MIME)
	require.Len(t, res.SHA256, 64)

	f, err := excelize.OpenReader(bytes.NewReader(res.Bytes))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	v, err := f.GetCellValue("operator_efficiency", "A1")
	require.NoError(t, err)
	require.Equal(t, "Operator", v)
	v, err = f.GetCellValue("operator_efficiency", "A2")
	require.NoError(t, err)
	require.Equal(t, "Алиса", v) // Cyrillic round-trip
}

func TestRenderCSV_BOMAndRowCount(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderCSV(sample())
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(res.Bytes, []byte{0xEF, 0xBB, 0xBF}), "expect UTF-8 BOM")
	require.Equal(t, "text/csv; charset=utf-8", res.MIME)
	// header + 2 data rows = 3 newlines
	require.Equal(t, 3, bytes.Count(res.Bytes, []byte("\n")))
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
	d.Rows = make([]service.OperatorEfficiencyRow, 5001)
	_, err := tmpl.RenderPDF(d)
	require.ErrorIs(t, err, reportsapi.ErrTooLarge)
}
