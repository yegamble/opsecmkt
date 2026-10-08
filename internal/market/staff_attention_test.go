package market

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaffAttentionCountsActionableWorkWithoutCapsOrCrossCategorySums(t *testing.T) {
	e := newTestApp(t)
	suffix := randomToken()[:6]
	buyer, _ := e.user("buyer_"+suffix, "buyer")
	vendor, _ := e.user("vendor_"+suffix, "vendor")
	mod, modSession := e.user("mod_"+suffix, "moderator")
	admin, adminSession := e.user("admin_"+suffix, "admin")
	product := e.product(vendor, "physical")
	orders := make([]string, 101)
	for i := range orders {
		orders[i] = e.order(buyer, product, "BTC", stateDisputed)
		if _, err := e.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The buyer says the parcel never arrived.')", randomToken(), orders[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,locked)
			VALUES($1,'BTC',$2,0,'test-address',1000,3,true,true)`, orders[i], fmt.Sprintf("%064x", i+1)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.DB.Exec(`INSERT INTO xmr_escrow_collisions(order_id,wallet_name,account,txid)
			VALUES($1,'test-wallet',$2,$3)`, orders[i], i+1, fmt.Sprintf("%064x", i+1)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.DB.Exec(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state)
			VALUES($1,'release',$2,'BTC',1000,'test-address','held')`, orders[i], vendor); err != nil {
			t.Fatal(err)
		}
	}
	// Two open deposits for one order still represent one review order. An escalated review is actionable only by an admin.
	if _, err := e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,locked)
		VALUES($1,'BTC',$2,1,'test-address',1000,3,true,true)`, orders[0], strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("UPDATE payments SET disposition='escalated' WHERE order_id=$1", orders[1]); err != nil {
		t.Fatal(err)
	}

	// A staff member who is a party cannot count any of the three moderator-desk categories on that order.
	partyOrder := e.order(mod, product, "BTC", stateDisputed)
	if _, err := e.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The staff buyer says the parcel never arrived.')", randomToken(), partyOrder); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,locked)
		VALUES($1,'BTC',$2,0,'test-address',1000,3,true,true)`, partyOrder, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("INSERT INTO xmr_escrow_collisions(order_id,wallet_name,account,txid) VALUES($1,'test-wallet',1000,$2)", partyOrder, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state)
		VALUES($1,'release',$2,'BTC',1000,'test-address','held')`, partyOrder, vendor); err != nil {
		t.Fatal(err)
	}

	load := func(id, role string) *StaffAttention {
		t.Helper()
		d := &PageData{User: &User{ID: id, Role: role}}
		r := httptest.NewRequest("GET", "/", nil)
		if err := loadStaffAttention(r.Context(), e.A, r, d); err != nil {
			t.Fatal(err)
		}
		return d.StaffAttention
	}
	modCounts := load(mod, "moderator")
	if modCounts == nil || modCounts.Disputes != 101 || modCounts.PaymentReviews != 100 || modCounts.EscrowAlerts != 101 || modCounts.Payouts != 0 {
		t.Fatalf("moderator counts = %+v, want 101 disputes, 100 review orders, 101 alerts, 0 payouts", modCounts)
	}
	adminCounts := load(admin, "admin")
	if adminCounts == nil || adminCounts.Disputes != 102 || adminCounts.PaymentReviews != 102 || adminCounts.EscrowAlerts != 102 || adminCounts.Payouts != 102 {
		t.Fatalf("admin counts = %+v, want 102 in each accessible category", adminCounts)
	}
	if got := load(mod, "admin"); got != nil {
		t.Fatalf("stale administrator role received counts after demotion: %+v", got)
	}
	for _, v := range []struct{ id, role string }{{buyer, "buyer"}, {vendor, "vendor"}} {
		if got := load(v.id, v.role); got != nil {
			t.Fatalf("%s account received attention counts: %+v", v.role, got)
		}
	}
	if _, err := e.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", mod); err != nil {
		t.Fatal(err)
	}
	if got := load(mod, "moderator"); got != nil {
		t.Fatalf("suspended staff account received attention counts: %+v", got)
	}
	if _, err := e.DB.Exec("UPDATE users SET suspended_at=NULL WHERE id=$1", mod); err != nil {
		t.Fatal(err)
	}

	modPage := e.do("GET", "/moderator", modSession, nil)
	if modPage.Code != 200 || !strings.Contains(modPage.Body.String(), "101 disputes · 100 review orders · 101 alerts") || !strings.Contains(modPage.Body.String(), "100 orders under payment review") {
		t.Fatalf("moderator page did not show its scoped counts: status=%d", modPage.Code)
	}
	adminPage := e.do("GET", "/admin", adminSession, nil)
	if adminPage.Code != 200 || !strings.Contains(adminPage.Body.String(), "102 disputes · 102 review orders · 102 alerts") || !strings.Contains(adminPage.Body.String(), "102 payouts") {
		t.Fatalf("admin page did not show its scoped counts: status=%d", adminPage.Code)
	}

	// Each category clears independently as its underlying work is resolved or disposed.
	if _, err := e.DB.Exec("UPDATE disputes SET status='Resolved' WHERE order_id=$1", orders[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("UPDATE payments SET disposition='no_action',disposed_at=now() WHERE order_id=$1", orders[3]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("UPDATE xmr_escrow_collisions SET disposition_note='Checked the wallet record.',disposed_at=now(),disposed_by=$2 WHERE order_id=$1", orders[4], mod); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("UPDATE payouts SET state='sent' WHERE order_id=$1", orders[5]); err != nil {
		t.Fatal(err)
	}
	cleared := load(mod, "moderator")
	if cleared == nil || cleared.Disputes != 100 || cleared.PaymentReviews != 99 || cleared.EscrowAlerts != 100 || cleared.Payouts != 0 {
		t.Fatalf("cleared counts = %+v, want 100 disputes, 99 reviews, 100 alerts, 0 payouts", cleared)
	}
	clearedAdmin := load(admin, "admin")
	if clearedAdmin == nil || clearedAdmin.Disputes != 101 || clearedAdmin.PaymentReviews != 101 || clearedAdmin.EscrowAlerts != 101 || clearedAdmin.Payouts != 101 {
		t.Fatalf("cleared admin counts = %+v, want 101 in each category", clearedAdmin)
	}
}

func TestStaffAttentionZeroEscrowAlertDoesNotLinkToMissingSection(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("mod_"+randomToken()[:6], "moderator")
	w := e.do("GET", "/moderator", session, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `href="/moderator">0 escrow alerts</a>`) || strings.Contains(w.Body.String(), `href="/moderator#escrow-alerts"`) {
		t.Fatalf("zero-alert link does not lead to a present target: status=%d", w.Code)
	}
}

// The navigation counter may disable JIT locally, but a borrowed pool connection must retain its original
// setting afterward, including when an obsolete staff identity yields no row.
func TestStaffAttentionKeepsConnectionJITSetting(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("jit_attention_admin", "admin")
	e.DB.SetMaxOpenConns(1)
	if _, err := e.DB.Exec("SET jit=on"); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", "moderator"} {
		d := &PageData{User: &User{ID: id, Role: role}}
		if err := loadStaffAttention(t.Context(), e.A, nil, d); err != nil {
			t.Fatal(err)
		}
		var after string
		if err := e.DB.QueryRow("SHOW jit").Scan(&after); err != nil || after != "on" {
			t.Fatalf("connection JIT changed: %q %v", after, err)
		}
		if (role == "admin") != (d.StaffAttention != nil) {
			t.Fatalf("stale role counted: %#v", d.StaffAttention)
		}
	}
}
