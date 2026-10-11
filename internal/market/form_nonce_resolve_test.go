package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestReceiptAuthorizeRequiresConfirmedFormNonce(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("unguarded receipt authorization was registered")
		}
		if _, exists := actions["/invalid-receipt-authorize"]; exists {
			t.Fatal("invalid action was registered")
		}
	}()
	registerAction("/invalid-receipt-authorize", actionSpec{Roles: []string{"admin"}, OwnTx: true,
		ReceiptAuthorize: func(*actionCtx) error { return nil }, Run: func(*actionCtx) (actionResult, error) { return actionResult{}, nil }})
}

func resolveNonceForm(dispute, outcome, nonce string) url.Values {
	return resolveConfirmed(url.Values{
		"id": {dispute}, "outcome": {outcome},
		"resolution": {"The evidence supports this decision and the payment disposition."},
		"reply_seen": {"0"}, formNonceField: {nonce},
	}, 100000)
}

func TestResolveNonceEscrowStillRequiresCurrentAdminRole(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("nonce_escrow_admin_"+randomToken()[:6], "admin")
	_, moderator := x.user("nonce_escrow_mod_"+randomToken()[:6], "moderator")
	order, dispute := x.disputedEscrow(regtestAddr("nonce-escrow-refund"))
	f := resolveNonceForm(dispute, "refund", randomToken()[:32])
	w := x.do("POST", "/resolve", moderator, f)
	x.check(w, 403)
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/resolve'"); n != 0 {
		t.Fatalf("ineligible escrow moderator wrote %d receipts", n)
	}
	x.check(x.do("POST", "/resolve", admin, f), 303)
	settlement, kind, _ := x.mainSettlement(order)
	if kind != "refund" {
		t.Fatalf("settlement kind %q", kind)
	}
	// Model a current role change without deleting the old session. The receipt
	// must still enforce escrow's administrator-only rule at replay time.
	if _, err := x.DB.Exec("UPDATE users SET role='moderator' WHERE id=$1", adminID); err != nil {
		t.Fatal(err)
	}
	x.check(x.do("POST", "/resolve", admin, f), 403)
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE id=$1", settlement); n != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Resolved dispute on order%'", adminID) != 1 {
		t.Fatal("ineligible replay changed escrow settlement or audit")
	}
}

func TestResolveNonceEscrowOldTOTPDoesNotAuthorizeNewForm(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("nonce_totp_admin_"+randomToken()[:6], "admin")
	order, dispute := x.disputedEscrow(regtestAddr("nonce-totp-refund"))
	key, _ := agEnableTOTP(x.testEnv, adminID)
	f := resolveNonceForm(dispute, "refund", randomToken()[:32])
	f.Set("code", agTOTPNow(key))
	x.check(x.do("POST", "/resolve", admin, f), 303)
	var step int64
	if err := x.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", adminID).Scan(&step); err != nil {
		t.Fatal(err)
	}
	x.check(x.do("POST", "/resolve", admin, f), 303)
	var after int64
	if err := x.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", adminID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != step {
		t.Fatalf("receipt replay spent TOTP twice: %d to %d", step, after)
	}
	f.Set(formNonceField, randomToken()[:32])
	x.check(x.do("POST", "/resolve", admin, f), 401)
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='refund'", order); n != 1 {
		t.Fatalf("new form with old code changed settlements: %d", n)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/resolve'"); n != 1 {
		t.Fatalf("new form with old code wrote %d receipts", n)
	}
}

func TestResolveNonceEscrowLostCommitAcknowledgementReplaysOneSettlement(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("nonce_lost_admin_"+randomToken()[:6], "admin")
	order, dispute := x.disputedEscrow(regtestAddr("nonce-lost-refund"))
	f := resolveNonceForm(dispute, "refund", randomToken()[:32])
	lost := loseNextActionCommit(t, x.testEnv)
	w := x.do("POST", "/resolve", admin, f)
	if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), "Resolution outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost acknowledgement: injected=%t status=%d retry=%q body=%q", !lost.Load(), w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	x.check(x.do("POST", "/resolve", admin, f), 303)
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='refund'", order); n != 1 {
		t.Fatalf("lost-ack replay wrote %d settlements", n)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/resolve'"); n != 1 {
		t.Fatalf("lost-ack replay wrote %d receipts", n)
	}
}
