package finance

import (
	"bytes"
	"fmt"
	"strconv"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

// RenderPDF emits a "Metric | Value" header row followed by 5 (metric, value)
// rows. PDF row cap not applicable — finance has a fixed 5-row layout.
func RenderPDF(data service.FinanceData) (reportsapi.RenderResult, error) {
	pdf, err := common.PDFInit()
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.pdf: %w", err)
	}
	defer func() { _ = pdf.Close() }()
	if err := common.PDFHeader(pdf, "Finance"); err != nil {
		return reportsapi.RenderResult{}, err
	}
	widths := []float64{180, 180}
	header := []string{"Metric", "Value"}
	y := 80.0
	y, err = common.PDFRow(pdf, y, header, widths)
	if err != nil {
		return reportsapi.RenderResult{}, err
	}
	rows := [][]string{
		{"TotalCalls", strconv.FormatUint(data.Calls.Total, 10)},
		{"TotalDurSec", strconv.FormatUint(data.Calls.TotalDurSec, 10)},
		{"TotalMinutes", strconv.FormatFloat(data.TotalMinutes, 'f', 2, 64)},
		{"PerMinuteRateRub", strconv.FormatFloat(data.PerMinuteRate, 'f', 2, 64)},
		{"TotalCostRub", strconv.FormatFloat(data.TotalCostRub, 'f', 2, 64)},
	}
	for _, row := range rows {
		y, err = common.PDFRow(pdf, y, row, widths)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
	}
	buf := &bytes.Buffer{}
	if _, err := pdf.WriteTo(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.pdf: WriteTo: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMEPDF, data.Window.From), nil
}
