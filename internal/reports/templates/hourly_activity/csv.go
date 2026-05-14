package hourly_activity //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/internal/reports/templates/common"
)

// RenderCSV emits a header row + N bucket rows. Hour is RFC3339-formatted
// so non-Excel readers display the timestamp correctly.
func RenderCSV(data service.HourlyActivityData) (reportsapi.RenderResult, error) {
	buf := &bytes.Buffer{}
	w, err := common.NewCSVWriter(buf)
	if err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.csv: NewCSVWriter: %w", err)
	}
	if err := w.Write([]string{"Hour", "Count", "AvgDurSec"}); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.csv: header: %w", err)
	}
	for _, b := range data.Buckets {
		if err := w.Write([]string{
			b.Hour.UTC().Format(time.RFC3339),
			strconv.FormatUint(b.Count, 10),
			strconv.FormatFloat(b.AvgDurSec, 'f', 2, 64),
		}); err != nil {
			return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.csv: row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return reportsapi.RenderResult{}, fmt.Errorf("hourly_activity.csv: flush: %w", err)
	}
	return common.NewRenderResult(buf.Bytes(), kind, common.MIMECSV, data.Window.From), nil
}
