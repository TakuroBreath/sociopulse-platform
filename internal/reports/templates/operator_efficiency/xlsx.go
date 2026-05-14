// Package operator_efficiency renders the operator-efficiency report
// (one row per operator) in XLSX/CSV/PDF.
package operator_efficiency //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"fmt"

	"github.com/xuri/excelize/v2"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

const (
	sheetName = "operator_efficiency"
	kind      = "operator_efficiency"
)

// RenderXLSX produces an .xlsx artifact for the operator-efficiency
// projection. One header row + N body rows under the "operator_efficiency"
// sheet (Excel constrains sheet names to ASCII ≤31 chars).
func RenderXLSX(data service.OperatorEfficiencyData) (reportsapi.RenderResult, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	idx, err := f.NewSheet(sheetName)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.xlsx: NewSheet: %w", err)
	}
	f.SetActiveSheet(idx)
	if err := f.DeleteSheet("Sheet1"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.xlsx: DeleteSheet: %w", err)
	}

	headers := []string{"Operator", "CallsTotal", "SuccessRate", "AvgTalkSec", "PauseShare", "AboveTeamAvg"}
	hdrStyle, err := common.HeaderStyle(f)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.xlsx: HeaderStyle: %w", err)
	}
	for i, h := range headers {
		axis, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err := f.SetCellValue(sheetName, axis, h); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.xlsx: SetCellValue header[%d]: %w", i, err)
		}
		if err := f.SetCellStyle(sheetName, axis, axis, hdrStyle); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.xlsx: SetCellStyle: %w", err)
		}
	}
	for r, row := range data.Rows {
		rowIdx := r + 2
		cells := []any{
			row.DisplayName, row.CallsTotal, row.SuccessRate,
			row.AvgTalkSec, row.PauseShare, row.AboveTeamAvg,
		}
		for i, v := range cells {
			axis, _ := excelize.CoordinatesToCellName(i+1, rowIdx)
			if err := f.SetCellValue(sheetName, axis, v); err != nil {
				return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.xlsx: SetCellValue row[%d][%d]: %w", r, i, err)
			}
		}
	}

	buf := &bytes.Buffer{}
	if err := f.Write(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("operator_efficiency.xlsx: Write: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMEXlsx, data.Window.From), nil
}
