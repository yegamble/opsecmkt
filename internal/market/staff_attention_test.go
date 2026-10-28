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
	wallet := xmrWallet(t, e, 1, "active")
	walletName := agStr(e, "SELECT name FROM xmr_escrow_wallets WHERE id=$1", wallet)
	orders := make([]string, 101)
	addresses := make([]string, len(orders))
	for i := range orders {
		orders[i] = e.order(buyer, product, "XMR", stateDisputed)
		account := int64(i + 1)
		addresses[i], _ = xmrEscrowOf(t, e, wallet, account, orders[i])
		if _, err := e.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The buyer says the parcel never arrived.')", randomToken(), orders[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,locked)
			VALUES($1,'XMR',$2,$3,$4,1000,3,true,true)`, orders[i], fmt.Sprintf("%064x", i+1), xmrEscrowIdx(wallet, account), addresses[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := e.DB.Exec(`INSERT INTO xmr_escrow_collisions(order_id,wallet_name,account,txid)
			VALUES($1,$2,$3,$4)`, orders[i], walletName, account, fmt.Sprintf("%064x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	// Two open deposits for one order still represent one review order. An escalated review is actionable only by an admin.
	if _, err := e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,locked)
		VALUES($1,'XMR',$2,$3,$4,1000,3,true,true)`, orders[0], strings.Repeat("a", 64), xmrEscrowIdx(wallet, 1), addresses[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("UPDATE payments SET disposition='escalated' WHERE order_id=$1", orders[1]); err != nil {
		t.Fatal(err)
	}

	// A staff member who is a party cannot count any of the three moderator-desk categories on that order.
	partyOrder := e.order(mod, product, "XMR", stateDisputed)
	partyAddress, _ := xmrEscrowOf(t, e, wallet, 1000, partyOrder)
	if _, err := e.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The staff buyer says the parcel never arrived.')", randomToken(), partyOrder); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,locked)
		VALUES($1,'XMR',$2,$3,$4,1000,3,true,true)`, partyOrder, strings.Repeat("b", 64), xmrEscrowIdx(wallet, 1000), partyAddress); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("INSERT INTO xmr_escrow_collisions(order_id,wallet_name,account,txid) VALUES($1,$2,1000,$3)", partyOrder, walletName, strings.Repeat("c", 64)); err != nil {
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
	if modCounts == nil || modCounts.Disputes != 0 || modCounts.PaymentReviews != 100 || modCounts.EscrowAlerts != 101 {
		t.Fatalf("moderator counts = %+v, want 0 administrator-only disputes, 100 review orders, 101 alerts", modCounts)
	}
	adminCounts := load(admin, "admin")
	if adminCounts == nil || adminCounts.Disputes != 102 || adminCounts.PaymentReviews != 102 || adminCounts.EscrowAlerts != 102 {
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
	if modPage.Code != 200 || !strings.Contains(modPage.Body.String(), "0 disputes · 100 review orders · 101 alerts") || !strings.Contains(modPage.Body.String(), "100 orders under payment review") {
		t.Fatalf("moderator page did not show its scoped counts: status=%d", modPage.Code)
	}
	adminPage := e.do("GET", "/admin", adminSession, nil)
	if adminPage.Code != 200 || !strings.Contains(adminPage.Body.String(), "102 disputes · 102 review orders · 102 alerts") {
		t.Fatalf("admin page did not show its scoped counts: status=%d", adminPage.Code)
	}
	// A-492: pooled payouts are removed, so no page counts or links them.
	for _, page := range []string{modPage.Body.String(), adminPage.Body.String()} {
		mustNotContain(t, page, " payouts)", "payouts</a>", `#payouts"`)
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
	cleared := load(mod, "moderator")
	if cleared == nil || cleared.Disputes != 0 || cleared.PaymentReviews != 99 || cleared.EscrowAlerts != 100 {
		t.Fatalf("cleared counts = %+v, want 0 administrator-only disputes, 99 reviews, 100 alerts", cleared)
	}
	clearedAdmin := load(admin, "admin")
	if clearedAdmin == nil || clearedAdmin.Disputes != 101 || clearedAdmin.PaymentReviews != 101 || clearedAdmin.EscrowAlerts != 101 {
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

// A-686 (STAFF94-6): a listing awaiting review is counted in the staff badge, the overview line and the
// Vendors section flag, except for its own vendor, who cannot review it.
func TestStaffAttentionCountsPendingListings(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("pending_count_vendor", "vendor")
	mod, modSession := e.user("pending_count_mod", "moderator")
	other, otherSession := e.user("pending_count_mod2", "moderator")
	admin, adminSession := e.user("pending_count_admin", "admin")
	load := func(id, role string) *StaffAttention {
		t.Helper()
		d := &PageData{User: &User{ID: id, Role: role}}
		if err := loadStaffAttention(t.Context(), e.A, nil, d); err != nil {
			t.Fatal(err)
		}
		return d.StaffAttention
	}
	if got := load(mod, "moderator"); got == nil || got.ListingReviews != 0 {
		t.Fatalf("empty queue counts %+v", got)
	}
	mustNotContain(t, e.body("GET", "/admin", adminSession, nil, 200), "await review.")
	held := e.product(vendor, "physical")
	own := e.product(mod, "physical")
	// Approved, rejected and archived pending listings are not awaiting review.
	for _, state := range []string{"approved", "rejected", "pending"} {
		p := e.product(vendor, "physical")
		reason := any(nil)
		if state == "rejected" {
			reason = "Rejected in the fixture."
		}
		agExec(e, "UPDATE products SET review_state=$2,review_rejection_reason=$3,archived=$4 WHERE id=$1", p, state, reason, state == "pending")
	}
	agExec(e, "UPDATE products SET review_state='pending' WHERE id=$1 OR id=$2", held, own)
	for _, v := range []struct {
		id, role string
		want     int
	}{{mod, "moderator", 1}, {other, "moderator", 2}, {admin, "admin", 2}} {
		if got := load(v.id, v.role); got == nil || got.ListingReviews != v.want {
			t.Fatalf("%s counts %+v, want %d listing reviews", v.role, got, v.want)
		}
	}
	// With the moderator's own listing approved again, one listing awaits review for every staff member.
	agExec(e, "UPDATE products SET review_state='approved' WHERE id=$1", own)
	for _, s := range []string{modSession, otherSession} {
		mustContain(t, e.body("GET", "/moderator", s, nil, 200),
			`<a href="/moderator#listing-review">1 listing awaiting review</a>`, "· 1 listing to review)")
	}
	mustContain(t, e.body("GET", "/admin", adminSession, nil, 200),
		`<a href="/moderator#listing-review">1 listing awaiting review</a>`, "· 1 listing to review)", "1 listing(s) await review.")
	// The vendor-moderator owner does not count its own pending listing; another moderator does.
	agExec(e, "UPDATE products SET review_state='approved' WHERE id=$1", held)
	agExec(e, "UPDATE products SET review_state='pending' WHERE id=$1", own)
	mustContain(t, e.body("GET", "/moderator", modSession, nil, 200), `<a href="/moderator#listing-review">0 listings awaiting review</a>`, "· 0 listings to review)")
	mustContain(t, e.body("GET", "/moderator", otherSession, nil, 200), `<a href="/moderator#listing-review">1 listing awaiting review</a>`)
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
