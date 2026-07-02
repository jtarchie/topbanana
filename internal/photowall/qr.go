package photowall

import (
	"fmt"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// QRCodeSVG renders content as a QR code and returns a self-contained SVG
// document. SVG (not PNG) so the code stays crisp at any size — a photo-wall
// display is often projected large, and the owner may print it. The output has
// no external references, so it satisfies the hosted-site self-containment rule.
//
// One black <path> holds every dark module (M x y h1 v1 h-1 z per module) over
// a white background; shape-rendering=crispEdges keeps the modules sharp when
// scaled. The QR includes its standard quiet-zone border so scanners lock on.
func QRCodeSVG(content string) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", fmt.Errorf("encode qr: %w", err)
	}
	bitmap := q.Bitmap()
	n := len(bitmap)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR code linking to the photo upload page">`, n, n)
	b.WriteString(`<rect width="100%" height="100%" fill="#ffffff"/>`)
	b.WriteString(`<path fill="#000000" d="`)
	for y := range n {
		row := bitmap[y]
		for x := range row {
			if row[x] {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}
