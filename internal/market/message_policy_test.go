package market

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
)

// A-543 (owner directive 2026-10-18): a non-staff account starts a message only to an active vendor, an order
// partner, someone who has messaged it, or staff it can see (the public staff list, the staff contact, or a dispute
// it is a party to). Moderators and administrators message anyone. An unknown handle and a handle the sender may not
// message answer alike and write nothing.

func TestMessageContactPolicy(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("policy_buyer", "buyer")
	otherBuyerID, otherBuyer := e.user("policy_other_buyer", "buyer")
	vendorID, vendor := e.user("policy_vendor", "vendor")
	partnerVendorID, partnerVendor := e.user("policy_partner_vendor", "vendor")
	_, _ = e.user("policy_third_buyer", "buyer")
	suspendedVendorID, _ := e.user("policy_suspended_vendor", "vendor")
	closedVendorID, _ := e.user("policy_closed_vendor", "vendor")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", suspendedVendorID)
	agExec(e, "UPDATE users SET closed_at=now() WHERE id=$1", closedVendorID)
	_, moderator := e.user("policy_moderator", "moderator")
	listedModID, _ := e.user("policy_listed_mod", "moderator")
	_, _ = e.user("policy_hidden_mod", "moderator")
	_, _ = e.user("policy_contact_admin", "admin")
	_, admin := e.user("policy_admin", "admin")
	agExec(e, "UPDATE users SET staff_hidden=false WHERE id=$1", listedModID)
	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_list_public','true') ON CONFLICT(key) DO UPDATE SET value=excluded.value")
	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact','policy_contact_admin') ON CONFLICT(key) DO UPDATE SET value=excluded.value")
	// An order on the partner vendor's listing makes policy_buyer and policy_partner_vendor order partners.
	e.order(buyerID, e.product(partnerVendorID, "physical"), "XMR", stateAwaitingPayment)
	// A message stored before the policy: policy_other_buyer wrote to policy_buyer, who may now reply.
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'legacy inbound',true)`, randomToken(), otherBuyerID, buyerID)

	key, _ := testPGPKey(t, "policy-recipient")
	body, err := encryptTo(key, "policy matrix message")
	if err != nil {
		t.Fatal(err)
	}
	written := func() int {
		t.Helper()
		return abuseCount(e, "SELECT count(*) FROM messages") + abuseCount(e, "SELECT count(*) FROM notifications") +
			abuseCount(e, "SELECT count(*) FROM audit_events WHERE action LIKE 'Sent OpenPGP-encrypted message%'")
	}
	send := func(session, recipient string) *bytesResult {
		t.Helper()
		w := e.do("POST", "/messages", session, url.Values{"recipient": {recipient}, "body": {body}})
		return &bytesResult{code: w.Code, body: w.Body.Bytes()}
	}
	for _, c := range []struct {
		name, session, recipient string
		ok                       bool
	}{
		{"vendor to buyer without an order", vendor, "policy_buyer", false},
		{"buyer to active vendor", buyer, "policy_vendor", true},
		{"vendor to buyer after the buyer wrote", vendor, "policy_buyer", true},
		{"buyer to buyer", buyer, "policy_third_buyer", false},
		{"other buyer to buyer before any reply", otherBuyer, "policy_buyer", false},
		{"vendor to buyer with an order", partnerVendor, "policy_buyer", true},
		{"buyer to vendor with an order", buyer, "policy_partner_vendor", true},
		{"reply after an inbound message", buyer, "policy_other_buyer", true},
		{"other buyer to buyer after the reply", otherBuyer, "policy_buyer", true},
		{"moderator to a buyer", moderator, "policy_other_buyer", true},
		{"administrator to a buyer", admin, "policy_buyer", true},
		{"buyer to listed staff", otherBuyer, "policy_listed_mod", true},
		{"buyer to the staff contact", otherBuyer, "policy_contact_admin", true},
		{"buyer to unlisted staff", otherBuyer, "policy_hidden_mod", false},
		{"buyer to a suspended vendor", buyer, "policy_suspended_vendor", false},
		{"buyer to a closed vendor", buyer, "policy_closed_vendor", false},
		{"vendor to another vendor", vendor, "policy_partner_vendor", true},
	} {
		before := written()
		r := send(c.session, c.recipient)
		if c.ok {
			if r.code != 303 {
				t.Fatalf("%s: status %d body %s", c.name, r.code, r.body)
			}
			if written() != before+3 {
				t.Fatalf("%s: wrote %d rows, want a message, a notification and an audit row", c.name, written()-before)
			}
			continue
		}
		if r.code != 403 {
			t.Fatalf("%s: status %d body %s", c.name, r.code, r.body)
		}
		mustContain(t, string(r.body), messageNotPermitted)
		if written() != before {
			t.Fatalf("%s: a refused message wrote %d rows", c.name, written()-before)
		}
	}
	// An unknown handle and an existing handle the sender may not message answer byte for byte alike.
	unknown, forbidden := send(vendor, "policy_never_registered"), send(vendor, "policy_other_buyer")
	if unknown.code != forbidden.code || !bytes.Equal(unknown.body, forbidden.body) {
		t.Fatalf("unknown (%d) and forbidden (%d) refusals differ:\n%s\n----\n%s", unknown.code, forbidden.code, unknown.body, forbidden.body)
	}

	// The public staff list links a signed-in reader to each listed member's compose page; a visitor gets no link.
	canary := e.body("GET", "/canary", otherBuyer, nil, 200)
	mustContain(t, canary, `href="/messages?to=policy_listed_mod">Message policy_listed_mod</a>`)
	mustNotContain(t, canary, "policy_hidden_mod")
	mustNotContain(t, e.body("GET", "/canary", "", nil, 200), `href="/messages?to=`)

	// A dispute party may write to any current staff member: the order page lists them all (disputeStaffLoader).
	disputed := e.order(otherBuyerID, e.product(vendorID, "digital"), "XMR", stateAwaitingPayment)
	if r := send(otherBuyer, "policy_hidden_mod"); r.code != 403 {
		t.Fatalf("before the dispute: status %d", r.code)
	}
	agExec(e, "UPDATE orders SET state=$1 WHERE id=$2", stateDisputed, disputed)
	if r := send(otherBuyer, "policy_hidden_mod"); r.code != 303 {
		t.Fatalf("dispute party to staff: status %d body %s", r.code, r.body)
	}
}

type bytesResult struct {
	code int
	body []byte
}

// The retention rule still applies on top of the contact policy, and the message budget is still spent before the
// recipient is looked up (A-516).
func TestMessageContactPolicyKeepsRetentionAndBudget(t *testing.T) {
	e := newTestApp(t)
	restrictedID, restricted := e.user("policy_restricted", "buyer")
	_, _ = e.user("policy_open_vendor", "vendor")
	_, _ = e.user("policy_some_buyer", "buyer")
	agExec(e, "UPDATE users SET retention_restricted_at=now() WHERE id=$1", restrictedID)
	key, _ := testPGPKey(t, "policy-retention")
	body, err := encryptTo(key, "retention message")
	if err != nil {
		t.Fatal(err)
	}
	send := func(session, recipient string) (int, string) {
		w := e.do("POST", "/messages", session, url.Values{"recipient": {recipient}, "body": {body}})
		return w.Code, w.Body.String()
	}
	code, page := send(restricted, "policy_open_vendor")
	if code != 403 || !strings.Contains(page, "While deletion waits for funds") {
		t.Fatalf("restricted sender to a vendor with no open order: %d %s", code, page)
	}
	unknownCode, unknownPage := send(restricted, "policy_nobody_here")
	forbiddenCode, forbiddenPage := send(restricted, "policy_some_buyer")
	if unknownCode != 403 || unknownCode != forbiddenCode || unknownPage != forbiddenPage || !strings.Contains(unknownPage, messageNotPermitted) {
		t.Fatalf("restricted sender: unknown %d and forbidden %d differ or are not the policy refusal", unknownCode, forbiddenCode)
	}
	if n := abuseCount(e, "SELECT count(*) FROM messages"); n != 0 {
		t.Fatalf("messages=%d", n)
	}

	_, sender := e.user("policy_budget_sender", "buyer")
	for i := range messageBudget {
		if code, _ := send(sender, "policy_nobody_here"); code != 403 {
			t.Fatalf("refusal %d: status %d", i+1, code)
		}
	}
	if code, page := send(sender, "policy_open_vendor"); code != 429 || !strings.Contains(page, "at most 20 messages in 10 minutes") {
		t.Fatalf("after %d refused sends: status %d", messageBudget, code)
	}
}

// QA543-1: a vendor reaches a buyer only through an order its /orders list shows (vendorSeesOrder). A draft the buyer
// saved, or one cancelled before checkout, is not customer work: the vendor's send is refused exactly as an unknown
// handle is, and nothing is written. Once the order awaits payment, or a cancelled one had a payment, it may write.
// The buyer's side is any order, drafts included.
func TestMessageVendorToBuyerNeedsAVisibleOrder(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("vis_buyer", "buyer")
	vendorID, vendor := e.user("vis_vendor", "vendor")
	key, _ := testPGPKey(t, "vis-recipient")
	body, err := encryptTo(key, "visibility message")
	if err != nil {
		t.Fatal(err)
	}
	written := func() int {
		return abuseCount(e, "SELECT count(*) FROM messages") + abuseCount(e, "SELECT count(*) FROM notifications") +
			abuseCount(e, "SELECT count(*) FROM audit_events WHERE action LIKE 'Sent OpenPGP-encrypted message%'")
	}
	send := func(session, recipient string) *bytesResult {
		t.Helper()
		w := e.do("POST", "/messages", session, url.Values{"recipient": {recipient}, "body": {body}})
		return &bytesResult{code: w.Code, body: w.Body.Bytes()}
	}
	refusedLikeUnknown := func(name string) {
		t.Helper()
		before := written()
		forbidden, unknown := send(vendor, "vis_buyer"), send(vendor, "vis_nobody_xx")
		if forbidden.code != 403 || forbidden.code != unknown.code || !bytes.Equal(forbidden.body, unknown.body) {
			t.Fatalf("%s: forbidden %d and unknown %d refusals differ:\n%s", name, forbidden.code, unknown.code, forbidden.body)
		}
		if written() != before {
			t.Fatalf("%s: a refused message wrote %d rows", name, written()-before)
		}
	}
	draftOn := func(product string) string {
		t.Helper()
		w := e.do("POST", "/orders", buyer, url.Values{"product_id": {product}, "currency": {"XMR"}})
		e.check(w, 303)
		return strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
	}

	// Vendor to buyer with only a draft: refused.
	first := draftOn(e.product(vendorID, "digital"))
	if n := abuseCount(e, "SELECT count(*) FROM orders WHERE id=$1 AND state='draft' AND buyer_id=$2", first, buyerID); n != 1 {
		t.Fatalf("POST /orders made %d drafts", n)
	}
	refusedLikeUnknown("vendor to buyer with only a draft")
	// The buyer's side is any order: it may write to the vendor of its draft.
	if r := send(buyer, "vis_vendor"); r.code != 303 {
		t.Fatalf("buyer to the vendor of its draft: %d %s", r.code, r.body)
	}
	agExec(e, "DELETE FROM messages")
	agExec(e, "DELETE FROM notifications")

	// After the draft was cancelled before checkout: still refused.
	e.check(e.do("POST", "/orders/cancel", buyer, url.Values{"order_id": {first}, "from": {stateDraft}}), 303)
	if n := abuseCount(e, "SELECT count(*) FROM orders WHERE id=$1 AND state='cancelled'", first); n != 1 {
		t.Fatal("the draft was not cancelled")
	}
	refusedLikeUnknown("after the draft was cancelled")

	// Once an order awaits payment, the vendor sees it and may write.
	e.order(buyerID, e.product(vendorID, "digital"), "XMR", stateAwaitingPayment)
	if r := send(vendor, "vis_buyer"); r.code != 303 {
		t.Fatalf("vendor to buyer once awaiting_payment: %d %s", r.code, r.body)
	}

	// A cancelled order that had a recorded payment is customer work too.
	lateVendorID, otherVendor := e.user("vis_late_vendor", "vendor")
	third := e.order(buyerID, e.product(lateVendorID, "digital"), "XMR", stateCancelled)
	if r := send(otherVendor, "vis_buyer"); r.code != 403 {
		t.Fatalf("late vendor through a cancelled order with no escrow or payment: %d", r.code)
	}
	agExec(e, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'XMR',$2,0,'vis-address',100000,12)", third, strings.Repeat("ab", 32))
	if r := send(otherVendor, "vis_buyer"); r.code != 303 {
		t.Fatalf("vendor to buyer through a cancelled order with a payment: %d %s", r.code, r.body)
	}
}

// GET /messages?to= loads a key only for a handle the viewer may message; an unknown handle and a forbidden one render
// the same page apart from the handle itself, with no key and no fingerprint.
func TestMessageRecipientKeyFollowsContactPolicy(t *testing.T) {
	e := newTestApp(t)
	_, viewer := e.user("policy_viewer", "buyer")
	_, moderator := e.user("policy_key_mod", "moderator")
	sessions, ids := map[string]string{}, map[string]string{}
	for _, handle := range []string{"policy_key_vendor", "policy_key_buyer"} {
		role := "vendor"
		if handle == "policy_key_buyer" {
			role = "buyer"
		}
		id, session := e.user(handle, role)
		sessions[handle], ids[handle] = session, id
		_, pub := testPGPKey(t, handle)
		e.check(e.do("POST", "/account", session, url.Values{"pgp": {pub}, "password": {testPassword}}), 303)
	}
	const keyShown, armor = `class="contact-key"`, "-----BEGIN PGP PUBLIC KEY BLOCK-----"
	lookup := func(session, handle string) string {
		t.Helper()
		return e.body("GET", "/messages?to="+url.QueryEscape(handle), session, nil, 200)
	}
	mustContain(t, lookup(viewer, "policy_key_vendor"), keyShown, armor, "fingerprint", `name="recipient" value="policy_key_vendor"`)
	forbidden, unknown := lookup(viewer, "policy_key_buyer"), lookup(viewer, "policy_key_nobody")
	for _, page := range []string{forbidden, unknown} {
		mustContain(t, page, messageNotPermitted)
		mustNotContain(t, page, keyShown, armor, "fingerprint", `name="recipient"`, `name="body"`)
	}
	if strings.ReplaceAll(forbidden, "policy_key_buyer", "HANDLE") != strings.ReplaceAll(unknown, "policy_key_nobody", "HANDLE") {
		t.Fatal("the forbidden and unknown lookups render differently")
	}
	// QA543-1: the vendor loads the buyer's key only through an order its /orders list shows. With only a draft
	// (made through POST /orders), or after that draft was cancelled before checkout, the page is the unknown one.
	vendorView := func() string {
		t.Helper()
		return strings.ReplaceAll(lookup(sessions["policy_key_vendor"], "policy_key_buyer"), "policy_key_buyer", "HANDLE")
	}
	vendorUnknown := strings.ReplaceAll(lookup(sessions["policy_key_vendor"], "policy_key_nobody"), "policy_key_nobody", "HANDLE")
	w := e.do("POST", "/orders", sessions["policy_key_buyer"], url.Values{"product_id": {e.product(ids["policy_key_vendor"], "digital")}, "currency": {"XMR"}})
	e.check(w, 303)
	draft := strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
	for _, step := range []string{"with only a draft", "after the draft was cancelled"} {
		if step == "after the draft was cancelled" {
			e.check(e.do("POST", "/orders/cancel", sessions["policy_key_buyer"], url.Values{"order_id": {draft}, "from": {stateDraft}}), 303)
		}
		if page := vendorView(); page != vendorUnknown {
			t.Fatalf("vendor ?to= buyer %s differs from an unknown handle", step)
		}
	}
	e.order(ids["policy_key_buyer"], e.product(ids["policy_key_vendor"], "digital"), "XMR", stateAwaitingPayment)
	mustContain(t, lookup(sessions["policy_key_vendor"], "policy_key_buyer"), keyShown, armor, `name="recipient" value="policy_key_buyer"`)

	// A non-staff page has no free handle search; staff keep one and load any account's key.
	mustNotContain(t, lookup(viewer, "policy_key_vendor"), "Find a recipient", `name="to"`)
	mustNotContain(t, e.body("GET", "/messages", viewer, nil, 200), "Find a recipient", `name="to"`, `name="recipient"`)
	staffPage := lookup(moderator, "policy_key_buyer")
	mustContain(t, staffPage, "Find a recipient", `name="to"`, keyShown, armor, `name="recipient"`)
}

// Each received message links a reply prefilled with its sender; a sent one does not.
func TestMessageReplyLink(t *testing.T) {
	e := newTestApp(t)
	viewerID, viewer := e.user("reply_viewer", "buyer")
	senderID, _ := e.user("reply_sender", "vendor")
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'inbound',true),($4,$3,$2,'outbound',true)`,
		randomToken(), senderID, viewerID, randomToken())
	page := e.body("GET", "/messages", viewer, nil, 200)
	mustContain(t, page, `href="/messages?to=reply_sender">Reply to reply_sender`)
	if strings.Count(page, ">Reply to ") != 1 {
		t.Fatal("want exactly one reply link, on the received message")
	}
}
