package mail

import (
	"strings"
	"testing"
	"time"
)

func TestBuild(t *testing.T) {
	b := string(Build("Legion <s@example.com>", Message{To: "a@b.co", Subject: "Fine Statement", Body: "Hi",
		FileName: "x.pdf", File: []byte(strings.Repeat("%PDF", 40))}, time.Unix(0, 0)))
	for _, want := range []string{"To: a@b.co\r\n", "multipart/mixed", `filename="x.pdf"`, "Content-Transfer-Encoding: base64"} {
		if !strings.Contains(b, want) {
			t.Fatalf("missing %q in\n%s", want, b)
		}
	}
	for _, l := range strings.Split(b, "\r\n") {
		if len(l) > 78 {
			t.Fatalf("line too long: %d", len(l))
		}
	}
}

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{"a@b.co": true, "a@b": false, "": false, "a b@c.com": false, "x@y.ke": true} {
		if Valid(s) != want {
			t.Errorf("Valid(%q) = %v", s, !want)
		}
	}
}
