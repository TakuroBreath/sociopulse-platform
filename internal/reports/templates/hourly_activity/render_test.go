package hourly_activity_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	tmpl "github.com/sociopulse/platform/internal/reports/templates/hourly_activity"
)

func sample() service.HourlyActivityData {
	return service.HourlyActivityData{
		Window: analyticsapi.Window{
			From: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		},
		Buckets: []analyticsapi.HourlyBucket{
			{Hour: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC), Count: 42, AvgDurSec: 120.5},
			{Hour: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC), Count: 55, AvgDurSec: 95.0},
			{Hour: time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC), Count: 60, AvgDurSec: 88.0},
		},
	}
}

func TestRenderXLSX_RoundTripsViaExcelize(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderXLSX(sample())
	require.NoError(t, err)
	require.NotEmpty(t, res.Bytes)
	require.Contains(t, res.Filename, "hourly_activity_")
	require.Equal(t, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", res.MIME)
	require.Len(t, res.SHA256, 64)

	f, err := excelize.OpenReader(bytes.NewReader(res.Bytes))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	v, err := f.GetCellValue("hourly_activity", "A1")
	require.NoError(t, err)
	require.Equal(t, "Hour", v)
	v, err = f.GetCellValue("hourly_activity", "B2")
	require.NoError(t, err)
	require.Equal(t, "42", v)
}

func TestRenderCSV_BOMAndRowCount(t *testing.T) {
	t.Parallel()
	res, err := tmpl.RenderCSV(sample())
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(res.Bytes, []byte{0xEF, 0xBB, 0xBF}), "expect UTF-8 BOM")
	require.Equal(t, "text/csv; charset=utf-8", res.MIME)
	// header + 3 buckets = 4 newlines
	require.Equal(t, 4, bytes.Count(res.Bytes, []byte("\n")))
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
	d.Buckets = make([]analyticsapi.HourlyBucket, 5001)
	_, err := tmpl.RenderPDF(d)
	require.ErrorIs(t, err, reportsapi.ErrTooLarge)
}
