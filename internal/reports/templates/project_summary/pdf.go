package project_summary //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

const pdfRowLimit = 5000

// RenderPDF produces a 3-section PDF. PDF row cap counts total rows across
// sections (Summary≈7 + OperatorState≈5 + Regions). The cap targets the
// Regions section because it's the only variable-length one.
func RenderPDF(data service.ProjectSummaryData) (reportsapi.RenderResult, error) {
	if len(data.Regions) > pdfRowLimit {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.pdf: %d regions > %d cap: %w",
			len(data.Regions), pdfRowLimit, reportsapi.ErrTooLarge)
	}
	pdf, err := common.PDFInit()
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.pdf: %w", err)
	}
	defer pdf.Close()

	// Section: Summary.
	if err := common.PDFHeader(pdf, "Project Summary"); err != nil {
		return reportsapi.RenderResult{}, err
	}
	widths2 := []float64{160, 160}
	y := 80.0
	for _, row := range [][]string{
		{"Total", strconv.FormatUint(data.Calls.Total, 10)},
		{"Successful", strconv.FormatUint(data.Calls.Successful, 10)},
		{"Failed", strconv.FormatUint(data.Calls.Failed, 10)},
		{"Refusals", strconv.FormatUint(data.Calls.Refusals, 10)},
		{"AvgDurSec", strconv.FormatFloat(data.Calls.AvgDurSec, 'f', 2, 64)},
		{"TotalDurSec", strconv.FormatUint(data.Calls.TotalDurSec, 10)},
	} {
		y, err = common.PDFRow(pdf, y, row, widths2)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
	}

	// Section: OperatorState.
	y += 12
	pdf.SetXY(40, y)
	if err := pdf.Cell(nil, "Operator State (seconds)"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.pdf: subtitle: %w", err)
	}
	y += 24
	for _, row := range [][]string{
		{"Talk", strconv.FormatUint(data.State.TalkSec, 10)},
		{"Pause", strconv.FormatUint(data.State.PauseSec, 10)},
		{"Ready", strconv.FormatUint(data.State.ReadySec, 10)},
		{"Wrap", strconv.FormatUint(data.State.WrapSec, 10)},
	} {
		y, err = common.PDFRow(pdf, y, row, widths2)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
	}

	// Section: Regions.
	y += 12
	pdf.SetXY(40, y)
	if err := pdf.Cell(nil, "Regions"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.pdf: regions title: %w", err)
	}
	y += 24
	widths4 := []float64{120, 80, 80, 80}
	y, err = common.PDFRow(pdf, y, []string{"Region", "Done", "Plan", "Progress"}, widths4)
	if err != nil {
		return reportsapi.RenderResult{}, err
	}
	for _, r := range data.Regions {
		y, err = common.PDFRow(pdf, y, []string{
			r.RegionCode,
			strconv.FormatUint(r.Done, 10),
			strconv.FormatUint(r.Plan, 10),
			strconv.FormatFloat(r.Progress, 'f', 2, 64),
		}, widths4)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
		if y > 800 {
			pdf.AddPage()
			y = 40
		}
	}

	buf := &bytes.Buffer{}
	if _, err := pdf.WriteTo(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.pdf: WriteTo: %w", err)
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("project_summary_%s.pdf", data.Window.From.Format("20060102")),
		MIME:     "application/pdf",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
