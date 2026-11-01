package payments

import (
	"testing"
	"time"
)

func TestNormAmount(t *testing.T) {
	for in, want := range map[string]string{
		"1000": "1000.00", "1,000.50": "1000.50", " KES 250 ": "250.00", "Ksh. 75.5": "75.50", "-300": "300.00",
	} {
		if got, ok := NormAmount(in); !ok || got != want {
			t.Errorf("NormAmount(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0", "abc", "1.2.3"} {
		if got, ok := NormAmount(in); ok {
			t.Errorf("NormAmount(%q) = %q, want refusal", in, got)
		}
	}
}

func TestParseTime(t *testing.T) {
	nbo := time.FixedZone("EAT", 3*3600)
	want := time.Date(2026, 10, 20, 10, 15, 0, 0, nbo)
	for _, in := range []string{"2026-10-20 10:15:00", "20261020101500", "2026-10-20T10:15:00+03:00", "1792480500"} {
		got, ok := ParseTime(in, nbo)
		if !ok || !got.Equal(want) {
			t.Errorf("ParseTime(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	if got, ok := ParseTime("2026-10-20", nbo); !ok || !got.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, nbo)) {
		t.Errorf("date only: %v %v", got, ok)
	}
	if _, ok := ParseTime("yesterday", nbo); ok {
		t.Error("nonsense parsed")
	}
}
