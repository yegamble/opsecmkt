package market

import (
	"html"

	"strings"
)

// staffHelp is the help line the order page shows staff who read it (order.html), or "" when it carries none.
func staffHelp(page string) string {
	_, help, ok := strings.Cut(html.UnescapeString(page), "You are viewing this order as ")
	if !ok {
		return ""
	}
	help, _, _ = strings.Cut(help, "</p>")
	return help
}
