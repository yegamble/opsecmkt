package market

import (
	"net/url"
	"testing"
)

func TestEscrowIntakeOnlyAllowsEscrowOrPaused(t *testing.T) {
	p := newPayEnv(t, newFreshTestApp(t))
	change := func(session, cur, value, password string) int {
		return p.do("POST", "/admin/payment-intake", session, url.Values{"currency": {cur}, "enabled": {value}, "password": {password}}).Code
	}
	for _, cur := range []string{"XMR"} {
		if got := p.str("SELECT value FROM settings WHERE key=$1", paymentIntakeKey(cur)); got != intakeEscrow {
			t.Fatalf("%s default %q", cur, got)
		}
		if code := change(p.buyerSess, cur, intakePaused, testPassword); code != 403 {
			t.Fatalf("buyer changed %s intake: %d", cur, code)
		}
		if code := change(p.adminSess, cur, "true", testPassword); code != 400 {
			t.Fatalf("legacy pooled value accepted: %d", code)
		}
		if code := change(p.adminSess, cur, intakePaused, testPassword); code != 303 {
			t.Fatalf("pause %s: %d", cur, code)
		}
		if got := p.str("SELECT value FROM settings WHERE key=$1", paymentIntakeKey(cur)); got != intakePaused {
			t.Fatalf("%s after pause %q", cur, got)
		}
		if code := change(p.adminSess, cur, intakeEscrow, testPassword); code != 303 {
			t.Fatalf("escrow %s: %d", cur, code)
		}
		if got := p.str("SELECT value FROM settings WHERE key=$1", paymentIntakeKey(cur)); got != intakeEscrow {
			t.Fatalf("%s after enable %q", cur, got)
		}
	}
}
