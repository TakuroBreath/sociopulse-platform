package common_test

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"

	"github.com/sociopulse/platform/internal/reports/templates/common"
)

func TestNewCSVWriter_EmitsUTF8BOM(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	w, err := common.NewCSVWriter(buf)
	require.NoError(t, err)
	require.NoError(t, w.Write([]string{"col"}))
	w.Flush()
	require.NoError(t, w.Error())
	require.True(t, bytes.HasPrefix(buf.Bytes(), []byte{0xEF, 0xBB, 0xBF}))

	// Round-trip read via csv.Reader (BOM must not confuse parsing).
	r := csv.NewReader(strings.NewReader(buf.String()))
	rec, err := r.Read()
	require.NoError(t, err)
	require.Len(t, rec, 1)
	// The first cell may carry the BOM as a prefix — csv.Reader does not
	// strip it. Tolerate that — but the column count is the contract.
}

func TestHeaderStyle_ReturnsValidStyleID(t *testing.T) {
	t.Parallel()
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	id, err := common.HeaderStyle(f)
	require.NoError(t, err)
	require.Positive(t, id, "HeaderStyle returns a valid (>0) style id")
}

func TestDateStyle_ReturnsValidStyleID(t *testing.T) {
	t.Parallel()
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	id, err := common.DateStyle(f)
	require.NoError(t, err)
	require.Positive(t, id)
}

func TestPDFInit_ProducesValidPDFShape(t *testing.T) {
	t.Parallel()
	pdf, err := common.PDFInit()
	require.NoError(t, err)
	defer pdf.Close()
	buf := &bytes.Buffer{}
	_, err = pdf.WriteTo(buf)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(buf.Bytes(), []byte("%PDF-")), "expect PDF magic")
}
