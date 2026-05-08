package service

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// utf8BOM is the byte sequence Excel-derived CSVs typically start with;
// we strip it before handing the stream to encoding/csv. Otherwise the
// first header column comes through as "<BOM>phone" and the
// case-insensitive header lookup misses.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// canonicalHeaderAliases maps a normalised header label to the
// canonical column name our parser writes into the ImportRow. Allows
// the parser to accept common Russian variants ("Телефон", "ФИО")
// alongside English headers without forcing operators to rename
// columns by hand.
//
// Lookup is case-insensitive on the input; the keys here are already
// lowercased.
var canonicalHeaderAliases = map[string]string{
	"phone":        "phone",
	"телефон":      "phone",
	"тел":          "phone",
	"phone_number": "phone",
	"phonenumber":  "phone",
	"full_name":    "full_name",
	"fullname":     "full_name",
	"name":         "full_name",
	"имя":          "full_name",
	"фио":          "full_name",
	"external_ref": "external_ref",
	"externalref":  "external_ref",
	"id":           "external_ref",
	"внешний_id":   "external_ref",
	"region_code":  "region_code",
	"regioncode":   "region_code",
	"регион":       "region_code",
}

// parseCSV streams a CSV file and returns the parsed ImportRows. The
// function eagerly materialises the rows so the caller can take a
// length count for the 100k cap and slice the result into batches.
// For 100k rows the slice fits comfortably in memory (≈ 30 MB).
//
// Rules:
//   - First row is the header. Header lookup is case-insensitive and
//     accepts a handful of aliases (see canonicalHeaderAliases).
//   - The "phone" column is mandatory; missing it returns an error.
//   - Any unmapped column lands in ImportRow.Attributes under its
//     normalised header name (lowercased, stripped of surrounding
//     whitespace).
//   - BOM is stripped, CRLF is handled by encoding/csv automatically.
//   - The cap of importMaxRows is enforced inside the loop — exceeding
//     it returns an error so a misbehaving caller cannot exhaust
//     memory before validation fires.
func parseCSV(r io.Reader) ([]ImportRow, error) {
	br := bufio.NewReader(r)
	if peek, err := br.Peek(len(utf8BOM)); err == nil && hasBOMPrefix(peek) {
		_, _ = br.Discard(len(utf8BOM))
	}

	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1 // tolerant: rows may have trailing empty cells
	cr.LazyQuotes = true
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("csv: empty file")
		}
		return nil, fmt.Errorf("csv: read header: %w", err)
	}
	colMap, phoneIdx, err := mapHeaders(header)
	if err != nil {
		return nil, err
	}

	rows := make([]ImportRow, 0, 64)
	lineNo := 1
	for {
		lineNo++
		rec, rerr := cr.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("csv: read row %d: %w", lineNo, rerr)
		}
		if isBlankRow(rec) {
			continue
		}
		if len(rows) >= importMaxRows {
			return nil, fmt.Errorf("csv: payload exceeds %d row limit", importMaxRows)
		}
		row := buildImportRow(rec, colMap, phoneIdx, lineNo)
		rows = append(rows, row)
	}
	return rows, nil
}

// mapHeaders normalises the supplied header row, applies the
// canonicalHeaderAliases lookup, and returns the column→canonical map
// plus the index of the phone column. Headers that don't map to a
// canonical name are recorded under their normalised form so the
// caller can stash them in ImportRow.Attributes.
//
// Returns an error when no phone column is found.
func mapHeaders(header []string) (map[int]string, int, error) {
	colMap := make(map[int]string, len(header))
	phoneIdx := -1
	for i, h := range header {
		norm := strings.ToLower(strings.TrimSpace(h))
		if norm == "" {
			continue
		}
		canon, ok := canonicalHeaderAliases[norm]
		if !ok {
			canon = norm
		}
		colMap[i] = canon
		if canon == "phone" {
			phoneIdx = i
		}
	}
	if phoneIdx < 0 {
		return nil, -1, errors.New("csv: no phone column found in header")
	}
	return colMap, phoneIdx, nil
}

// buildImportRow constructs a single ImportRow from one CSV record.
// Phone, full_name, external_ref, region_code are pulled into typed
// fields; everything else is stuffed into Attributes verbatim.
func buildImportRow(rec []string, colMap map[int]string, phoneIdx, lineNo int) ImportRow {
	row := ImportRow{
		LineNumber: lineNo,
		Attributes: map[string]any{},
	}
	for i, cell := range rec {
		canon, ok := colMap[i]
		if !ok {
			continue
		}
		val := strings.TrimSpace(cell)
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
	if row.Phone == "" && phoneIdx >= 0 && phoneIdx < len(rec) {
		row.Phone = strings.TrimSpace(rec[phoneIdx])
	}
	return row
}

// hasBOMPrefix returns true when b starts with the UTF-8 BOM.
func hasBOMPrefix(b []byte) bool {
	if len(b) < len(utf8BOM) {
		return false
	}
	for i := range utf8BOM {
		if b[i] != utf8BOM[i] {
			return false
		}
	}
	return true
}

// isBlankRow reports whether every cell in rec is empty (after
// trimming). encoding/csv emits zero-length records for "...,," and
// for trailing newlines; we skip those so the line counter stays
// accurate even with sloppy spreadsheet exports.
func isBlankRow(rec []string) bool {
	for _, cell := range rec {
		if strings.TrimSpace(cell) != "" {
			return false
		}
	}
	return true
}

// utf8RuneCount is unused at runtime; kept here as a sanity-touch on
// the unicode/utf8 import so go-imports tooling does not eagerly drop
// it when future edits introduce its companion (Russian header
// validation will use it).
var _ = utf8.RuneCountInString
