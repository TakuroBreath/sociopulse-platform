// Package finance renders the finance report (5-row cost summary:
// per-minute rate, total minutes, total cost) in XLSX/CSV/PDF.
//
// v1 is the "per-call cost" projection. Full margin/billing logic lands in
// Plan 14; here we surface CallsResult totals × a fixed per-minute rate.
package finance

import (
	"bytes"
	"fmt"

	"github.com/xuri/excelize/v2"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

const (
	sheetName = "finance"
	kind      = "finance"
)

// RenderXLSX produces a single-sheet workbook with 5 labeled rows. No PDF
// row cap because the row count is fixed.
func RenderXLSX(data service.FinanceData) (reportsapi.RenderResult, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	idx, err := f.NewSheet(sheetName)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.xlsx: NewSheet: %w", err)
	}
	f.SetActiveSheet(idx)
	if err := f.DeleteSheet("Sheet1"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.xlsx: DeleteSheet: %w", err)
	}

	hdrStyle, err := common.HeaderStyle(f)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.xlsx: HeaderStyle: %w", err)
	}

	rows := [][]any{
		{"Metric", "Value"},
		{"TotalCalls", data.Calls.Total},
		{"TotalDurSec", data.Calls.TotalDurSec},
		{"TotalMinutes", data.TotalMinutes},
		{"PerMinuteRateRub", data.PerMinuteRate},
		{"TotalCostRub", data.TotalCostRub},
	}
	for r, row := range rows {
		for i, v := range row {
			axis, _ := excelize.CoordinatesToCellName(i+1, r+1)
			if err := f.SetCellValue(sheetName, axis, v); err != nil {
				return reportsapi.RenderResult{}, fmt.Errorf("finance.xlsx: SetCellValue[%d][%d]: %w", r, i, err)
			}
			if r == 0 {
				if err := f.SetCellStyle(sheetName, axis, axis, hdrStyle); err != nil {
					return reportsapi.RenderResult{}, fmt.Errorf("finance.xlsx: SetCellStyle: %w", err)
				}
			}
		}
	}

	buf := &bytes.Buffer{}
	if err := f.Write(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("finance.xlsx: Write: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMEXlsx, data.Window.From), nil
}
