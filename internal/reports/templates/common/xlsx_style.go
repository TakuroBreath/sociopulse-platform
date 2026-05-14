// Package common holds shared helpers for the per-kind renderers.
package common

import "github.com/xuri/excelize/v2"

// HeaderStyle returns a bold + dark-background style id suitable for the
// first row of a renderer's primary sheet. Cache the returned id at
// renderer level; recreating it for every cell is wasteful.
func HeaderStyle(f *excelize.File) (int, error) {
	return f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"374151"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
	})
}

// DateStyle returns the builtin "m/d/yy h:mm" format (NumFmt 22) — portable
// date rendering across Excel/Numbers/LibreOffice. Avoids the default
// "Excel-serial float" cell that doesn't render in non-Excel readers.
func DateStyle(f *excelize.File) (int, error) {
	numFmt := 22
	return f.NewStyle(&excelize.Style{NumFmt: numFmt})
}
