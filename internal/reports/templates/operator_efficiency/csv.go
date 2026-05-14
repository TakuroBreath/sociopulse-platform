package operator_efficiency //nolint:revive // package name mirrors the module's filesystem path

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

// RenderCSV produces a UTF-8 BOM-prefixed CSV with one header + N rows.
func RenderCSV(data service.OperatorEfficiencyData) (reportsapi.RenderResult, error) {
	buf := &bytes.Buffer{}
	w, err := common.NewCSVWriter(buf)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.csv: NewCSVWriter: %w", err)
	}
	if err := w.Write([]string{"Operator", "CallsTotal", "SuccessRate", "AvgTalkSec", "PauseShare", "AboveTeamAvg"}); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.csv: header: %w", err)
	}
	for _, row := range data.Rows {
		if err := w.Write([]string{
			row.DisplayName,
			strconv.FormatUint(row.CallsTotal, 10),
			strconv.FormatFloat(row.SuccessRate, 'f', 4, 64),
			strconv.FormatFloat(row.AvgTalkSec, 'f', 2, 64),
			strconv.FormatFloat(row.PauseShare, 'f', 4, 64),
			strconv.FormatBool(row.AboveTeamAvg),
		}); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.csv: row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.csv: flush: %w", err)
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("operator_efficiency_%s.csv", data.Window.From.Format("20060102")),
		MIME:     "text/csv; charset=utf-8",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
