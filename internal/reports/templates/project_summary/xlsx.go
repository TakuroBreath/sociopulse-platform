// Package project_summary renders the project-summary report (3-section:
// Calls overview / OperatorState breakdown / Region progress) in
// XLSX/CSV/PDF.
package project_summary //nolint:revive // package name mirrors the module's filesystem path

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

const (
	sheetSummary = "Summary"
	sheetState   = "OperatorState"
	sheetRegions = "Regions"
)

// RenderXLSX produces a 3-sheet workbook: Summary (Calls), OperatorState,
// Regions.
func RenderXLSX(data service.ProjectSummaryData) (reportsapi.RenderResult, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	sumIdx, err := f.NewSheet(sheetSummary)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.xlsx: NewSheet Summary: %w", err)
	}
	if _, err := f.NewSheet(sheetState); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.xlsx: NewSheet OperatorState: %w", err)
	}
	if _, err := f.NewSheet(sheetRegions); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.xlsx: NewSheet Regions: %w", err)
	}
	f.SetActiveSheet(sumIdx)
	if err := f.DeleteSheet("Sheet1"); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.xlsx: DeleteSheet: %w", err)
	}

	hdrStyle, err := common.HeaderStyle(f)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.xlsx: HeaderStyle: %w", err)
	}

	// Sheet 1: Summary (Calls scalars + ByStatus table).
	summaryRows := [][]any{
		{"Metric", "Value"},
		{"Total", data.Calls.Total},
		{"Successful", data.Calls.Successful},
		{"Failed", data.Calls.Failed},
		{"Refusals", data.Calls.Refusals},
		{"AvgDurSec", data.Calls.AvgDurSec},
		{"TotalDurSec", data.Calls.TotalDurSec},
	}
	if err := writeRows(f, sheetSummary, summaryRows, hdrStyle); err != nil {
		return reportsapi.RenderResult{}, err
	}

	// Sheet 2: OperatorState breakdown.
	stateRows := [][]any{
		{"State", "Seconds"},
		{"Talk", data.State.TalkSec},
		{"Pause", data.State.PauseSec},
		{"Ready", data.State.ReadySec},
		{"Wrap", data.State.WrapSec},
	}
	if err := writeRows(f, sheetState, stateRows, hdrStyle); err != nil {
		return reportsapi.RenderResult{}, err
	}

	// Sheet 3: Regions.
	regionRows := make([][]any, 0, len(data.Regions)+1)
	regionRows = append(regionRows, []any{"RegionCode", "Done", "Plan", "Progress"})
	for _, r := range data.Regions {
		regionRows = append(regionRows, []any{r.RegionCode, r.Done, r.Plan, r.Progress})
	}
	if err := writeRows(f, sheetRegions, regionRows, hdrStyle); err != nil {
		return reportsapi.RenderResult{}, err
	}

	buf := &bytes.Buffer{}
	if err := f.Write(buf); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.xlsx: Write: %w", err)
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("project_summary_%s.xlsx", data.Window.From.Format("20060102")),
		MIME:     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}

// writeRows writes a sheet of N rows where row 0 is the styled header.
func writeRows(f *excelize.File, sheet string, rows [][]any, hdrStyle int) error {
	for r, row := range rows {
		for i, v := range row {
			axis, _ := excelize.CoordinatesToCellName(i+1, r+1)
			if err := f.SetCellValue(sheet, axis, v); err != nil {
				return fmt.Errorf("project_summary.xlsx: SetCellValue %s!%s: %w", sheet, axis, err)
			}
			if r == 0 {
				if err := f.SetCellStyle(sheet, axis, axis, hdrStyle); err != nil {
					return fmt.Errorf("project_summary.xlsx: SetCellStyle %s!%s: %w", sheet, axis, err)
				}
			}
		}
	}
	return nil
}
