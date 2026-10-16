package brandingconfig

import (
	"context"
	"strings"
	"testing"

	"opsecmkt/internal/onionidentity"
)

func TestParseOriginNormalizesReviewedFixedOrigins(t *testing.T) {
	id, err := onionidentity.Generate(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		raw, want   string
		development bool
	}{
		{"", "", false},
		{"https://UPLOAD.MARKET.EXAMPLE:443", "https://upload.market.example", false},
		{"HTTPS://upload.market.example:8443", "https://upload.market.example:8443", false},
		{"https://[2001:0db8:0:0::1]:443", "https://[2001:db8::1]", false},
		{"http://" + id.Address + ":80", "http://" + id.Address, false},
		{"http://" + strings.ToUpper(id.Address), "http://" + id.Address, false},
		{"http://localhost:8080", "http://localhost:8080", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", true},
		{"http://[::1]:8080", "http://[::1]:8080", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ParseOrigin(tc.raw, tc.development)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestParseOriginRefusesAmbiguousOrUnsafeOriginsWithoutEchoingInput(t *testing.T) {
	for _, raw := range []string{
		"https://operator:sensitive-password@market.example", "https://market.example/", "https://market.example/logo",
		"https://market.example/%2fadmin", "https://market.example?secret=private", "https://market.example?",
		"https://market.example#private", "https://market.example#", "https://market.example:", "https://market.example:0",
		"https://market.example:65536", "https://market.example:bad", "https://market.example:443:443",
		"https://market.example\\@other.example", " https://market.example", "https://market.example\n",
		"//market.example", "https:market.example", "ftp://market.example", "http://market.example",
		"http://10.20.0.1", "http://localhost", "http://127.0.0.1", "http://[::1]",
		"http://example.onion", "http://" + strings.Repeat("a", 56) + ".onion", "https://market.example.",
		"https://market_example", "https://märket.example", "https://2130706433", "https://127.1", "https://0x7f000001",
		"https://[market.example]", "https://[127.0.0.1]", "https://[fe80::1%25en0]",
	} {
		t.Run(raw, func(t *testing.T) {
			got, err := ParseOrigin(raw, false)
			if err == nil || got != "" {
				t.Fatalf("accepted forbidden origin: %q", raw)
			}
			if err.Error() != originError {
				t.Fatal("validation error must not reveal configured input")
			}
		})
	}
	for _, raw := range []string{"http://market.example", "http://10.20.0.1", "http://127.1", "http://127.0.0.1/logo"} {
		if _, err := ParseOrigin(raw, true); err == nil {
			t.Fatalf("development accepted non-loopback or non-origin %q", raw)
		}
	}
}
