package market

import (
	"net/url"
	"strings"
	"testing"
)

func escrowFeeNonceForm(bps, address, nonce string) url.Values {
	return url.Values{
		"fee_bps": {bps}, "fee_address": {address}, "password": {testPassword}, formNonceField: {nonce},
	}
}

type escrowFeeNonceEffects struct{ receipts, audits, settings int }

func escrowFeeEffects(x *escrowEnv, adminID string) escrowFeeNonceEffects {
	return escrowFeeNonceEffects{
		agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/admin/escrow-fee'"),
		agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Changed the Bitcoin escrow market fee%'", adminID),
		agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key IN ($1,$2)", escrowFeeBpsSetting, escrowFeeAddrSetting),
	}
}

func TestEscrowFeeConfirmedNonceReplayAfterNewerFeeAndProviderOutage(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("fee_nonce_admin", "admin")
	page := x.page("/admin", admin)
	nonce := renderedFormNonce(t, page, "/admin/escrow-fee")
	if got := escrowFeeEffects(x, adminID); got != (escrowFeeNonceEffects{}) {
		t.Fatalf("GET wrote fee state: %+v", got)
	}
	address1, address2 := regtestAddr("fee-nonce-first"), regtestAddr("fee-nonce-second")
	f1 := escrowFeeNonceForm("250", address1, nonce)
	first := x.do("POST", "/admin/escrow-fee", admin, f1)
	x.check(first, 303)
	if first.Header().Get("Location") != "/admin#escrow-fee" {
		t.Fatalf("fee redirect %q", first.Header().Get("Location"))
	}
	if got := escrowFeeEffects(x, adminID); got != (escrowFeeNonceEffects{1, 1, 2}) {
		t.Fatalf("first fee effects: %+v", got)
	}
	f2 := escrowFeeNonceForm("500", address2, randomToken()[:32])
	x.check(x.do("POST", "/admin/escrow-fee", admin, f2), 303)
	committed := escrowFeeEffects(x, adminID)
	if committed != (escrowFeeNonceEffects{2, 2, 2}) {
		t.Fatalf("second fee effects: %+v", committed)
	}
	provider := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	rpcBefore := len(x.core.methods())
	replay := x.do("POST", "/admin/escrow-fee", admin, f1)
	x.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") || escrowFeeEffects(x, adminID) != committed || len(x.core.methods()) != rpcBefore {
		t.Fatal("old fee receipt changed the newer fee, wrote an effect, called Core, or changed its redirect")
	}
	if bps := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowFeeBpsSetting); len(bps) != 1 || bps[0] != "500" {
		t.Fatalf("replayed old fee replaced newer basis points: %q", bps)
	}
	if address := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", escrowFeeAddrSetting); len(address) != 1 || address[0] != address2 {
		t.Fatalf("replayed old fee replaced newer destination: %q", address)
	}
	changed := escrowFeeNonceForm("500", address1, nonce)
	x.check(x.do("POST", "/admin/escrow-fee", admin, changed), 409)
	x.check(x.do("POST", "/admin/escrow-fee", agSession(x.testEnv, adminID), f1), 409)
	f1.Set("password", "wrong password")
	x.check(x.do("POST", "/admin/escrow-fee", admin, f1), 401)
	if escrowFeeEffects(x, adminID) != committed || len(x.core.methods()) != rpcBefore {
		t.Fatal("changed/session/password refusals changed the fee or called Core")
	}
	x.A.escrow.Store(provider)
	var receipt string
	if err := x.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, admin, address1, address2} {
		if strings.Contains(receipt, secret) {
			t.Fatal("fee receipt retained raw password, session, or destination")
		}
	}
}

func TestEscrowFeeConfirmedNonceRefusalsRollbackAndTOTP(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("fee_nonce_refuse", "admin")
	key, _ := agEnableTOTP(x.testEnv, adminID)
	address := regtestAddr("fee-nonce-refuse")
	f := escrowFeeNonceForm("300", address, randomToken()[:32])
	f.Set("code", agTOTPNow(key))
	provider := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	x.check(x.do("POST", "/admin/escrow-fee", admin, f), 409)
	if got := escrowFeeEffects(x, adminID); got != (escrowFeeNonceEffects{}) {
		t.Fatalf("unavailable provider kept fee claim/effect: %+v", got)
	}
	x.A.escrow.Store(provider)
	x.check(x.do("POST", "/admin/escrow-fee", admin, f), 303)
	step := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", adminID)
	x.check(x.do("POST", "/admin/escrow-fee", admin, f), 303)
	if agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", adminID) != step {
		t.Fatal("fee replay spent authenticator code twice")
	}
	if got := escrowFeeEffects(x, adminID); got != (escrowFeeNonceEffects{1, 1, 2}) {
		t.Fatalf("retry/replay fee effects: %+v", got)
	}
	f.Set(formNonceField, randomToken()[:32])
	x.check(x.do("POST", "/admin/escrow-fee", admin, f), 401)
	if got := escrowFeeEffects(x, adminID); got != (escrowFeeNonceEffects{1, 1, 2}) {
		t.Fatalf("fresh nonce reused TOTP: %+v", got)
	}
}

func TestEscrowFeeConfirmedNonceFreshStaleAndUsedAddressGuards(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("fee_nonce_guard", "admin")
	used := regtestAddr("fee-nonce-used-refund")
	x.mustCheckout(x.draft(), used)
	nonce := randomToken()[:32]
	f := escrowFeeNonceForm("250", used, nonce)
	x.check(x.do("POST", "/admin/escrow-fee", admin, f), 409)
	if got := escrowFeeEffects(x, adminID); got != (escrowFeeNonceEffects{}) {
		t.Fatalf("used destination kept fee claim/effect: %+v", got)
	}
	f.Set("fee_address", regtestAddr("fee-nonce-unused"))
	x.check(x.do("POST", "/admin/escrow-fee", admin, f), 303) // refused nonce is reusable after rollback
	x.check(x.do("POST", "/admin/escrow-fee", admin, escrowFeeNonceForm("250", f.Get("fee_address"), randomToken()[:32])), 409)
	if got := escrowFeeEffects(x, adminID); got != (escrowFeeNonceEffects{1, 1, 2}) {
		t.Fatalf("fresh stale request changed fee: %+v", got)
	}
}

func TestEscrowFeeConfirmedNonceRechecksCurrentAdminAndSession(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		want         int
	}{
		{"demoted", "UPDATE users SET role='buyer' WHERE id=$1", 403},
		{"suspended", "UPDATE users SET suspended_at=now() WHERE id=$1", 401},
		{"password changed", "UPDATE users SET password_hash=$2 WHERE id=$1", 401},
		{"revoked session", "DELETE FROM sessions WHERE user_id=$1", 401},
		{"expired session", "UPDATE sessions SET expires=clock_timestamp()-interval '1 second' WHERE user_id=$1", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newEscrowEnv(t)
			adminID, admin := x.user("fee_nonce_auth", "admin")
			f := escrowFeeNonceForm("350", regtestAddr("fee-nonce-auth"), randomToken()[:32])
			x.check(x.do("POST", "/admin/escrow-fee", admin, f), 303)
			committed := escrowFeeEffects(x, adminID)
			if tc.name == "password changed" {
				hash, err := hashPassword("a-different-long-password")
				if err != nil {
					t.Fatal(err)
				}
				agExec(x.testEnv, tc.change, adminID, hash)
			} else {
				agExec(x.testEnv, tc.change, adminID)
			}
			x.check(x.do("POST", "/admin/escrow-fee", admin, f), tc.want)
			if got := escrowFeeEffects(x, adminID); got != committed {
				t.Fatalf("stale actor replay changed fee effects: before=%+v after=%+v", committed, got)
			}
		})
	}
}

func TestEscrowFeeConfirmedNonceLostCommitAcknowledgement(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("fee_nonce_lost", "admin")
	f := escrowFeeNonceForm("275", regtestAddr("fee-nonce-lost"), randomToken()[:32])
	lost := loseNextActionCommit(t, x.testEnv)
	w := x.do("POST", "/admin/escrow-fee", admin, f)
	if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
		!strings.Contains(w.Body.String(), "Market fee outcome unknown") ||
		!strings.Contains(w.Body.String(), "Check the fee setting and audit trail") ||
		!strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost fee acknowledgement status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if got := escrowFeeEffects(x, adminID); got != (escrowFeeNonceEffects{1, 1, 2}) {
		t.Fatalf("lost ack did not persist fee/receipt/audit once: %+v", got)
	}
	x.check(x.do("POST", "/admin/escrow-fee", admin, f), 303)
	if got := escrowFeeEffects(x, adminID); got != (escrowFeeNonceEffects{1, 1, 2}) {
		t.Fatalf("lost ack replay duplicated fee effects: %+v", got)
	}
}
