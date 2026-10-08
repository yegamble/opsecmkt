package market

import (
	"strings"
	"unicode/utf8"
)

// Normalize text after receipt matching, leaving the original submitted form available for its digest.
func normalizedFormText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

func textLength(s string, minChars, maxChars int) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0) && n >= minChars && n <= maxChars
}
