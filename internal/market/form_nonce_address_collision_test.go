package market

import (
	"context"
	"net/url"
	"testing"
)

// An old dismissal receipt must not dismiss a new wallet-collision warning.
func TestFormNonceAddressCollisionReplayPreservesNewWarning(t *testing.T) {
	p := newPayEnv(t)
	p.A.recordAddressCollision(context.Background(), "BTC", "regtest")
	nonce := renderedFormNonce(t, p.page("/admin", p.adminSess), "/admin/payment-address-collisions")
	if got := p.count("SELECT count(*) FROM form_submissions WHERE action_path='/admin/payment-address-collisions'"); got != 0 {
		t.Fatalf("GET wrote %d receipts", got)
	}
	f := url.Values{"currency": {"BTC"}, "seen": {"1"}, formNonceField: {nonce}}
	first := p.do("POST", "/admin/payment-address-collisions", p.adminSess, f)
	p.check(first, 303)
	if got := p.count("SELECT address_collisions FROM payment_status WHERE currency='BTC'"); got != 0 {
		t.Fatalf("first dismissal kept %d collisions", got)
	}
	p.A.recordAddressCollision(context.Background(), "BTC", "regtest")
	replay := p.do("POST", "/admin/payment-address-collisions", p.adminSess, f)
	p.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatal("replay changed the saved redirect")
	}
	if got := p.count("SELECT address_collisions FROM payment_status WHERE currency='BTC'"); got != 1 {
		t.Fatalf("old receipt dismissed a new warning: %d", got)
	}
	if got := p.count("SELECT count(*) FROM audit_events WHERE action='Dismissed the BTC issued-address warning (1 payment request refused)'"); got != 1 {
		t.Fatalf("replay wrote %d dismissal audits", got)
	}
	changed := url.Values{"currency": {"XMR"}, "seen": {"1"}, formNonceField: {nonce}}
	p.check(p.do("POST", "/admin/payment-address-collisions", p.adminSess, changed), 409)
	p.check(p.do("POST", "/admin/payment-address-collisions", agSession(p.testEnv, p.str("SELECT user_id FROM sessions WHERE token_hash=$1", digest(p.adminSess))), f), 409)
	p.check(p.do("POST", "/admin/payment-address-collisions", p.buyerSess, f), 403)
	fresh := url.Values{"currency": {"BTC"}, "seen": {"1"}, formNonceField: {randomToken()[:32]}}
	p.check(p.do("POST", "/admin/payment-address-collisions", p.adminSess, fresh), 303)
	if got := p.count("SELECT address_collisions FROM payment_status WHERE currency='BTC'"); got != 0 {
		t.Fatalf("fresh form did not dismiss current warning: %d", got)
	}
}
