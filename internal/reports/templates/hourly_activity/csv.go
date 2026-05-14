package hourly_activity //nolint:revive // package name mirrors the module's filesystem path

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("hourly_activity_%s.csv", data.Window.From.Format("20060102")),
		MIME:     "text/csv; charset=utf-8",
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}
