// Package calls_by_status renders the calls-by-status report (4 summary
// scalars + a (Status, Count) breakdown table) in XLSX/CSV/PDF.
package calls_by_status //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"fmt"

	"github.com/xuri/excelize/v2"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/templates/common"
	tpldata "github.com/sociopulse/platform/internal/reports/templates/data"
)

const (
	sheetName = "calls_by_status"
	kind      = "calls_by_status"
)

// RenderXLSX produces a single-sheet workbook with 4 summary rows + a
// header + N status-bucket rows.
func RenderXLSX(data tpldata.CallsByStatusData) (reportsapi.RenderResult, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	idx, err := f.NewSheet(sheetName)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: NewSheet: %w", err)
	}
	f.SetActiveSheet(idx)
	if err := f.DeleteSheet("Sheet1"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: DeleteSheet: %w", err)
	}

	hdrStyle, err := common.HeaderStyle(f)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: HeaderStyle: %w", err)
	}

	// 4 summary scalar rows (rows 1..4): Total, Successful, Failed, Refusals.
	scalars := [][]any{
		{"Total", data.Result.Total},
		{"Successful", data.Result.Successful},
		{"Failed", data.Result.Failed},
		{"Refusals", data.Result.Refusals},
	}
	for r, row := range scalars {
		for i, v := range row {
			axis, _ := excelize.CoordinatesToCellName(i+1, r+1)
			if err := f.SetCellValue(sheetName, axis, v); err != nil {
				return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: scalar[%d][%d]: %w", r, i, err)
			}
		}
	}

	// Header row at row 6, then ByStatus from row 7.
	headerRow := 6
	axisA, _ := excelize.CoordinatesToCellName(1, headerRow)
	axisB, _ := excelize.CoordinatesToCellName(2, headerRow)
	if err := f.SetCellValue(sheetName, axisA, "Status"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: header Status: %w", err)
	}
	if err := f.SetCellValue(sheetName, axisB, "Count"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: header Count: %w", err)
	}
	if err := f.SetCellStyle(sheetName, axisA, axisB, hdrStyle); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: SetCellStyle header: %w", err)
	}
	for i, bucket := range data.Result.ByStatus {
		row := headerRow + 1 + i
		ax1, _ := excelize.CoordinatesToCellName(1, row)
		ax2, _ := excelize.CoordinatesToCellName(2, row)
		if err := f.SetCellValue(sheetName, ax1, bucket.Status); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: bucket[%d] status: %w", i, err)
		}
		if err := f.SetCellValue(sheetName, ax2, bucket.Count); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: bucket[%d] count: %w", i, err)
		}
	}

	buf := &bytes.Buffer{}
	if err := f.Write(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("calls_by_status.xlsx: Write: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMEXlsx, data.Window.From), nil
}
