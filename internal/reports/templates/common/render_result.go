package common

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
)

// MIME constants for the three supported export formats. Used by every
// per-kind renderer when building RenderResult; centralised here so a
// future MIME tweak (e.g. xlsx → application/octet-stream fallback) lands
// in one place.
const (
	MIMEXlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MIMECSV  = "text/csv; charset=utf-8"
	MIMEPDF  = "application/pdf"
)

// PDFRowLimit is the maximum detail-row count a per-kind PDF renderer
// accepts before returning reportsapi.ErrTooLarge. The cap exists to
// keep single-artifact PDFs render-able within a worker's wall-clock
// budget (>5000 rows × 18pt/row ≈ 150 pages, ~1.5 MB). Variable-row
// kinds enforce this; the finance renderer (fixed 5 rows) ignores it.
const PDFRowLimit = 5000

// Extension maps an export format to its file extension (without the
// dot). Used by NewRenderResult below.
func Extension(mime string) string {
	switch mime {
	case MIMEXlsx:
		return "xlsx"
	case MIMECSV:
		return "csv"
	case MIMEPDF:
		return "pdf"
	default:
		return ""
	}
}

// NewRenderResult assembles a reportsapi.RenderResult from the rendered
// payload, the kind slug, the MIME type, and the window-start day used in
// the filename. The caller passes window.From; we format it as YYYYMMDD
// in UTC for the filename.
//
// SHA-256 is hex-encoded for cheap log/audit comparison; consumers that
// need the raw 32-byte digest can decode it via hex.DecodeString.
func NewRenderResult(payload []byte, kind, mime string, windowFrom time.Time) reportsapi.RenderResult {
	sum := sha256.Sum256(payload)
	return reportsapi.RenderResult{
		Bytes:    payload,
		Filename: fmt.Sprintf("%s_%s.%s", kind, windowFrom.UTC().Format("20060102"), Extension(mime)),
		MIME:     mime,
		SHA256:   hex.EncodeToString(sum[:]),
	}
}
