package channels

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"net/url"
	"sort"
	"strings"
)

// TwilioSignature is the X-Twilio-Signature Twilio sends with a webhook:
// base64(HMAC-SHA1(auth token, full URL + each POST field name and value,
// sorted by name)).
func TwilioSignature(authToken, fullURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(fullURL)
	for _, k := range keys {
		for _, v := range form[k] {
			b.WriteString(k)
			b.WriteString(v)
		}
	}
	m := hmac.New(sha1.New, []byte(authToken))
	m.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// ValidTwilioSignature checks a webhook's signature.
func ValidTwilioSignature(authToken, fullURL string, form url.Values, signature string) bool {
	want := TwilioSignature(authToken, fullURL, form)
	return hmac.Equal([]byte(want), []byte(signature))
}

// TwiML wraps a reply for Twilio, which sends it back to the caller.
func TwiML(text string) []byte {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<Response><Message>")
	_ = xml.EscapeText(&b, []byte(text))
	b.WriteString("</Message></Response>")
	return []byte(b.String())
}
