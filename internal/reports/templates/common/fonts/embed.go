// Package fonts embeds default TTF fonts for the reports module's PDF
// renderer.
//
// DejaVuSans.ttf is bundled (public-domain-equivalent Bitstream Vera Fonts
// License). Cyrillic-capable — required because gopdf falls back to
// ASCII-only Helvetica otherwise and Russian text renders as boxes.
package fonts

import _ "embed"

//go:embed DejaVuSans.ttf
var DejaVuSans []byte
