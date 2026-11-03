// Package pdf writes simple text-and-rule PDF documents, enough for the
// statements the mobile app downloads. It uses the standard Helvetica
// fonts, so nothing is embedded and the output stays small.
package pdf

import (
	"bytes"
	"fmt"
	"strings"
)

// A4 in points.
const (
	PageWidth  = 595.0
	PageHeight = 842.0
)

// Doc is a document being built, one page at a time.
type Doc struct {
	pages []*bytes.Buffer
}

func (d *Doc) page() *bytes.Buffer {
	if len(d.pages) == 0 {
		d.AddPage()
	}
	return d.pages[len(d.pages)-1]
}

// AddPage starts a new page; later drawing goes on it.
func (d *Doc) AddPage() { d.pages = append(d.pages, &bytes.Buffer{}) }

// Pages is how many pages the document has.
func (d *Doc) Pages() int { return len(d.pages) }

// Text draws s with its baseline at (x, y), measured from the top left.
func (d *Doc) Text(x, y, size float64, bold bool, s string) {
	font := "F1"
	if bold {
		font = "F2"
	}
	fmt.Fprintf(d.page(), "BT /%s %.1f Tf %.2f %.2f Td (%s) Tj ET\n", font, size, x, PageHeight-y, escape(s))
}

// TextRight draws s ending at x.
func (d *Doc) TextRight(x, y, size float64, bold bool, s string) {
	d.Text(x-Width(s, size, bold), y, size, bold, s)
}

// Line draws a thin rule from (x1, y1) to (x2, y2).
func (d *Doc) Line(x1, y1, x2, y2 float64) {
	fmt.Fprintf(d.page(), "0.5 w %.2f %.2f m %.2f %.2f l S\n", x1, PageHeight-y1, x2, PageHeight-y2)
}

// Width estimates how wide s is in Helvetica at size. Digits, most
// letters and punctuation are close enough to these averages for laying
// out right-aligned columns.
func Width(s string, size float64, bold bool) float64 {
	var w float64
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			w += 556
		case r == ' ' || r == ',' || r == '.' || r == ':' || r == ';' || r == '!' || r == '|' || r == 'i' || r == 'l' || r == 'j' || r == '\'':
			w += 278
		case r == '-' || r == '(' || r == ')' || r == 'f' || r == 't' || r == 'r' || r == 'I':
			w += 333
		case r >= 'A' && r <= 'Z', r == 'm', r == 'w':
			w += 700
		default:
			w += 556
		}
	}
	if bold {
		w *= 1.05
	}
	return w * size / 1000
}

// Fit shortens s with "..." until it is at most width wide.
func Fit(s string, width, size float64, bold bool) string {
	if Width(s, size, bold) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && Width(string(r)+"...", size, bold) > width {
		r = r[:len(r)-1]
	}
	return string(r) + "..."
}

// escape keeps s inside a PDF string: backslashes and parentheses are
// escaped and anything outside printable ASCII becomes "?", since the
// standard fonts only cover WinAnsi.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '(' || r == ')':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 32 && r < 127:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// Bytes is the finished PDF file.
func (d *Doc) Bytes() []byte {
	if len(d.pages) == 0 {
		d.AddPage()
	}
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	out.WriteString("%PDF-1.4\n")
	// 1 catalog, 2 pages, 3 and 4 fonts, then a page and its content
	// stream for each page.
	kids := make([]string, len(d.pages))
	for i := range d.pages {
		kids[i] = fmt.Sprintf("%d 0 R", 5+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(d.pages)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	for i, p := range d.pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.0f %.0f] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>",
			PageWidth, PageHeight, 6+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", p.Len(), p.String()))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}
