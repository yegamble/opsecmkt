package market

import (
	"fmt"
	"net/url"
	"testing"
)

// A-516: abuse costs for an open public instance. Messages have a budget of their own apart from the general write
// budget, recipient lookups on /messages?to= are bounded by distinct handle, and an administrator's edit of another
// vendor's listing is told to that vendor.

func abuseCount(e *testEnv, query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.DB.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestMessageBudgetSeparateFromWrites(t *testing.T) {
	e := newTestApp(t)
	senderID, sender := e.user("budget_sender", "buyer")
	recipientID, _ := e.user("budget_recipient", "vendor")
	_, other := e.user("budget_other_sender", "buyer")
	key, _ := testPGPKey(t, "budget-recipient")
	body, err := encryptTo(key, "one encrypted message")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"recipient": {"budget_recipient"}, "body": {body}}
	counts := func() (messages, notes, audits int) {
		t.Helper()
		return abuseCount(e, "SELECT count(*) FROM messages WHERE sender_id=$1", senderID),
			abuseCount(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'New message from budget_sender%'", recipientID),
			abuseCount(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Sent OpenPGP-encrypted message%'", senderID)
	}
	for i := range messageBudget {
		if w := e.do("POST", "/messages", sender, form); w.Code != 303 {
			t.Fatalf("message %d: status %d body %s", i+1, w.Code, w.Body.String())
		}
	}
	w := e.do("POST", "/messages", sender, form)
	e.check(w, 429)
	mustContain(t, w.Body.String(), "Message not sent", "at most 20 messages in 10 minutes", "Nothing was stored")
	if m, n, a := counts(); m != messageBudget || n != messageBudget || a != messageBudget {
		t.Fatalf("after the refused message: messages=%d notifications=%d audits=%d, want %d each", m, n, a, messageBudget)
	}
	// The general write budget is not spent out: another change from this account is accepted.
	e.check(e.do("POST", "/notifications", sender, url.Values{"mark_all": {"1"}}), 303)
	// The budget is the account's own: another sender still sends.
	e.check(e.do("POST", "/messages", other, form), 303)
	if m, _, _ := counts(); m != messageBudget {
		t.Fatalf("messages from the limited sender=%d, want %d", m, messageBudget)
	}
}

func TestMessageRecipientLookupBudget(t *testing.T) {
	e := newTestApp(t)
	_, viewer := e.user("lookup_viewer", "buyer")
	_, otherViewer := e.user("lookup_viewer_two", "buyer")
	for _, handle := range []string{"lookup_first", "lookup_late"} {
		_, session := e.user(handle, "vendor")
		_, pub := testPGPKey(t, handle)
		e.check(e.do("POST", "/account", session, url.Values{"pgp": {pub}, "password": {testPassword}}), 303)
	}
	const keyShown, missing, paused = `class="contact-key"`, messageNotPermitted, "can look up at most 60 different handles"
	lookup := func(session, handle string) string {
		t.Helper()
		return e.body("GET", "/messages?to="+url.QueryEscape(handle), session, nil, 200)
	}
	mustContain(t, lookup(viewer, "lookup_first"), keyShown)
	// Looking the same handle up again spends nothing.
	for range 5 {
		page := lookup(viewer, "lookup_first")
		mustContain(t, page, keyShown)
		mustNotContain(t, page, paused)
	}
	for i := 2; i <= lookupBudget; i++ {
		page := lookup(viewer, fmt.Sprintf("lookup_miss_%02d", i))
		mustContain(t, page, missing)
		mustNotContain(t, page, paused)
	}
	// The 61st distinct handle: an account that exists and one that does not render alike, with neither a key
	// nor any word on whether the handle exists.
	for _, handle := range []string{"lookup_late", "lookup_never_registered"} {
		page := lookup(viewer, handle)
		mustContain(t, page, paused)
		mustNotContain(t, page, keyShown, missing, "-----BEGIN PGP PUBLIC KEY BLOCK-----")
	}
	// A handle already looked up still loads, and another account's budget is its own.
	mustContain(t, lookup(viewer, "lookup_first"), keyShown)
	mustContain(t, lookup(viewer, "lookup_miss_02"), missing)
	mustContain(t, lookup(otherViewer, "lookup_late"), keyShown)
	// Invalid prefill is ignored as before and spends nothing.
	mustNotContain(t, lookup(otherViewer, "x"), paused, missing)
}

func TestAdminListingEditNotifiesVendor(t *testing.T) {
	e := newTestApp(t)
	vendor, vendorS := e.user("edit_notice_vendor", "vendor")
	admin, adminS := e.user("edit_notice_admin", "admin")
	p := e.product(vendor, "physical")
	adminNotices := func() int {
		return abuseCount(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator edited your listing%'", vendor)
	}
	vendorAudits := func() int {
		return abuseCount(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Listing "+p[:8]+" edited by administrator%'", vendor)
	}
	// The vendor's own edit: neither.
	e.check(e.do("POST", "/listings/update", vendorS, inventoryForm(p, nil)), 303)
	if n, a := adminNotices(), vendorAudits(); n != 0 || a != 0 {
		t.Fatalf("own edit: notifications=%d vendor audits=%d", n, a)
	}
	// An administrator's edit: one notification naming the changed fields, one audit row on the vendor.
	e.check(e.do("POST", "/listings/update", adminS, inventoryForm(p, map[string]string{"title": "Admin retitled", "price_xmr": "0.8", "stock": "7"})), 303)
	if n, a := adminNotices(), vendorAudits(); n != 1 || a != 1 {
		t.Fatalf("admin edit: notifications=%d vendor audits=%d, want 1 each", n, a)
	}
	var note, audit string
	if err := e.DB.QueryRow("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator edited your listing%'", vendor).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.QueryRow("SELECT action FROM audit_events WHERE user_id=$1 AND action LIKE 'Listing % edited by administrator%'", vendor).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{note, audit} {
		mustContain(t, s, p[:8], "title, Monero price, stock")
		mustNotContain(t, s, "description", "category", "region", "Bitcoin price", "fulfillment", "dispatch")
	}
	mustContain(t, note, "“Edited listing”")
	mustContain(t, audit, "edit_notice_admin")
	if inventoryAudits(e, "Updated listing "+p[:8]+" as administrator") != 1 || abuseCount(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Updated listing%'", admin) != 1 {
		t.Fatal("the administrator's own audit row is missing")
	}
	// An administrator's save that changes nothing tells the vendor nothing.
	e.check(e.do("POST", "/listings/update", adminS, inventoryForm(p, map[string]string{"title": "Admin retitled", "price_xmr": "0.8", "stock": "7", "stock_seen": "7"})), 303)
	if n, a := adminNotices(), vendorAudits(); n != 1 || a != 1 {
		t.Fatalf("unchanged admin save: notifications=%d vendor audits=%d, want 1 each", n, a)
	}
}
