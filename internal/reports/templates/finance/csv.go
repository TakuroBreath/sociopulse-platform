package finance

import (
	"bytes"
	"fmt"
	"strconv"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/templates/common"
	tpldata "github.com/sociopulse/platform/internal/reports/templates/data"
)

// RenderCSV emits 5 single-(metric, value) rows under a header.
func RenderCSV(data tpldata.FinanceData) (reportsapi.RenderResult, error) {
	buf := &bytes.Buffer{}
	w, err := common.NewCSVWriter(buf)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.csv: NewCSVWriter: %w", err)
	}
	if err := w.Write([]string{"Metric", "Value"}); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.csv: header: %w", err)
	}
	rows := [][]string{
		{"TotalCalls", strconv.FormatUint(data.Calls.Total, 10)},
		{"TotalDurSec", strconv.FormatUint(data.Calls.TotalDurSec, 10)},
		{"TotalMinutes", strconv.FormatFloat(data.TotalMinutes, 'f', 2, 64)},
		{"PerMinuteRateRub", strconv.FormatFloat(data.PerMinuteRate, 'f', 2, 64)},
		{"TotalCostRub", strconv.FormatFloat(data.TotalCostRub, 'f', 2, 64)},
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("finance.csv: row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.csv: flush: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMECSV, data.Window.From), nil
}
