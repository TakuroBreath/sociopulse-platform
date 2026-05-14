package hourly_activity //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"fmt"
	"strconv"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

// RenderPDF emits a (Hour, Count, AvgDurSec) table. Hour is formatted as
// "2006-01-02 15:04" (human-readable). >5000 buckets return
// reportsapi.ErrTooLarge.
func RenderPDF(data service.HourlyActivityData) (reportsapi.RenderResult, error) {
	if len(data.Buckets) > common.PDFRowLimit {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.pdf: %d rows > %d cap: %w",
			len(data.Buckets), common.PDFRowLimit, reportsapi.ErrTooLarge)
	}
	pdf, err := common.PDFInit()
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.pdf: %w", err)
	}
	defer func() { _ = pdf.Close() }()
	if err := common.PDFHeader(pdf, "Hourly Activity"); err != nil {
		return reportsapi.RenderResult{}, err
	}
	widths := []float64{160, 80, 80}
	header := []string{"Hour", "Count", "AvgDurSec"}
	writeHeader := func(y float64) (float64, error) {
		return common.PDFRow(pdf, y, header, widths)
	}
	y := 80.0
	y, err = writeHeader(y)
	if err != nil {
		return reportsapi.RenderResult{}, err
	}
	for _, b := range data.Buckets {
		y, err = common.PDFRow(pdf, y, []string{
			b.Hour.UTC().Format("2006-01-02 15:04"),
			strconv.FormatUint(b.Count, 10),
			strconv.FormatFloat(b.AvgDurSec, 'f', 2, 64),
		}, widths)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
		if y > 800 {
			pdf.AddPage()
			y = 40
			y, err = writeHeader(y)
			if err != nil {
				return reportsapi.RenderResult{}, err
			}
		}
	}
	buf := &bytes.Buffer{}
	if _, err := pdf.WriteTo(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.pdf: WriteTo: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMEPDF, data.Window.From), nil
}
