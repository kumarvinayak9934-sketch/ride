package verification

import (
	"strings"
	"testing"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/config"
)

// §10 decision-rule helpers: masking must never leak full identifiers.
func TestMaskName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"AARAV SHARMA", "A SHARMA"},
		{"PRIYA", "P***"},
		{"  ravi kumar singh ", "R singh"},
		{"", ""},
	}
	for _, c := range cases {
		if got := maskName(c.in); got != c.want {
			t.Errorf("maskName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLast4(t *testing.T) {
	if last4("DL0420260012345") != "2345" {
		t.Fatalf("last4 = %q", last4("DL0420260012345"))
	}
	if last4("1234") != "1234" {
		t.Fatal("short string must round-trip")
	}
	if last4("") != "" {
		t.Fatal("empty string")
	}
}

func TestMaskRef(t *testing.T) {
	got := maskRef("digilocker://dl/DL0420260012345")
	if len(got) > 11 || got[:3] != "***" {
		t.Fatalf("maskRef leaked: %q", got)
	}
	if maskRef("short") != "***" {
		t.Fatal("short ref must be fully masked")
	}
}

// Owner of a raw number must never equal the masked output.
func TestMasksAreLossy(t *testing.T) {
	raw := "DL0420260012345"
	if last4(raw) == raw {
		t.Fatal("last4 leaked the full number")
	}
	if maskName("AARAV SHARMA") == "AARAV SHARMA" {
		t.Fatal("maskName leaked the full name")
	}
}

func TestAuthorizeURLContainsPKCE(t *testing.T) {
	h := &Handler{cfg: config.DigiLockerConfig{
		BaseURL:      "https://api.digilocker.gov.in",
		ClientID:     "cid",
		RedirectURI:  "https://safar.example/cb",
		FaceMatchURL: "",
	}}
	u := h.authorizeURL("state123", "challenge456")
	for _, want := range []string{
		"response_type=code", "client_id=cid", "state=state123",
		"code_challenge=challenge456", "code_challenge_method=S256",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("authorize URL missing %q: %s", want, u)
		}
	}
}

func TestFaceMatchThreshold(t *testing.T) {
	if FaceMatchThreshold != 0.75 {
		t.Fatalf("threshold = %v, spec says 0.75", FaceMatchThreshold)
	}
}
