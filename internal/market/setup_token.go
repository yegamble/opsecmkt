package market

import (
	"errors"
	"strings"
)

// minSetupTokenVariety is the fewest distinct characters a SETUP_TOKEN may contain. The installer writes
// 64 hex characters (openssl rand -hex 32); fewer than 8 distinct ones among them has a probability of about
// 1e-19, and about 4e-8 for a 32-character hex token.
const minSetupTokenVariety = 8

// placeholderMarkers are words that only appear in example or template values, never in generated secrets.
var placeholderMarkers = []string{"replace", "changeme", "change_me", "placeholder"}

// checkSetupToken refuses SETUP_TOKEN values that are too short, copied from .env.example, or obviously not
// random. The token authorizes /setup and derives the CSRF/CAPTCHA key and the key sealing TOTP secrets and
// recovery-code reveals, so a guessable value exposes all of them.
func checkSetupToken(token string) error {
	const fix = "; generate one with `openssl rand -hex 32` (scripts/install.sh does this) and see UPGRADING.md before changing it on an existing instance"
	if len(token) < 32 {
		return errors.New("SETUP_TOKEN must contain at least 32 random characters" + fix)
	}
	lower := strings.ToLower(token)
	for _, marker := range placeholderMarkers {
		if strings.Contains(lower, marker) {
			return errors.New("SETUP_TOKEN is still a placeholder value (for example the one in .env.example)" + fix)
		}
	}
	seen := map[rune]bool{}
	for _, r := range token {
		seen[r] = true
	}
	if len(seen) < minSetupTokenVariety {
		return errors.New("SETUP_TOKEN is not random: it uses fewer than 8 distinct characters" + fix)
	}
	// A value made of two or more copies of a shorter unit ("0123456789" four times) is not random either.
	for period := 1; period <= len(token)/2; period++ {
		if strings.Repeat(token[:period], len(token)/period+1)[:len(token)] == token {
			return errors.New("SETUP_TOKEN is not random: it repeats a short pattern" + fix)
		}
	}
	return nil
}
