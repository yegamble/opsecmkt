package market

import (
	"regexp"
)

var flaggedAt = regexp.MustCompile(`^<tr class="payment-review-row">\s*<td>\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC</td>`)
