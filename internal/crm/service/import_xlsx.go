package service

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// parseXLSX reads an XLSX payload via the excelize streaming iterator
// (f.Rows(sheet)) — NOT GetRows, which materialises the whole sheet
// in memory. The first non-empty sheet is treated as the source; the
// header lookup mirrors the CSV path.
//
// Cell type quirks we tolerate:
//   - Excel stores phone numbers entered as 89991234567 as a float.
//     The parser uses GetCellValue which returns the formatted string
//     representation; for plain numeric cells without a custom format,
//     excelize emits scientific notation ("8.9991234567e+10") which
//     would defeat libphonenumber. We normalise float-ish content via
//     parseFloatCell into a digit string before handing it to the
//     downstream phone normaliser.
//   - Date cells that the operator accidentally placed in the phone
//     column come through as serial numbers; libphonenumber rejects
//     them so they fall into the "skipped invalid phone" bucket.
//   - Formula cells: GetCellValue returns the cached value; a
//     freshly-saved file without a formula recalc may have empty
//     cached values. We accept that as "blank cell".
func parseXLSX(r io.Reader) ([]ImportRow, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return nil, fmt.Errorf("xlsx: open reader: %w", err)
	}
	defer func() { _ = f.Close() }()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, errors.New("xlsx: workbook has no sheets")
	}
	iter, err := f.Rows(sheets[0])
	if err != nil {
		return nil, fmt.Errorf("xlsx: open rows iterator: %w", err)
	}
	defer func() { _ = iter.Close() }()

	header, headerLineNo, err := readXLSXHeader(iter)
	if err != nil {
		return nil, err
	}
	colMap, phoneIdx, err := mapHeaders(header)
	if err != nil {
		return nil, err
	}
	return readXLSXBody(iter, colMap, phoneIdx, headerLineNo)
}

// readXLSXHeader walks the iterator past leading blank rows and
// returns the first non-empty row plus its 1-indexed line number.
// Bails out after 5 blank rows so a malformed file doesn't burn the
// whole iterator looking for a header that isn't there.
func readXLSXHeader(iter *excelize.Rows) ([]string, int, error) {
	headerLineNo := 0
	for iter.Next() {
		headerLineNo++
		cols, cerr := iter.Columns()
		if cerr != nil {
			return nil, 0, fmt.Errorf("xlsx: read header columns: %w", cerr)
		}
		if !isBlankCells(cols) {
			return cols, headerLineNo, nil
		}
		if headerLineNo > 5 {
			return nil, 0, errors.New("xlsx: header not found in first 5 rows")
		}
	}
	return nil, 0, errors.New("xlsx: no header row found")
}

// readXLSXBody walks the iterator over data rows and materialises
// ImportRow values. Enforces the importMaxRows cap inside the loop so
// a misbehaving file cannot exhaust memory before validation fires.
func readXLSXBody(iter *excelize.Rows, colMap map[int]string, phoneIdx, headerLineNo int) ([]ImportRow, error) {
	rows := make([]ImportRow, 0, 64)
	lineNo := headerLineNo
	for iter.Next() {
		lineNo++
		cells, cerr := iter.Columns()
		if cerr != nil {
			return nil, fmt.Errorf("xlsx: read row %d: %w", lineNo, cerr)
		}
		if isBlankCells(cells) {
			continue
		}
		if len(rows) >= importMaxRows {
			return nil, fmt.Errorf("xlsx: payload exceeds %d row limit", importMaxRows)
		}
		rows = append(rows, buildXLSXImportRow(cells, colMap, phoneIdx, lineNo))
	}
	if ierr := iter.Error(); ierr != nil {
		return nil, fmt.Errorf("xlsx: iterator: %w", ierr)
	}
	return rows, nil
}

// buildXLSXImportRow mirrors buildImportRow for the CSV path but adds
// excelize-specific cell normalisation: phone cells received as a
// scientific-notation float get rebuilt into a digit string so the
// libphonenumber parser sees "89991234567" not "8.9991234567e+10".
func buildXLSXImportRow(cells []string, colMap map[int]string, phoneIdx, lineNo int) ImportRow {
	row := ImportRow{
		LineNumber: lineNo,
		Attributes: map[string]any{},
	}
	for i, raw := range cells {
		canon, ok := colMap[i]
		if !ok {
			continue
		}
		val := strings.TrimSpace(raw)
		if canon == "phone" {
			val = normalisePhoneCell(val)
		}
		switch canon {
		case "phone":
			row.Phone = val
		case "full_name":
			row.FullName = val
		case "external_ref":
			row.ExternalRef = val
		case "region_code":
			if val != "" {
				row.Attributes["region_code"] = val
			}
		default:
			if val != "" {
				row.Attributes[canon] = val
			}
		}
	}
	if row.Phone == "" && phoneIdx >= 0 && phoneIdx < len(cells) {
		row.Phone = normalisePhoneCell(strings.TrimSpace(cells[phoneIdx]))
	}
	return row
}

// normalisePhoneCell converts excelize cell content into a phone-ish
// string suitable for the downstream libphonenumber parser. Real-world
// flow:
//   - "+79161234567"          → returned as-is (libphonenumber-friendly)
//   - "8 (916) 123-45-67"     → returned as-is (NormalizeRussianPhone strips)
//   - "89161234567"           → returned as-is
//   - "8.9161234567e+10"      → converted back to "89161234567"
//
// Without the float repair, scientific notation defeats parsing because
// libphonenumber rejects 'e' as non-digit content.
func normalisePhoneCell(v string) string {
	if v == "" {
		return ""
	}
	if !strings.ContainsAny(v, "eE") {
		return v
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return v
	}
	// Format with no decimals; phone numbers don't have fractional
	// parts. strconv.FormatFloat with 'f', -1 prec emits scientific
	// notation for big numbers, so we use 'f', 0.
	return strconv.FormatFloat(f, 'f', 0, 64)
}

// isBlankCells reports whether every cell is empty after trimming.
// Mirrors isBlankRow for the XLSX path.
func isBlankCells(cells []string) bool {
	for _, c := range cells {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
