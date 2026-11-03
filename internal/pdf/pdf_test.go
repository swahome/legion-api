package pdf

import (
	"bytes"
	"strings"
	"testing"
)

func TestDocument(t *testing.T) {
	var d Doc
	d.Text(40, 40, 14, true, "Statement (test) \\ ok")
	d.Line(40, 50, 555, 50)
	d.AddPage()
	d.TextRight(555, 80, 9, false, "1,250.00")
	b := d.Bytes()
	if !bytes.HasPrefix(b, []byte("%PDF-1.4")) || !bytes.HasSuffix(b, []byte("%%EOF\n")) {
		t.Fatalf("not a PDF: %q", b[:20])
	}
	if !strings.Contains(string(b), `(Statement \(test\) \\ ok) Tj`) {
		t.Fatal("text not escaped")
	}
	if !strings.Contains(string(b), "/Count 2") {
		t.Fatal("want two pages")
	}
	// Each xref offset points at its object.
	s := string(b)
	i := strings.Index(s, "xref\n")
	lines := strings.Split(s[i:], "\n")[3:9]
	for n, l := range lines {
		var off int
		for _, c := range l[:10] {
			off = off*10 + int(c-'0')
		}
		want := strings.TrimSpace(strings.Split(s[off:], " ")[0])
		if want != itoa(n+1) {
			t.Fatalf("xref %d points at %q", n+1, s[off:off+10])
		}
	}
	if got := Fit("A very long description indeed", 60, 9, false); !strings.HasSuffix(got, "...") || Width(got, 9, false) > 60 {
		t.Fatalf("Fit = %q", got)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
