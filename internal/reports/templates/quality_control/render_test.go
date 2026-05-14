package quality_control_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	tmpl "github.com/sociopulse/platform/internal/reports/templates/quality_control"
)

func sample() service.QualityControlData {
	return service.QualityControlData{
		Window: analyticsapi.Window{
			From: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		},
		Calls: analyticsapi.CallsResult{
			Total: 800, Successful: 400, Failed: 250, Refusals: 150,
			ByStatus: []analyticsapi.StatusBucket{
				{Status: "success", Count: 400},
				{Status: "refused", Count: 150},
				{Status: "failed", Count: 250},
			},
		},
	}
}

func TestRenderXLSX_RoundTripsViaExcelize(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderXLSX(sample())
	require.NoError(t, err)
	require.NotEmpty(t, res.Bytes)
	require.Contains(t, res.Filename, "quality_control_")
	require.Equal(t, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", res.MIME)
	require.Len(t, res.SHA256, 64)

	f, err := excelize.OpenReader(bytes.NewReader(res.Bytes))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	v, err := f.GetCellValue("quality_control", "A1")
	require.NoError(t, err)
	require.Equal(t, "Total", v)
	v, err = f.GetCellValue("quality_control", "A6")
	require.NoError(t, err)
	require.Equal(t, "Status", v)
}

func TestRenderCSV_BOMAndRowCount(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderCSV(sample())
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(res.Bytes, []byte{0xEF, 0xBB, 0xBF}), "expect UTF-8 BOM")
	require.Equal(t, "text/csv; charset=utf-8", res.MIME)
	// 4 scalars + 1 blank + 1 header + 3 buckets = 9 newlines
	require.Equal(t, 9, bytes.Count(res.Bytes, []byte("\n")))
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
	d.Calls.ByStatus = make([]analyticsapi.StatusBucket, 5001)
	_, err := tmpl.RenderPDF(d)
	require.ErrorIs(t, err, reportsapi.ErrTooLarge)
}
