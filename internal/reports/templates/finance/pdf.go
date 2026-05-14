package finance

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

// RenderPDF emits 5 (metric, value) rows under a header. PDF row cap not
// applicable — finance has a fixed 5-row layout.
func RenderPDF(data service.FinanceData) (reportsapi.RenderResult, error) {
	pdf, err := common.PDFInit()
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.pdf: %w", err)
	}
	defer pdf.Close()
	if err := common.PDFHeader(pdf, "Finance"); err != nil {
		return reportsapi.RenderResult{}, err
	}
	widths := []float64{200, 200}
	y := 80.0
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
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("finance_%s.pdf", data.Window.From.Format("20060102")),
		MIME:     "application/pdf",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
