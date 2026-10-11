package market

import (
	"strings"
	"testing"
)

func TestListingCharacterLimitsAndCRLFNormalization(t *testing.T) {
	f := inventoryForm("", map[string]string{"title": strings.Repeat("界", 140), "description": strings.Repeat("界", 9998) + "\r\n終"})
	in, err := validateListing(f)
	if err != nil || in.Title != f.Get("title") || in.Description != strings.Repeat("界", 9998)+"\n終" {
		t.Fatalf("character-limit listing: %v", err)
	}
	for _, bad := range []map[string]string{
		{"title": strings.Repeat("界", 141)},
		{"description": strings.Repeat("界", 10001)},
		{"title": "invalid\xff"},
		{"description": "invalid\x00"},
	} {
		if _, err := validateListing(inventoryForm("", bad)); err == nil {
			t.Fatal("invalid listing text accepted")
		}
	}
}

func TestDisputeCharacterLimitsAndCRLFStorage(t *testing.T) {
	w := newDisputeOrderWorld(t)
	o := w.fundedPhysicalFor(w.buyerID, w.buyer, w.vendorID, w.vendor)
	if code, body := w.x.shipEscrow(o, w.vendor, regtestAddr("form-text-claim"), testPassword); code != 303 {
		t.Fatalf("ship physical escrow: %d %s", code, body)
	}
	// Twenty Unicode characters are valid despite occupying more than twenty bytes.
	for _, reason := range []string{strings.Repeat("界", 19), strings.Repeat("界", 5001), "invalid\xff reason long enough", "invalid\x00 reason long enough"} {
		w.expect("/disputes", w.buyer, form("order_id", o, "reason", reason), 400, "")
	}
	reason := strings.Repeat("界", 4998) + "\r\n終"
	w.expect("/disputes", w.buyer, form("order_id", o, "reason", reason), 303, "")
	var id, stored string
	if err := w.e.DB.QueryRow("SELECT id,reason FROM disputes WHERE order_id=$1", o).Scan(&id, &stored); err != nil || stored != normalizedFormText(reason) {
		t.Fatalf("stored reason differs: %v", err)
	}
	decision := strings.Repeat("決", 4998) + "\r\n終"
	_, admin := w.e.user("form_text_admin", "admin")
	w.expect("/resolve", admin, resolveConfirmed(form("id", id, "outcome", "refund", "resolution", decision), 100000), 303, "")
	if err := w.e.DB.QueryRow("SELECT resolution FROM disputes WHERE id=$1", id).Scan(&stored); err != nil || stored != normalizedFormText(decision) {
		t.Fatalf("stored decision differs: %v", err)
	}
}
