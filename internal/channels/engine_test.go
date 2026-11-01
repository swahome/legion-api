package channels

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestSplitAnswers(t *testing.T) {
	if got := splitAnswers(""); got != nil {
		t.Fatalf("empty: %v", got)
	}
	if got := splitAnswers(" 1*2*500 "); strings.Join(got, ",") != "1,2,500" {
		t.Fatalf("got %v", got)
	}
}

func TestCommas(t *testing.T) {
	for in, want := range map[string]string{"0": "0.00", "1500": "1,500.00", "1234567.5": "1,234,567.50", "-2500": "-2,500.00", "999": "999.00"} {
		if got := commas(in); got != want {
			t.Errorf("commas(%s) = %s, want %s", in, got, want)
		}
	}
}

// Long lists page on USSD: every screen fits 182 characters and "98"
// reaches every option, numbered as the whole list.
func TestUSSDPaging(t *testing.T) {
	var opts []option
	for i := 1; i <= 30; i++ {
		opts = append(opts, option{label: fmt.Sprintf("Member number %d with a long name", i)})
	}
	v := view{text: "Refund which member?", options: opts}
	c := &convo{format: USSD, st: State{Back: []Frame{{Screen: "main"}}}}
	seen := 0
	for page := 0; page < 30; page++ {
		c.st.Page = page
		out := c.layout(v)
		if n := len([]rune(out)); n > ussdLimit {
			t.Fatalf("page %d is %d characters:\n%s", page, n, out)
		}
		if !strings.HasPrefix(out, "CON Refund which member?\n") || !strings.HasSuffix(out, "0 Back 00 Home") {
			t.Fatalf("page %d layout:\n%s", page, out)
		}
		lines, more := c.page(v)
		for _, l := range lines {
			seen++
			if !strings.HasPrefix(l, fmt.Sprintf("%d. ", seen)) {
				t.Fatalf("numbering: %q, want %d", l, seen)
			}
		}
		if !more {
			if strings.Contains(out, "98. More") {
				t.Fatalf("last page offers more:\n%s", out)
			}
			break
		}
		if !strings.Contains(out, "98. More") {
			t.Fatalf("page %d lacks More:\n%s", page, out)
		}
	}
	if seen != 30 {
		t.Fatalf("reached %d of 30 options", seen)
	}
}

func TestWhatsAppLayout(t *testing.T) {
	var opts []option
	for i := 1; i <= 12; i++ {
		opts = append(opts, option{label: fmt.Sprintf("Item %d", i)})
	}
	c := &convo{format: WhatsApp}
	out := c.layout(view{text: "Pick one:", options: opts})
	if !strings.Contains(out, "Pick one:\n\n1. Item 1") || !strings.Contains(out, "9. Item 9\n98. More") ||
		strings.Contains(out, "10. Item 10") || !strings.HasSuffix(out, "_Reply 0 to exit._") {
		t.Fatalf("layout:\n%s", out)
	}
	c.st.Page = 1
	if out = c.layout(view{text: "Pick one:", options: opts}); !strings.Contains(out, "10. Item 10") || strings.Contains(out, "More") {
		t.Fatalf("page 2:\n%s", out)
	}
}

func TestTwilio(t *testing.T) {
	form := url.Values{"Body": {"1"}, "From": {"whatsapp:+254700000001"}, "MessageSid": {"SM1"}}
	sig := TwilioSignature("tok", "https://legion.test/v1/hooks/whatsapp?key=k", form)
	if !ValidTwilioSignature("tok", "https://legion.test/v1/hooks/whatsapp?key=k", form, sig) {
		t.Fatal("own signature refused")
	}
	form.Set("Body", "2")
	if ValidTwilioSignature("tok", "https://legion.test/v1/hooks/whatsapp?key=k", form, sig) {
		t.Fatal("tampered body accepted")
	}
	if got := string(TwiML("Pay <KES> & go")); !strings.Contains(got, "<Message>Pay &lt;KES&gt; &amp; go</Message>") {
		t.Fatalf("twiml: %s", got)
	}
}
