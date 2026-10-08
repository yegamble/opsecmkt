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

func resolveCounts(t *testing.T, p *payEnv, order string) (receipts, payouts, audits, events, notices int) {
	t.Helper()
	if err := p.DB.QueryRow("SELECT count(*) FROM form_submissions WHERE action_path='/resolve'").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql string
		arg any
		out *int
	}{
		{"SELECT count(*) FROM payouts WHERE order_id=$1", order, &payouts},
		{"SELECT count(*) FROM audit_events WHERE action LIKE $1", "Resolved dispute on order " + shortID(order) + "%", &audits},
		{"SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='disputed' AND to_state='resolved'", order, &events},
		{"SELECT count(*) FROM notifications WHERE body LIKE $1", "Order " + shortID(order) + " is now Resolved%", &notices},
	} {
		if err := p.DB.QueryRow(q.sql, q.arg).Scan(q.out); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestResolveNoncePaidRefundAndReleaseReplayOnce(t *testing.T) {
	for _, outcome := range []string{"refund", "release"} {
		t.Run(outcome, func(t *testing.T) {
			p := newPayEnv(t)
			p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
			p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
			dispute, moderator := p.disputedOrder(100000)
			desk := p.do("GET", "/moderator", moderator, nil)
			p.check(desk, 200)
			nonce := renderedFormNonce(t, desk.Body.String(), "/resolve")
			otherPage := p.do("GET", "/disputes", moderator, nil)
			p.check(otherPage, 200)
			if other := renderedFormNonce(t, otherPage.Body.String(), "/resolve"); other == nonce {
				t.Fatal("moderator and disputes renderings reused a resolve form nonce")
			}
			if n := p.count("SELECT count(*) FROM form_submissions WHERE action_path='/resolve'"); n != 0 {
				t.Fatalf("GET wrote %d resolve receipts", n)
			}
			f := resolveNonceForm(dispute, outcome, nonce)
			first := p.do("POST", "/resolve", moderator, f)
			p.check(first, 303)
			replay := p.do("POST", "/resolve", moderator, f)
			p.check(replay, 303)
			if first.Header().Get("Location") != replay.Header().Get("Location") || first.Header().Get("Location") == "" {
				t.Fatalf("resolve redirect changed: %q vs %q", first.Header().Get("Location"), replay.Header().Get("Location"))
			}
			if state := p.state(p.order); state != stateResolved {
				t.Fatalf("order state %s", state)
			}
			if n, paid, audits, events, notices := resolveCounts(t, p, p.order); n != 1 || paid != 1 || audits != 1 || events != 1 || notices != 2 {
				t.Fatalf("replay counts receipt=%d payout=%d audit=%d event=%d notice=%d", n, paid, audits, events, notices)
			}
			var kind string
			if err := p.DB.QueryRow("SELECT kind FROM payouts WHERE order_id=$1", p.order).Scan(&kind); err != nil {
				t.Fatal(err)
			}
			if kind != outcome {
				t.Fatalf("payout kind=%s want %s", kind, outcome)
			}
			var saved string
			if err := p.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{testPassword, f.Get("resolution"), moderator, "fake-testnet-buyer", "fake-testnet-vendor"} {
				if strings.Contains(saved, secret) {
					t.Fatal("receipt stored raw form or destination data")
				}
			}
			for _, field := range []string{"outcome", "amount_seen", "resolution", "payout_confirmed"} {
				changed := url.Values{}
				for k, v := range f {
					changed[k] = v
				}
				switch field {
				case "outcome":
					if outcome == "refund" {
						changed.Set(field, "release")
					} else {
						changed.Set(field, "refund")
					}
				case "amount_seen":
					changed.Set(field, "100001")
				case "resolution":
					changed.Set(field, f.Get(field)+" More evidence was considered.")
				case "payout_confirmed":
					changed[field] = []string{"confirmed", "extra"} // valid first value; exact body still differs
				}
				p.check(p.do("POST", "/resolve", moderator, changed), 409)
			}
			_, otherModerator := p.user("other_resolve_mod_"+randomToken()[:6], "moderator")
			p.check(p.do("POST", "/resolve", otherModerator, f), 409)
			fresh := resolveNonceForm(dispute, outcome, randomToken()[:32])
			p.check(p.do("POST", "/resolve", moderator, fresh), 409)
			if n, paid, audits, events, notices := resolveCounts(t, p, p.order); n != 1 || paid != 1 || audits != 1 || events != 1 || notices != 2 {
				t.Fatalf("refusals changed receipt=%d payout=%d audit=%d event=%d notice=%d", n, paid, audits, events, notices)
			}
		})
	}
}

func TestResolveNonceOldTOTPReplaysButCannotAuthorizeNewForm(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	dispute, moderator := p.disputedOrder(100000)
	actor, err := p.A.sessionUser(t.Context(), moderator)
	if err != nil || actor == nil {
		t.Fatalf("moderator session: %v", err)
	}
	key, _ := agEnableTOTP(p.testEnv, actor.ID)
	f := resolveNonceForm(dispute, "refund", randomToken()[:32])
	f.Set("code", agTOTPNow(key))
	p.check(p.do("POST", "/resolve", moderator, f), 303)
	var saved string
	if err := p.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", f.Get(formNonceField)).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(saved, `"`+f.Get("code")+`"`) {
		t.Fatal("receipt stored authenticator code")
	}
	var step int64
	if err := p.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", actor.ID).Scan(&step); err != nil {
		t.Fatal(err)
	}
	p.check(p.do("POST", "/resolve", moderator, f), 303)
	var after int64
	if err := p.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", actor.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != step {
		t.Fatalf("replay spent TOTP again: %d to %d", step, after)
	}
	f.Set(formNonceField, randomToken()[:32])
	p.check(p.do("POST", "/resolve", moderator, f), 401)
	if n, paid, audits, events, _ := resolveCounts(t, p, p.order); n != 1 || paid != 1 || audits != 1 || events != 1 {
		t.Fatalf("new form with old code changed receipt=%d payout=%d audit=%d event=%d", n, paid, audits, events)
	}
}

func TestResolveNonceCurrentEligibilityBeforeReplay(t *testing.T) {
	t.Run("now party", func(t *testing.T) {
		p := newPayEnv(t)
		p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
		dispute, moderator := p.disputedOrder(100000)
		actor, err := p.A.sessionUser(t.Context(), moderator)
		if err != nil || actor == nil {
			t.Fatalf("moderator session: %v", err)
		}
		f := resolveNonceForm(dispute, "release", randomToken()[:32])
		p.check(p.do("POST", "/resolve", moderator, f), 303)
		// Simulate a legacy/manual ownership correction. The application does not reassign a listing's owner.
		if _, err := p.DB.Exec("UPDATE products SET vendor_id=$1 WHERE id=$2", actor.ID, p.productID); err != nil {
			t.Fatal(err)
		}
		p.check(p.do("POST", "/resolve", moderator, f), 403)
		if n, paid, audits, events, _ := resolveCounts(t, p, p.order); n != 1 || paid != 1 || audits != 1 || events != 1 {
			t.Fatalf("party replay changed receipt=%d payout=%d audit=%d event=%d", n, paid, audits, events)
		}
	})
	t.Run("missing dispute", func(t *testing.T) {
		p := newPayEnv(t)
		p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
		dispute, moderator := p.disputedOrder(100000)
		f := resolveNonceForm(dispute, "refund", randomToken()[:32])
		p.check(p.do("POST", "/resolve", moderator, f), 303)
		if _, err := p.DB.Exec("DELETE FROM disputes WHERE id=$1", dispute); err != nil {
			t.Fatal(err)
		}
		p.check(p.do("POST", "/resolve", moderator, f), 409)
		if n, paid, audits, events, _ := resolveCounts(t, p, p.order); n != 1 || paid != 1 || audits != 1 || events != 1 {
			t.Fatalf("missing dispute replay changed receipt=%d payout=%d audit=%d event=%d", n, paid, audits, events)
		}
	})
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

func TestResolveNonceStaleReplyRollsBackClaim(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	dispute, moderator := p.disputedOrder(100000)
	f := resolveNonceForm(dispute, "release", randomToken()[:32])
	p.check(p.do("POST", "/disputes/reply", p.vendorSess, url.Values{
		"order_id": {p.order}, "reply": {"The shipment tracking confirms delivery to the agreed place."},
	}), 303)
	p.check(p.do("POST", "/resolve", moderator, f), 409)
	if n := p.count("SELECT count(*) FROM form_submissions WHERE action_path='/resolve'"); n != 0 {
		t.Fatalf("stale reply kept %d receipts", n)
	}
	f.Set("reply_seen", "1")
	p.check(p.do("POST", "/resolve", moderator, f), 303)
	if n, paid, audits, events, _ := resolveCounts(t, p, p.order); n != 1 || paid != 1 || audits != 1 || events != 1 {
		t.Fatalf("corrected reply form counts receipt=%d payout=%d audit=%d event=%d", n, paid, audits, events)
	}
}

func TestResolveNonceLostCommitAcknowledgementReplaysOnePayout(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	dispute, moderator := p.disputedOrder(100000)
	f := resolveNonceForm(dispute, "refund", randomToken()[:32])
	lost := loseNextActionCommit(t, p.testEnv)
	w := p.do("POST", "/resolve", moderator, f)
	if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
		!strings.Contains(w.Body.String(), "Resolution outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost acknowledgement: injected=%t state=%s payouts=%d status=%d retry=%q body=%q",
			!lost.Load(), p.state(p.order), p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order),
			w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	p.check(p.do("POST", "/resolve", moderator, f), 303)
	if n, paid, audits, events, notices := resolveCounts(t, p, p.order); n != 1 || paid != 1 || audits != 1 || events != 1 || notices != 2 {
		t.Fatalf("lost-ack replay counts receipt=%d payout=%d audit=%d event=%d notice=%d", n, paid, audits, events, notices)
	}
}
