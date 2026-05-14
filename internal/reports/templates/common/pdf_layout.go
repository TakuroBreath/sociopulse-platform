package common

import (
	"fmt"

	"github.com/signintech/gopdf"

	"github.com/sociopulse/platform/internal/reports/templates/common/fonts"
)

// PDFInit constructs a configured gopdf.GoPdf with DejaVuSans loaded under
// the "default" alias and the first page added. Callers MUST call
// pdf.Close() in a defer; gopdf manages its lifetime through Output / Write.
func PDFInit() (*gopdf.GoPdf, error) {
	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	if err := pdf.AddTTFFontData("default", fonts.DejaVuSans); err != nil {
		return nil, fmt.Errorf("common.PDFInit: AddTTFFontData: %w", err)
	}
	if err := pdf.SetFont("default", "", 12); err != nil {
		return nil, fmt.Errorf("common.PDFInit: SetFont: %w", err)
	}
	pdf.AddPage()
	return pdf, nil
}

// PDFHeader writes a centered title at the top of the current page using
// a larger font, then restores the body font.
func PDFHeader(pdf *gopdf.GoPdf, title string) error {
	pdf.SetXY(40, 30)
	if err := pdf.SetFont("default", "", 18); err != nil {
		return fmt.Errorf("common.PDFHeader: SetFont: %w", err)
	}
	if err := pdf.Cell(nil, title); err != nil {
		return fmt.Errorf("common.PDFHeader: Cell: %w", err)
	}
	if err := pdf.SetFont("default", "", 12); err != nil {
		return fmt.Errorf("common.PDFHeader: restore SetFont: %w", err)
	}
	return nil
}

// PDFRow writes one row of N cells starting at y. Returns the y for the
// next row.
func PDFRow(pdf *gopdf.GoPdf, y float64, cells []string, widths []float64) (float64, error) {
	if len(cells) != len(widths) {
		return 0, fmt.Errorf("common.PDFRow: cells (%d) and widths (%d) length mismatch", len(cells), len(widths))
	}
	x := 40.0
	for i, c := range cells {
		pdf.SetXY(x, y)
		if err := pdf.Cell(nil, c); err != nil {
			return 0, fmt.Errorf("common.PDFRow: Cell[%d]: %w", i, err)
		}
		x += widths[i]
	}
	return y + 18, nil
}
