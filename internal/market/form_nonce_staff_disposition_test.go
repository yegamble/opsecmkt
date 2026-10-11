package market

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func collisionNonceFixture(t *testing.T) (*xmrEscrowEnv, string, string, string, string) {
	t.Helper()
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, mod := x.user("collision_nonce_"+randomToken()[:6], "moderator")
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("nonce collision "+order), xmrListingPrice, 10)
	x.refusePass(order)
	id := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_collisions WHERE order_id=$1", order)[0]
	return x, modID, mod, order, id
}

func TestXMRCollisionConfirmedNonceReplayAndLostAck(t *testing.T) {
	t.Run("exact replay and current eligibility", func(t *testing.T) {
		x, modID, mod, order, id := collisionNonceFixture(t)
		nonce := renderedFormNonce(t, x.page("/order?id="+order, mod), "/moderator/xmr-collision")
		if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/moderator/xmr-collision'"); n != 0 {
			t.Fatalf("GET wrote %d collision receipts", n)
		}
		f := url.Values{"collision_id": {id}, "note": {"Checked the refused deposit in the wallet."},
			"password": {testPassword}, formNonceField: {nonce}}
		events := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order)
		notices := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
		first := x.do("POST", "/moderator/xmr-collision", mod, f)
		x.check(first, 303)
		if x.collisionAudits(modID) != 1 || agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/moderator/xmr-collision'") != 1 {
			t.Fatal("first collision disposition did not persist one audit and receipt")
		}
		var disposed time.Time
		if err := x.DB.QueryRow("SELECT disposed_at FROM xmr_escrow_collisions WHERE id=$1", id).Scan(&disposed); err != nil {
			t.Fatal(err)
		}
		replay := x.do("POST", "/moderator/xmr-collision", mod, f)
		x.check(replay, 303)
		if replay.Header().Get("Location") != first.Header().Get("Location") || x.collisionAudits(modID) != 1 ||
			agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order) != events ||
			agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id IN ($1,$2)", x.buyer, x.vendor) != notices {
			t.Fatal("collision replay changed redirect/audit/event/notices")
		}
		var unchanged time.Time
		if err := x.DB.QueryRow("SELECT disposed_at FROM xmr_escrow_collisions WHERE id=$1", id).Scan(&unchanged); err != nil || !unchanged.Equal(disposed) {
			t.Fatal("collision replay changed disposition time")
		}
		changed := url.Values{"collision_id": {id}, "note": {"A different valid action was checked in the wallet."},
			"password": {testPassword}, formNonceField: {nonce}}
		x.check(x.do("POST", "/moderator/xmr-collision", mod, changed), 409)
		x.check(x.do("POST", "/moderator/xmr-collision", agSession(x.testEnv, modID), f), 409)
		f.Set("password", "wrong password")
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 401)
		f.Set("password", testPassword)
		f.Set(formNonceField, randomToken()[:32])
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 409)
		f.Set(formNonceField, nonce)
		agExec(x.testEnv, "UPDATE orders SET buyer_id=$1 WHERE id=$2", modID, order)
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 403)
		if x.collisionAudits(modID) != 1 {
			t.Fatal("current-party refusal added audit")
		}
	})
	t.Run("TOTP and current staff role", func(t *testing.T) {
		x, modID, mod, _, id := collisionNonceFixture(t)
		key, _ := agEnableTOTP(x.testEnv, modID)
		f := url.Values{"collision_id": {id}, "note": {"Wallet ownership of this deposit was checked."},
			"password": {testPassword}, "code": {agTOTPNow(key)}, formNonceField: {randomToken()[:32]}}
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 303)
		step := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", modID)
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 303)
		if got := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", modID); got != step {
			t.Fatalf("collision replay spent TOTP again: %d to %d", step, got)
		}
		original := f.Get(formNonceField)
		f.Set(formNonceField, randomToken()[:32])
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 401)
		f.Set(formNonceField, original)
		agExec(x.testEnv, "UPDATE users SET role='buyer' WHERE id=$1", modID)
		if w := x.do("POST", "/moderator/xmr-collision", mod, f); w.Code == 303 {
			t.Fatal("demoted moderator replayed collision disposition")
		}
		agExec(x.testEnv, "UPDATE users SET role='moderator',suspended_at=now() WHERE id=$1", modID)
		if w := x.do("POST", "/moderator/xmr-collision", mod, f); w.Code == 303 {
			t.Fatal("suspended moderator replayed collision disposition")
		}
	})
	t.Run("lost commit acknowledgement", func(t *testing.T) {
		x, modID, mod, order, id := collisionNonceFixture(t)
		f := url.Values{"collision_id": {id}, "note": {"Wallet and refused deposit were checked carefully."},
			"password": {testPassword}, formNonceField: {randomToken()[:32]}}
		lost := loseNextActionCommit(t, x.testEnv)
		w := x.do("POST", "/moderator/xmr-collision", mod, f)
		if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
			!strings.Contains(w.Body.String(), "Staff disposition outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
			t.Fatalf("lost collision ack status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		if x.collisionAudits(modID) != 1 || agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_collisions WHERE id=$1 AND disposed_at IS NOT NULL", id) != 1 {
			t.Fatal("lost ACK did not persist the collision disposition")
		}
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 303)
		if x.collisionAudits(modID) != 1 || agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/moderator/xmr-collision'") != 1 {
			t.Fatal("lost ACK replay duplicated the collision disposition")
		}
		var saved string
		if err := x.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", f.Get(formNonceField)).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{testPassword, f.Get("note"), mod} {
			if strings.Contains(saved, secret) {
				t.Fatal("collision receipt retained raw form or session")
			}
		}
		_ = order
	})
}
