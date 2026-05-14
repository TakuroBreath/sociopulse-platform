// Package hourly_activity renders the per-hour activity report (Hour /
// Count / AvgDurSec) in XLSX/CSV/PDF.
//
// XLSX uses a date-formatted Hour column (NumFmt 22 via common.DateStyle).
// CSV/PDF use RFC3339 / "2006-01-02 15:04" string formatting.
package hourly_activity //nolint:revive // package name mirrors the module's filesystem path

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

const sheetName = "hourly_activity"

// RenderXLSX produces a single-sheet workbook with header + N bucket rows.
// The Hour column uses a date-format style for portability across Excel,
// Numbers, and LibreOffice (default excelize cells render as Excel-serial
// floats otherwise).
func RenderXLSX(data service.HourlyActivityData) (reportsapi.RenderResult, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	idx, err := f.NewSheet(sheetName)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: NewSheet: %w", err)
	}
	f.SetActiveSheet(idx)
	if err := f.DeleteSheet("Sheet1"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: DeleteSheet: %w", err)
	}

	hdrStyle, err := common.HeaderStyle(f)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: HeaderStyle: %w", err)
	}
	dateStyle, err := common.DateStyle(f)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: DateStyle: %w", err)
	}

	headers := []string{"Hour", "Count", "AvgDurSec"}
	for i, h := range headers {
		axis, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err := f.SetCellValue(sheetName, axis, h); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: header[%d]: %w", i, err)
		}
		if err := f.SetCellStyle(sheetName, axis, axis, hdrStyle); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: SetCellStyle header: %w", err)
		}
	}
	for r, b := range data.Buckets {
		row := r + 2
		axHour, _ := excelize.CoordinatesToCellName(1, row)
		axCount, _ := excelize.CoordinatesToCellName(2, row)
		axAvg, _ := excelize.CoordinatesToCellName(3, row)
		if err := f.SetCellValue(sheetName, axHour, b.Hour); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: bucket[%d] hour: %w", r, err)
		}
		if err := f.SetCellStyle(sheetName, axHour, axHour, dateStyle); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: bucket[%d] hour style: %w", r, err)
		}
		if err := f.SetCellValue(sheetName, axCount, b.Count); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: bucket[%d] count: %w", r, err)
		}
		if err := f.SetCellValue(sheetName, axAvg, b.AvgDurSec); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: bucket[%d] avg: %w", r, err)
		}
	}

	buf := &bytes.Buffer{}
	if err := f.Write(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.xlsx: Write: %w", err)
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("hourly_activity_%s.xlsx", data.Window.From.Format("20060102")),
		MIME:     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
