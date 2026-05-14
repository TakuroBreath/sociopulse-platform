package common_test

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/reports/templates/common"
)

func TestNewRenderResult_AssemblesContract(t *testing.T) {
	t.Parallel()
	payload := []byte("hello-payload")
	win := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	res := common.NewRenderResult(payload, "operator_efficiency", common.MIMEXlsx, win)
	require.Equal(t, payload, res.Bytes)
	require.Equal(t, "operator_efficiency_20260514.xlsx", res.Filename)
	require.Equal(t, common.MIMEXlsx, res.MIME)
	require.Len(t, res.SHA256, 64)
	// The hex string must decode to a 32-byte sha256 digest.
	raw, err := hex.DecodeString(res.SHA256)
	require.NoError(t, err)
	require.Len(t, raw, 32)
}

func TestExtension_AllThreeMIMEs(t *testing.T) {
	t.Parallel()
	require.Equal(t, "xlsx", common.Extension(common.MIMEXlsx))
	require.Equal(t, "csv", common.Extension(common.MIMECSV))
	require.Equal(t, "pdf", common.Extension(common.MIMEPDF))
	require.Equal(t, "", common.Extension("application/octet-stream"))
}

func TestNewRenderResult_NormalisesToUTC(t *testing.T) {
	t.Parallel()
	// Window in MSK (UTC+3); start at 01:00 MSK = 22:00 UTC previous day.
	msk := time.FixedZone("MSK", 3*60*60)
	win := time.Date(2026, 5, 14, 1, 0, 0, 0, msk)

	res := common.NewRenderResult([]byte("p"), "calls_by_status", common.MIMECSV, win)
	// 22:00 UTC on 13 May → filename uses 20260513, NOT 20260514.
	require.Equal(t, "calls_by_status_20260513.csv", res.Filename)
}
