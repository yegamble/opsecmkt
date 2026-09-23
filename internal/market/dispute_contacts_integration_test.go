package market

import (
	"context"
	"strings"
	"testing"
)

func TestDisputeStaffContacts(t *testing.T) {
	w := newOrderWorld(t)
	e := w.e
	adminID, _ := e.user("contact_admin", "admin")
	key, public := testPGPKey(t, "staff")
	if _, err := e.DB.Exec("UPDATE users SET pgp=$1 WHERE id=$2", public, w.modID); err != nil {
		t.Fatal(err)
	}
	// An administrator who is also the vendor must never appear as independent staff.
	if _, err := e.DB.Exec("UPDATE users SET role='admin' WHERE id=$1", w.vendorID); err != nil {
		t.Fatal(err)
	}
	product := e.product(w.vendorID, "physical")
	order := e.order(w.buyerID, product, "BTC", stateDisputed)
	body := w.page("/order?id="+order, w.buyer, 200)
	if !strings.Contains(body, "Contact dispute staff") || !strings.Contains(body, "Ownership not verified") || !strings.Contains(body, "contact_admin has not added a PGP public key") {
		t.Fatal("staff keys and honest missing/verification states were not rendered")
	}
	d := &PageData{Order: &Order{BuyerID: w.buyerID, VendorID: w.vendorID, State: stateDisputed}, User: &User{ID: w.buyerID}}
	if err := disputeStaffLoader(context.Background(), e.A, nil, d); err != nil {
		t.Fatal(err)
	}
	if len(d.StaffContacts) != 2 {
		t.Fatalf("want independent moderator and admin, got %v", d.StaffContacts)
	}
	for _, key := range d.StaffContacts {
		if key.Verified {
			t.Fatal("unverified key marked verified")
		}
	}
	// A completed ownership proof is reflected honestly in the same directory.
	e.check(e.do("POST", "/pgp/challenge", w.mod, form("kind", "decrypt")), 303)
	var challenge string
	if err := e.DB.QueryRow("SELECT challenge FROM pgp_challenges WHERE user_id=$1", w.modID).Scan(&challenge); err != nil {
		t.Fatal(err)
	}
	e.check(e.do("POST", "/pgp/verify", w.mod, form("response", testDecrypt(t, key, challenge))), 303)
	if body := w.page("/order?id="+order, w.vendor, 200); !strings.Contains(body, "Ownership verified") {
		t.Fatal("verified staff key proof missing for vendor")
	}
	// Demotion removes the staff entry immediately; unrelated viewers and normal orders get none.
	if _, err := e.DB.Exec("UPDATE users SET role='buyer' WHERE id=$1", adminID); err != nil {
		t.Fatal(err)
	}
	d.StaffContacts = nil
	if err := disputeStaffLoader(context.Background(), e.A, nil, d); err != nil {
		t.Fatal(err)
	}
	if len(d.StaffContacts) != 1 {
		t.Fatal("demoted staff remains in directory")
	}
	w.page("/order?id="+order, w.other, 404)
	for _, state := range []string{stateDraft, statePaid} {
		d.Order.State = state
		d.StaffContacts = nil
		if err := disputeStaffLoader(context.Background(), e.A, nil, d); err != nil {
			t.Fatal(err)
		}
		if len(d.StaffContacts) != 0 {
			t.Fatal("staff contacts exposed before dispute")
		}
	}
}
