package auth

import (
	"testing"
)

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"9876543210", "+919876543210", true},
		{"+91 98765 43210", "+919876543210", true},
		{"09876543210", "+919876543210", true},
		{"+14155552671", "+14155552671", true},
		{"12345", "", false},      // too short
		{"0000000000", "", false}, // leading zero after + strip -> invalid
		{"", "", false},
		{"abcdefghij", "", false},
	}
	for _, c := range cases {
		got, err := NormalizePhone(c.in)
		if c.ok && err != nil {
			t.Errorf("NormalizePhone(%q) err=%v", c.in, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("NormalizePhone(%q) expected error, got %q", c.in, got)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRandNumeric(t *testing.T) {
	for i := 0; i < 100; i++ {
		s := randNumeric(6)
		if len(s) != 6 {
			t.Fatalf("len = %d", len(s))
		}
		for _, ch := range s {
			if ch < '0' || ch > '9' {
				t.Fatalf("non-digit %q in %q", ch, s)
			}
		}
	}
	// two draws should differ (overwhelmingly likely)
	if randNumeric(12) == randNumeric(12) {
		t.Fatal("random codes collided")
	}
}

func TestMask(t *testing.T) {
	if mask("9876543210") != "987****210" { // first 3 + 4 stars + last 3
		t.Fatalf("mask = %q", mask("9876543210"))
	}
	if mask("12") != "***" {
		t.Fatalf("short mask = %q", mask("12"))
	}
}
