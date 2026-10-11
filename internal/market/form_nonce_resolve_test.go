package market

import (
	"net/url"
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
