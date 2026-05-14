package project_summary //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/signintech/gopdf"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

// RenderPDF produces a 3-section PDF. PDF row cap counts total rows across
// sections (Summary≈7 + OperatorState≈5 + Regions). The cap targets the
// Regions section because it's the only variable-length one.
func RenderPDF(data service.ProjectSummaryData) (reportsapi.RenderResult, error) {
	if len(data.Regions) > common.PDFRowLimit {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.pdf: %d regions > %d cap: %w",
			len(data.Regions), common.PDFRowLimit, reportsapi.ErrTooLarge)
	}
	pdf, err := common.PDFInit()
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.pdf: %w", err)
	}
	defer func() { _ = pdf.Close() }()

	if err := common.PDFHeader(pdf, "Project Summary"); err != nil {
		return reportsapi.RenderResult{}, err
	}

	y := 80.0
	if y, err = writeSummary(pdf, y, data); err != nil {
		return reportsapi.RenderResult{}, err
	}
	if y, err = writeState(pdf, y, data); err != nil {
		return reportsapi.RenderResult{}, err
	}
	if err = writeRegions(pdf, y, data); err != nil {
		return reportsapi.RenderResult{}, err
	}

	buf := &bytes.Buffer{}
	if _, err := pdf.WriteTo(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.pdf: WriteTo: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMEPDF, data.Window.From), nil
}

// writeSummary writes the fixed-row Calls summary section under a
// "Metric | Value" header. Returns the y after the last row.
func writeSummary(pdf *gopdf.GoPdf, y float64, data service.ProjectSummaryData) (float64, error) {
	widths := []float64{160, 160}
	y, err := common.PDFRow(pdf, y, []string{"Metric", "Value"}, widths)
	if err != nil {
		return 0, err
	}
	rows := [][]string{
		{"Total", strconv.FormatUint(data.Calls.Total, 10)},
		{"Successful", strconv.FormatUint(data.Calls.Successful, 10)},
		{"Failed", strconv.FormatUint(data.Calls.Failed, 10)},
		{"Refusals", strconv.FormatUint(data.Calls.Refusals, 10)},
		{"AvgDurSec", strconv.FormatFloat(data.Calls.AvgDurSec, 'f', 2, 64)},
		{"TotalDurSec", strconv.FormatUint(data.Calls.TotalDurSec, 10)},
	}
	for _, row := range rows {
		if y, err = common.PDFRow(pdf, y, row, widths); err != nil {
			return 0, err
		}
	}
	return y, nil
}

// writeState writes the fixed-row OperatorState section under a
// "State | Seconds" header.
func writeState(pdf *gopdf.GoPdf, y float64, data service.ProjectSummaryData) (float64, error) {
	y += 12
	pdf.SetXY(40, y)
	if err := pdf.Cell(nil, "Operator State (seconds)"); err != nil {
		return 0, fmt.Errorf("project_summary.pdf: subtitle: %w", err)
	}
	y += 24
	widths := []float64{160, 160}
	y, err := common.PDFRow(pdf, y, []string{"State", "Seconds"}, widths)
	if err != nil {
		return 0, err
	}
	rows := [][]string{
		{"Talk", strconv.FormatUint(data.State.TalkSec, 10)},
		{"Pause", strconv.FormatUint(data.State.PauseSec, 10)},
		{"Ready", strconv.FormatUint(data.State.ReadySec, 10)},
		{"Wrap", strconv.FormatUint(data.State.WrapSec, 10)},
	}
	for _, row := range rows {
		if y, err = common.PDFRow(pdf, y, row, widths); err != nil {
			return 0, err
		}
	}
	return y, nil
}

// writeRegions writes the variable-length Regions section. This is the
// only section that can page, so it carries the per-page header repeater.
func writeRegions(pdf *gopdf.GoPdf, y float64, data service.ProjectSummaryData) error {
	y += 12
	pdf.SetXY(40, y)
	if err := pdf.Cell(nil, "Regions"); err != nil {
		return fmt.Errorf("project_summary.pdf: regions title: %w", err)
	}
	y += 24
	widths := []float64{120, 80, 80, 80}
	header := []string{"Region", "Done", "Plan", "Progress"}
	writeHeader := func(y float64) (float64, error) {
		return common.PDFRow(pdf, y, header, widths)
	}
	y, err := writeHeader(y)
	if err != nil {
		return err
	}
	for _, r := range data.Regions {
		y, err = common.PDFRow(pdf, y, []string{
			r.RegionCode,
			strconv.FormatUint(r.Done, 10),
			strconv.FormatUint(r.Plan, 10),
			strconv.FormatFloat(r.Progress, 'f', 2, 64),
		}, widths)
		if err != nil {
			return err
		}
		if y > 800 {
			pdf.AddPage()
			y = 40
			if y, err = writeHeader(y); err != nil {
				return err
			}
		}
	}
	return nil
}
