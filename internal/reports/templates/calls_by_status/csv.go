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

// RenderCSV emits 4 summary rows + blank line + header + N bucket rows.
func RenderCSV(data service.CallsByStatusData) (reportsapi.RenderResult, error) {
	buf := &bytes.Buffer{}
	w, err := common.NewCSVWriter(buf)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.csv: NewCSVWriter: %w", err)
	}

	scalars := [][]string{
		{"Total", strconv.FormatUint(data.Result.Total, 10)},
		{"Successful", strconv.FormatUint(data.Result.Successful, 10)},
		{"Failed", strconv.FormatUint(data.Result.Failed, 10)},
		{"Refusals", strconv.FormatUint(data.Result.Refusals, 10)},
	}
	for _, row := range scalars {
		if err := w.Write(row); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.csv: scalar: %w", err)
		}
	}
	if err := w.Write([]string{""}); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.csv: blank: %w", err)
	}
	if err := w.Write([]string{"Status", "Count"}); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.csv: header: %w", err)
	}
	for _, b := range data.Result.ByStatus {
		if err := w.Write([]string{b.Status, strconv.FormatUint(b.Count, 10)}); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.csv: bucket: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.csv: flush: %w", err)
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("calls_by_status_%s.csv", data.Window.From.Format("20060102")),
		MIME:     "text/csv; charset=utf-8",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
