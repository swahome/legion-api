package auth

import "testing"

func TestParseIdentity(t *testing.T) {
	tests := []struct {
		in    string
		phone string
		email string
		err   bool
	}{
		{in: "+254712345678", phone: "+254712345678"},
		{in: "254712345678", phone: "+254712345678"},
		{in: "0712345678", phone: "+254712345678"},
		{in: "712345678", phone: "+254712345678"},
		{in: "0712 345-678", phone: "+254712345678"},
		{in: "00447911123456", phone: "+447911123456"},
		{in: "+44 7911 123456", phone: "+447911123456"},
		{in: "Jane.Doe@Example.com", email: "jane.doe@example.com"},
		{in: "  jane@example.com ", email: "jane@example.com"},
		{in: "", err: true},
		{in: "12", err: true},
		{in: "07123abc", err: true},
		{in: "@example.com", err: true},
		{in: "jane@", err: true},
	}
	for _, tt := range tests {
		got, err := ParseIdentity(tt.in, "254")
		if tt.err {
			if err == nil {
				t.Errorf("ParseIdentity(%q) = %+v, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseIdentity(%q) error: %v", tt.in, err)
			continue
		}
		if got.Phone != tt.phone || got.Email != tt.email {
			t.Errorf("ParseIdentity(%q) = %+v, want phone %q email %q", tt.in, got, tt.phone, tt.email)
		}
	}
}
