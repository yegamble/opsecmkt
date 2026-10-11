package market

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Shared helpers for the forms that state an amount (A-161, A-122): resolve, complete, a cancel of a paid order and a
// concession state the counted amount, need a ticked confirmation and carry amount_seen, which the settlement request
// compares with the amount it counts under its row locks (checkPayoutSeen).

// payoutConfirmed adds the confirmation fields those forms carry.
func payoutConfirmed(v url.Values, seen int64) url.Values {
	v.Set("amount_seen", strconv.FormatInt(seen, 10))
	v.Set("payout_confirmed", "confirmed")
	return v
}

// resolveConfirmed is payoutConfirmed plus the resolver's current password, which /resolve needs (A-32).
func resolveConfirmed(v url.Values, seen int64) url.Values {
	v = payoutConfirmed(v, seen)
	v.Set("password", testPassword)
	return v
}

var amountSeenField = regexp.MustCompile(`name="amount_seen" value="(\d+)"`)

// amountSeen returns the amount_seen values rendered on a page, in order.
func amountSeen(t *testing.T, page string) []int64 {
	t.Helper()
	var out []int64
	for _, m := range amountSeenField.FindAllStringSubmatch(page, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestNoTemplateSaysResolvingMovesNoFunds(t *testing.T) {
	files, err := filepath.Glob("web/templates/*/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates %v %v", files, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "moves no funds") {
			t.Errorf("%s still says a money-moving form moves no funds", f)
		}
	}
}
