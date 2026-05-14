package hourly_activity //nolint:revive // package name mirrors the module's filesystem path

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

// RenderPDF emits a (Hour, Count, AvgDurSec) table. Hour is formatted as
// "2006-01-02 15:04" (human-readable). >5000 buckets return
// reportsapi.ErrTooLarge.
func RenderPDF(data service.HourlyActivityData) (reportsapi.RenderResult, error) {
	if len(data.Buckets) > pdfRowLimit {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.pdf: %d rows > %d cap: %w",
			len(data.Buckets), pdfRowLimit, reportsapi.ErrTooLarge)
	}
	pdf, err := common.PDFInit()
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.pdf: %w", err)
	}
	defer pdf.Close()
	if err := common.PDFHeader(pdf, "Hourly Activity"); err != nil {
		return reportsapi.RenderResult{}, err
	}
	widths := []float64{160, 80, 80}
	y := 80.0
	y, err = common.PDFRow(pdf, y, []string{"Hour", "Count", "AvgDurSec"}, widths)
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
		}
	}
	buf := &bytes.Buffer{}
	if _, err := pdf.WriteTo(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.pdf: WriteTo: %w", err)
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("hourly_activity_%s.pdf", data.Window.From.Format("20060102")),
		MIME:     "application/pdf",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
