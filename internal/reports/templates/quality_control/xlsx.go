// Package quality_control renders the quality-control report (refusals +
// failed-call signal projection over the calls breakdown) in XLSX/CSV/PDF.
//
// v1 surfaces CallsResult (refusals + failed are the signal); v2 will add
// per-call review scores once those exist.
package quality_control //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/xuri/excelize/v2"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

const sheetName = "quality_control"

// RenderXLSX produces a single-sheet workbook with 4 summary rows + a
// (Status, Count) breakdown table.
func RenderXLSX(data service.QualityControlData) (reportsapi.RenderResult, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	idx, err := f.NewSheet(sheetName)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: NewSheet: %w", err)
	}
	f.SetActiveSheet(idx)
	if err := f.DeleteSheet("Sheet1"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: DeleteSheet: %w", err)
	}

	hdrStyle, err := common.HeaderStyle(f)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: HeaderStyle: %w", err)
	}

	scalars := [][]any{
		{"Total", data.Calls.Total},
		{"Successful", data.Calls.Successful},
		{"Failed", data.Calls.Failed},
		{"Refusals", data.Calls.Refusals},
	}
	for r, row := range scalars {
		for i, v := range row {
			axis, _ := excelize.CoordinatesToCellName(i+1, r+1)
			if err := f.SetCellValue(sheetName, axis, v); err != nil {
				return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: scalar[%d][%d]: %w", r, i, err)
			}
		}
	}

	headerRow := 6
	axisA, _ := excelize.CoordinatesToCellName(1, headerRow)
	axisB, _ := excelize.CoordinatesToCellName(2, headerRow)
	if err := f.SetCellValue(sheetName, axisA, "Status"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: header Status: %w", err)
	}
	if err := f.SetCellValue(sheetName, axisB, "Count"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: header Count: %w", err)
	}
	if err := f.SetCellStyle(sheetName, axisA, axisB, hdrStyle); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: SetCellStyle header: %w", err)
	}
	for i, bucket := range data.Calls.ByStatus {
		row := headerRow + 1 + i
		ax1, _ := excelize.CoordinatesToCellName(1, row)
		ax2, _ := excelize.CoordinatesToCellName(2, row)
		if err := f.SetCellValue(sheetName, ax1, bucket.Status); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: bucket[%d] status: %w", i, err)
		}
		if err := f.SetCellValue(sheetName, ax2, bucket.Count); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: bucket[%d] count: %w", i, err)
		}
	}

	buf := &bytes.Buffer{}
	if err := f.Write(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("quality_control.xlsx: Write: %w", err)
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("quality_control_%s.xlsx", data.Window.From.Format("20060102")),
		MIME:     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
