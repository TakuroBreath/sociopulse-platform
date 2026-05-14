package quality_control //nolint:revive // package name mirrors the module's filesystem path

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

// RenderCSV emits 4 summary rows + blank + (Status, Count) breakdown.
func RenderCSV(data service.QualityControlData) (reportsapi.RenderResult, error) {
	buf := &bytes.Buffer{}
	w, err := common.NewCSVWriter(buf)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.csv: NewCSVWriter: %w", err)
	}
	scalars := [][]string{
		{"Total", strconv.FormatUint(data.Calls.Total, 10)},
		{"Successful", strconv.FormatUint(data.Calls.Successful, 10)},
		{"Failed", strconv.FormatUint(data.Calls.Failed, 10)},
		{"Refusals", strconv.FormatUint(data.Calls.Refusals, 10)},
	}
	for _, row := range scalars {
		if err := w.Write(row); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("quality_control.csv: scalar: %w", err)
		}
	}
	if err := w.Write([]string{""}); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.csv: blank: %w", err)
	}
	if err := w.Write([]string{"Status", "Count"}); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.csv: header: %w", err)
	}
	for _, b := range data.Calls.ByStatus {
		if err := w.Write([]string{b.Status, strconv.FormatUint(b.Count, 10)}); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("quality_control.csv: bucket: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.csv: flush: %w", err)
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("quality_control_%s.csv", data.Window.From.Format("20060102")),
		MIME:     "text/csv; charset=utf-8",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
