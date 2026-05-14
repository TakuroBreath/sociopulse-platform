package quality_control //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"fmt"
	"strconv"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/templates/common"
	tpldata "github.com/sociopulse/platform/internal/reports/templates/data"
)

// RenderPDF emits 4 summary lines + (Status, Count) table. >5000 buckets
// return reportsapi.ErrTooLarge.
func RenderPDF(data tpldata.QualityControlData) (reportsapi.RenderResult, error) {
	if len(data.Calls.ByStatus) > common.PDFRowLimit {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.pdf: %d rows > %d cap: %w",
			len(data.Calls.ByStatus), common.PDFRowLimit, reportsapi.ErrTooLarge)
	}
	pdf, err := common.PDFInit()
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.pdf: %w", err)
	}
	defer func() { _ = pdf.Close() }()
	if err := common.PDFHeader(pdf, "Quality Control"); err != nil {
		return reportsapi.RenderResult{}, err
	}
	widths2 := []float64{160, 160}
	y := 80.0
	scalars := [][]string{
		{"Total", strconv.FormatUint(data.Calls.Total, 10)},
		{"Successful", strconv.FormatUint(data.Calls.Successful, 10)},
		{"Failed", strconv.FormatUint(data.Calls.Failed, 10)},
		{"Refusals", strconv.FormatUint(data.Calls.Refusals, 10)},
	}
	for _, row := range scalars {
		y, err = common.PDFRow(pdf, y, row, widths2)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
	}
	y += 12
	tableHeader := []string{"Status", "Count"}
	writeHeader := func(y float64) (float64, error) {
		return common.PDFRow(pdf, y, tableHeader, widths2)
	}
	y, err = writeHeader(y)
	if err != nil {
		return reportsapi.RenderResult{}, err
	}
	for _, b := range data.Calls.ByStatus {
		y, err = common.PDFRow(pdf, y, []string{b.Status, strconv.FormatUint(b.Count, 10)}, widths2)
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
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.pdf: WriteTo: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMEPDF, data.Window.From), nil
}
