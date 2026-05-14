package project_summary //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"fmt"
	"strconv"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

// RenderCSV produces a UTF-8 BOM-prefixed CSV containing 3 sections
// separated by blank lines: Summary scalars, OperatorState breakdown,
// Region progress rows.
func RenderCSV(data service.ProjectSummaryData) (reportsapi.RenderResult, error) {
	buf := &bytes.Buffer{}
	w, err := common.NewCSVWriter(buf)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.csv: NewCSVWriter: %w", err)
	}

	// Section 1: Summary (Calls scalars).
	sections := [][][]string{
		{
			{"Metric", "Value"},
			{"Total", strconv.FormatUint(data.Calls.Total, 10)},
			{"Successful", strconv.FormatUint(data.Calls.Successful, 10)},
			{"Failed", strconv.FormatUint(data.Calls.Failed, 10)},
			{"Refusals", strconv.FormatUint(data.Calls.Refusals, 10)},
			{"AvgDurSec", strconv.FormatFloat(data.Calls.AvgDurSec, 'f', 2, 64)},
			{"TotalDurSec", strconv.FormatUint(data.Calls.TotalDurSec, 10)},
		},
		{
			{"State", "Seconds"},
			{"Talk", strconv.FormatUint(data.State.TalkSec, 10)},
			{"Pause", strconv.FormatUint(data.State.PauseSec, 10)},
			{"Ready", strconv.FormatUint(data.State.ReadySec, 10)},
			{"Wrap", strconv.FormatUint(data.State.WrapSec, 10)},
		},
	}

	// Section 3: Regions.
	regionSec := [][]string{{"RegionCode", "Done", "Plan", "Progress"}}
	for _, r := range data.Regions {
		regionSec = append(regionSec, []string{
			r.RegionCode,
			strconv.FormatUint(r.Done, 10),
			strconv.FormatUint(r.Plan, 10),
			strconv.FormatFloat(r.Progress, 'f', 4, 64),
		})
	}
	sections = append(sections, regionSec)

	for i, sec := range sections {
		for _, row := range sec {
			if err := w.Write(row); err != nil {
				return reportsapi.RenderResult{}, fmt.Errorf("project_summary.csv: section[%d]: %w", i, err)
			}
		}
		// blank-line separator between sections (except after last).
		if i < len(sections)-1 {
			if err := w.Write([]string{""}); err != nil {
				return reportsapi.RenderResult{}, fmt.Errorf("project_summary.csv: blank line: %w", err)
			}
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("project_summary.csv: flush: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMECSV, data.Window.From), nil
}
