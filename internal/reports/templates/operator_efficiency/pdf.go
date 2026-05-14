package operator_efficiency //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"fmt"
	"strconv"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

// RenderPDF produces a PDF table of operator KPIs. Renderers with >5000
// detail rows return reportsapi.ErrTooLarge so the runner can route the
// caller to an XLSX fallback or async path.
func RenderPDF(data service.OperatorEfficiencyData) (reportsapi.RenderResult, error) {
	if len(data.Rows) > common.PDFRowLimit {
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.pdf: %d rows > %d cap: %w",
			len(data.Rows), common.PDFRowLimit, reportsapi.ErrTooLarge)
	}
	pdf, err := common.PDFInit()
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.pdf: %w", err)
	}
	defer func() { _ = pdf.Close() }()
	if err := common.PDFHeader(pdf, "Operator Efficiency"); err != nil {
		return reportsapi.RenderResult{}, err
	}
	widths := []float64{120, 60, 80, 80, 80, 80}
	header := []string{"Operator", "Calls", "Success", "AvgTalk", "Pause", "Above?"}
	writeHeader := func(y float64) (float64, error) {
		return common.PDFRow(pdf, y, header, widths)
	}

	y := 80.0
	y, err = writeHeader(y)
	if err != nil {
		return reportsapi.RenderResult{}, err
	}

	for _, row := range data.Rows {
		y, err = common.PDFRow(pdf, y, []string{
			row.DisplayName,
			strconv.FormatUint(row.CallsTotal, 10),
			strconv.FormatFloat(row.SuccessRate, 'f', 2, 64),
			strconv.FormatFloat(row.AvgTalkSec, 'f', 1, 64),
			strconv.FormatFloat(row.PauseShare, 'f', 2, 64),
			strconv.FormatBool(row.AboveTeamAvg),
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
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.pdf: WriteTo: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMEPDF, data.Window.From), nil
}
