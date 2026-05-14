package calls_by_status //nolint:revive // package name mirrors the module's filesystem path

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

// RenderPDF emits 4 summary lines + (Status, Count) table. >5000 buckets
// return reportsapi.ErrTooLarge.
func RenderPDF(data service.CallsByStatusData) (reportsapi.RenderResult, error) {
	if len(data.Result.ByStatus) > pdfRowLimit {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.pdf: %d rows > %d cap: %w",
			len(data.Result.ByStatus), pdfRowLimit, reportsapi.ErrTooLarge)
	}
	pdf, err := common.PDFInit()
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.pdf: %w", err)
	}
	defer pdf.Close()
	if err := common.PDFHeader(pdf, "Calls by Status"); err != nil {
		return reportsapi.RenderResult{}, err
	}
	widths2 := []float64{160, 160}
	y := 80.0
	scalars := [][]string{
		{"Total", strconv.FormatUint(data.Result.Total, 10)},
		{"Successful", strconv.FormatUint(data.Result.Successful, 10)},
		{"Failed", strconv.FormatUint(data.Result.Failed, 10)},
		{"Refusals", strconv.FormatUint(data.Result.Refusals, 10)},
	}
	for _, row := range scalars {
		y, err = common.PDFRow(pdf, y, row, widths2)
		if err != nil {
			return reportsapi.RenderResult{}, err
		}
	}
	y += 12
	y, err = common.PDFRow(pdf, y, []string{"Status", "Count"}, widths2)
	if err != nil {
		return reportsapi.RenderResult{}, err
	}
	for _, b := range data.Result.ByStatus {
		y, err = common.PDFRow(pdf, y, []string{b.Status, strconv.FormatUint(b.Count, 10)}, widths2)
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
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.pdf: WriteTo: %w", err)
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("calls_by_status_%s.pdf", data.Window.From.Format("20060102")),
		MIME:     "application/pdf",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
