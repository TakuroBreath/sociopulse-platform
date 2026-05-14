package common

import (
	"bytes"
	"encoding/csv"
	"fmt"
)

// NewCSVWriter returns a csv.Writer that emits a UTF-8 BOM as the first
// three bytes — Excel reads CSV correctly only with BOM. Caller MUST call
// Flush before reading buf.Bytes().
func NewCSVWriter(buf *bytes.Buffer) (*csv.Writer, error) {
	if _, err := buf.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return nil, fmt.Errorf("common.NewCSVWriter: write BOM: %w", err)
	}
	return csv.NewWriter(buf), nil
}
