package market

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestCheckSetupTokenRefusesPlaceholdersAndLowVariety(t *testing.T) {
	for name, token := range map[string]string{
		"short":                     "0123456789abcdef0123456789abcde",
		"alpha.1 .env.example":      "REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS",
		"lower-case placeholder":    "replace-me-with-a-long-random-value-please",
		"embedded placeholder":      "f3a9c1d7e5b2" + "REPLACE" + "0a8e6c4f2d9b7a5e3c1f0d8b6",
		"changeme":                  "changeme-changeme-0123456789abcdefghijkl",
		"placeholder word":          "this-is-a-placeholder-value-1234567890ab",
		"one character":             strings.Repeat("a", 64),
		"old test fixture":          strings.Repeat("test", 16),
		"seven distinct characters": strings.Repeat("abcdefg", 6),
		"repeated digits":           strings.Repeat("0123456789", 4),
		"repeated hex half":         strings.Repeat("0123456789abcdef", 2),
		"repeated unit, partial":    strings.Repeat("abcdefghij", 3) + "abcde",
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkSetupToken(token); err == nil {
				t.Fatalf("checkSetupToken(%q) accepted a weak token", token)
			} else if !strings.Contains(err.Error(), "openssl rand -hex 32") {
				t.Fatalf("error does not say how to fix it: %v", err)
			}
		})
	}
}

func TestCheckSetupTokenAcceptsGeneratedAndFixtureTokens(t *testing.T) {
	accepted := []string{
		testSetupToken,
		// Fixed tokens used by CI, operations scripts and Playwright configurations.
		"ops-restore-test-bootstrap-token-0123456789",
		"ci-container-bootstrap-token-only-123456789",
		"upgrade-test-bootstrap-token-only-0123456789",
		"e2e-local-only-setup-token-at-least-32-characters",
		"e2e-wallet-local-setup-token-at-least-32-characters",
		"local-chain-browser-test-setup-token-at-least-32",
		// A long passphrase is not random hex but has plenty of variety.
		"correct horse battery staple and a long passphrase",
	}
	// scripts/install.sh and README.md use `openssl rand -hex 32`: 32 random bytes as 64 hex characters.
	// Shorter hex and URL-safe base64 tokens of at least 32 characters are accepted too.
	for i := 0; i < 20000; i++ {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		accepted = append(accepted, hex.EncodeToString(b))
		if i < 2000 {
			accepted = append(accepted, hex.EncodeToString(b[:16]), base64.RawURLEncoding.EncodeToString(b[:24]))
		}
	}
	for _, token := range accepted {
		if err := checkSetupToken(token); err != nil {
			t.Fatalf("checkSetupToken(%q) = %v", token, err)
		}
	}
}

// The shipped example must never be usable as-is: either blank (Compose refuses to start) or refused here.
func TestEnvExampleSetupTokenIsNotUsable(t *testing.T) {
	f, err := os.Open(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	found := false
	for s := bufio.NewScanner(f); s.Scan(); {
		value, ok := strings.CutPrefix(s.Text(), "SETUP_TOKEN=")
		if !ok {
			continue
		}
		found = true
		if value = strings.Trim(value, `'"`); value != "" && checkSetupToken(value) == nil {
			t.Fatalf(".env.example SETUP_TOKEN %q would be accepted at startup", value)
		}
	}
	if !found {
		t.Fatal(".env.example has no SETUP_TOKEN line")
	}
}

// New refuses the placeholder before it derives any key or opens the database.
func TestNewRefusesPlaceholderSetupToken(t *testing.T) {
	t.Setenv("APP_MODE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("SETUP_TOKEN", "REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS")
	a, err := New(context.Background(), false)
	if err == nil || a != nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("New = %v, %v; want a placeholder refusal", a, err)
	}
}
