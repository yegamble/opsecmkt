package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// A-124: a dispute has a record from both sides. The party who did not open it may put one reply on record, stored
// once as an order event (no state change), shown to both parties and on the moderation desk with its role.

// replyEvents counts the dispute replies stored for order.
func (w *orderWorld) replyEvents(order string) int {
	return w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state='disputed' AND to_state='disputed' AND note LIKE 'Dispute reply: %'", order)
}

func TestDisputeCounterpartyResponse(t *testing.T) {
	w := newDisputeOrderWorld(t)
	reason := "The parcel arrived empty.\nThe seal on the box was broken."
	order, _ := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, reason)
	events := w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order)

	// The dispute-opened notice tells the vendor (who did not open it) that a reply exists; the buyer gets none.
	notice := "Order " + shortID(order) + " is now Disputed. You may put one reply to the dispute on record from the order page before an administrator decides."
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", w.vendorID, notice); n != 1 {
		t.Fatalf("vendor dispute notices with the reply hint: %d", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Order % is now Disputed.%'", w.buyerID); n != 0 {
		t.Fatalf("the opener got %d dispute transition notices", n)
	}

	// Line breaks in the reason survive (pre-wrap) on the order page and the desk; the vendor is offered the form,
	// the buyer (the opener) is not.
	vendorPage := w.page("/order?id="+order, w.vendor, 200)
	mustContain(t, vendorPage, `<p class="dispute-text">The parcel arrived empty.`+"\n"+`The seal on the box was broken.</p>`,
		`action="/disputes/reply"`, `aria-describedby="dispute-reply-help"`, "Reason given by the buyer",
		// A-80 copy, and with no staff key usable, no pointer to encrypted staff messages.
		`<a class="text-link" href="/canary#records">Stored unencrypted.</a> The buyer reads it on this order, as does every moderator and administrator, and the operator and anyone with a backup. Never include an address, real name or tracking number`,
		"No independent moderator or administrator has a usable PGP key")
	mustNotContain(t, vendorPage, "Upload image evidence in the")
	buyerPage := w.page("/order?id="+order, w.buyer, 200)
	mustNotContain(t, buyerPage, `action="/disputes/reply"`)
	mustContain(t, buyerPage, "The vendor may put one reply on record")
	css, err := os.ReadFile("web/static/orders.css")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`\.dispute-text \{[^}]*white-space:pre-wrap`).Match(css) {
		t.Fatal(".dispute-text is not white-space:pre-wrap")
	}

	// Image evidence stays on the order; usable staff keys allow separate private support.
	_, public := testPGPKey(t, "staff")
	if _, err := w.e.DB.Exec("UPDATE users SET pgp=$1 WHERE id=$2", public, w.modID); err != nil {
		t.Fatal(err)
	}
	mustContain(t, w.page("/order?id="+order, w.vendor, 200), "Upload image evidence in the")

	// The desk says the vendor has not replied yet, in the entry and on the resolve form.
	_, admin := w.e.user("reply_desk_admin_"+randomToken()[:6], "admin")
	desk := w.page("/moderator", admin, 200)
	mustContain(t, desk, `<p class="dispute-text">The parcel arrived empty.`+"\n", "UTC by the buyer</p>", "The vendor has not replied; only the buyer’s account is on record.")

	reply := "The parcel left sealed.\nThe courier photo shows the box intact."
	// Refusals: the opener 403, strangers and staff 404 (as on the order page), bad lengths 400. Nothing is written.
	w.expect("/disputes/reply", w.buyer, form("order_id", order, "reply", reply), 403, "You opened this dispute")
	w.expect("/disputes/reply", w.other, form("order_id", order, "reply", reply), 404, "")
	w.expect("/disputes/reply", w.mod, form("order_id", order, "reply", reply), 404, "")
	w.expect("/disputes/reply", w.vendor, form("order_id", order, "reply", "too short"), 400, "20–5000 characters")
	w.expect("/disputes/reply", w.vendor, form("order_id", order, "reply", strings.Repeat("x", 5001)), 400, "20–5000 characters")
	// CSRF and Origin are checked as on every form: a forged or cross-origin post is refused.
	for name, origin := range map[string]string{"forged token": "", "cross-origin": "https://attacker.example"} {
		v := form("order_id", order, "reply", reply)
		v.Set("csrf", w.e.A.csrf(w.vendor))
		if origin == "" {
			v.Set("csrf", w.e.A.csrf(w.buyer))
		}
		r := httptest.NewRequest("POST", "/disputes/reply", strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.AddCookie(&http.Cookie{Name: "session", Value: w.vendor})
		rec := httptest.NewRecorder()
		w.e.A.ServeHTTP(rec, r)
		if rec.Code != 403 {
			t.Fatalf("%s reply: status %d", name, rec.Code)
		}
	}
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order); n != events {
		t.Fatalf("refused replies wrote events: %d, want %d", n, events)
	}

	// Concurrent replies store one; the rest are 409. The race is not left to timing: the test holds the order row,
	// which the reply's INSERT waits on (its foreign key share-locks the order) after the request has read whether a
	// reply exists. Only when every request is waiting (on that INSERT, or on the dispute row a request holds) is the
	// order released: without the dispute row lock, every request would have read "no reply" by then.
	hold, err := w.e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hold.Exec("SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", order); err != nil {
		t.Fatal(err)
	}
	const n = 4
	results := make(chan agResult, n)
	for range n {
		go func() {
			r := w.post("/disputes/reply", w.vendor, form("order_id", order, "reply", reply))
			results <- agResult{r.Code, r.Body.String()}
		}()
	}
	for deadline := time.Now().Add(20 * time.Second); len(results) < n; time.Sleep(10 * time.Millisecond) {
		var waiting int
		if err = w.e.DB.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND (query LIKE 'SELECT status FROM disputes%' OR query LIKE 'INSERT INTO order_events%')").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d replies waiting on a lock", waiting, n)
		}
	}
	if err = hold.Rollback(); err != nil {
		t.Fatal(err)
	}
	rs := make([]agResult, 0, n)
	for range n {
		rs = append(rs, <-results)
	}
	if got := agTally(t, rs, 303, 409); got[303] != 1 {
		t.Fatalf("%d concurrent replies accepted", got[303])
	}
	if n := w.replyEvents(order); n != 1 {
		t.Fatalf("reply events %d, want 1", n)
	}
	var actor, note string
	if err := w.e.DB.QueryRow("SELECT actor_id,note FROM order_events WHERE order_id=$1 AND note LIKE 'Dispute reply: %'", order).Scan(&actor, &note); err != nil {
		t.Fatal(err)
	}
	if actor != w.vendorID || note != "Dispute reply: "+reply || w.state(order) != stateDisputed {
		t.Fatalf("actor=%s note=%q state=%s", actor, note, w.state(order))
	}
	// A second reply is 409 with nothing written.
	w.expect("/disputes/reply", w.vendor, form("order_id", order, "reply", "A second reply that must never be stored."), 409, "already replied")
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order); n != events+1 {
		t.Fatalf("events %d, want %d", n, events+1)
	}
	if !w.e.auditExact(w.vendorID, "Replied to the dispute on order "+shortID(order)) {
		t.Fatal("reply not audited (without its text)")
	}

	// Both parties and the desk see the reply with its role, line breaks kept; the form is gone.
	shown := `<p class="dispute-text">The parcel left sealed.` + "\n" + `The courier photo shows the box intact.</p>`
	for _, s := range []string{w.buyer, w.vendor} {
		p := w.page("/order?id="+order, s, 200)
		mustContain(t, p, "Reply from the vendor", shown, "Replied to the dispute; the reply is shown under Dispute.")
		mustNotContain(t, p, `action="/disputes/reply"`)
		mustContain(t, w.page("/disputes", s, 200), "Reply from the vendor", shown)
	}
	desk = w.page("/moderator", admin, 200)
	mustContain(t, desk, "Reply from the vendor", shown, "The vendor replied; the reply is shown above.")
	mustNotContain(t, desk, "has not replied")
	mustContain(t, w.page("/order?id="+order, w.mod, 200), "Reply from the vendor", shown)

	// The vendor can open a dispute too; the buyer then replies. After resolution no reply is offered (409).
	other, disputeID := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.vendor, "The buyer confirmed receipt by message and then stopped answering.")
	mustContain(t, w.page("/order?id="+other, w.buyer, 200), `action="/disputes/reply"`, "The vendor reads it on this order")
	w.expect("/resolve", admin, resolveConfirmed(form("id", disputeID, "outcome", "release", "resolution", "Delivery confirmed by the courier; release."), xmrListingPrice), 303, "")
	mustNotContain(t, w.page("/order?id="+other, w.buyer, 200), `action="/disputes/reply"`, "may put one reply")
	w.expect("/disputes/reply", w.buyer, form("order_id", other, "reply", reply), 409, "resolved")
	if n := w.replyEvents(other); n != 0 {
		t.Fatalf("reply stored after resolution: %d", n)
	}

	// A dispute with no recorded opener (a fixture or old import) offers no reply and refuses one.
	product := w.e.product(w.vendorID, "physical")
	bare := w.e.order(w.buyerID, product, "XMR", stateDisputed)
	if _, err := w.e.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Imported dispute with no opening event.')", randomToken(), bare); err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, w.page("/order?id="+bare, w.vendor, 200), `action="/disputes/reply"`)
	w.expect("/disputes/reply", w.vendor, form("order_id", bare, "reply", reply), 409, "")
	// No dispute at all: 409.
	noDispute := w.x.draft(w.x.digital(w.vendorID))
	w.expect("/disputes/reply", w.vendor, form("order_id", noDispute, "reply", reply), 409, "")
}

// A-162: the resolve form and desk entry name, per open dispute, the staff members each party messaged since it
// opened (count and latest time, from message metadata only), so a resolver consults them first.
func TestDeskShowsWhoHoldsDisputeEvidence(t *testing.T) {
	w := newDisputeOrderWorld(t)
	var modA string
	if err := w.e.DB.QueryRow("SELECT handle FROM users WHERE id=$1", w.modID).Scan(&modA); err != nil {
		t.Fatal(err)
	}
	modBID, modB := w.e.user("modb_"+randomToken()[:6], "moderator")
	var modBHandle string
	if err := w.e.DB.QueryRow("SELECT handle FROM users WHERE id=$1", modBID).Scan(&modBHandle); err != nil {
		t.Fatal(err)
	}
	keyA, publicA := testPGPKey(t, "staff a")
	keyB, publicB := testPGPKey(t, "staff b")
	for id, pub := range map[string]string{w.modID: publicA, modBID: publicB} {
		if _, err := w.e.DB.Exec("UPDATE users SET pgp=$1 WHERE id=$2", pub, id); err != nil {
			t.Fatal(err)
		}
	}
	send := func(session, to string, key *openpgp.Entity) string {
		t.Helper()
		body, err := encryptTo(key, "Evidence for the dispute: photos and the courier receipt.")
		if err != nil {
			t.Fatal(err)
		}
		w.expect("/messages", session, form("recipient", to, "body", body), 303, "")
		var stored string
		if err := w.e.DB.QueryRow("SELECT body FROM messages ORDER BY created DESC LIMIT 1").Scan(&stored); err != nil {
			t.Fatal(err)
		}
		return stored
	}

	// A message before the dispute opened does not count. Before it, the buyer reaches moderator A only on the public
	// staff list (A-543); once it opens, any current staff member.
	if _, err := w.e.DB.Exec("UPDATE users SET staff_hidden=false WHERE id=$1", w.modID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.DB.Exec("INSERT INTO settings(key,value) VALUES('staff_list_public','true') ON CONFLICT(key) DO UPDATE SET value=excluded.value"); err != nil {
		t.Fatal(err)
	}
	order := w.fundedFor(w.buyerID, w.buyer, w.vendorID, w.vendor)
	send(w.buyer, modA, keyA)
	if _, err := w.e.DB.Exec("UPDATE messages SET created=created-interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	w.openDispute(order, w.buyer, "The parcel never arrived at the agreed place.")
	ciphertext := send(w.buyer, modA, keyA)

	// Messages to someone who is not staff, from someone who is not a party, or from staff do not count either.
	if _, err := w.e.DB.Exec("INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'x',true),($4,$5,$6,'x',true),($7,$6,$2,'x',true),($8,$2,$5,'x',true)",
		randomToken(), w.buyerID, w.vendorID, randomToken(), w.otherID, w.modID, randomToken(), randomToken()); err != nil {
		t.Fatal(err)
	}

	// Staff member B (who holds nothing) sees that the buyer messaged A once and the vendor nobody, on the desk
	// entry and on /disputes; the resolve help says to consult the holders first. No ciphertext is rendered.
	holders := func(page string) string {
		t.Helper()
		m := regexp.MustCompile(`(?s)<div class="dispute-evidence".*?</div>`).FindString(page)
		if m == "" {
			t.Fatal("no evidence holders block")
		}
		return m
	}
	latest := `, latest \d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC\)`
	// entryFor returns the desk entry of order on page.
	entryFor := func(page, order string) string {
		t.Helper()
		for _, a := range strings.Split(page, `<article class="message">`) {
			if strings.Contains(a, "Order "+order+"<") {
				return a
			}
		}
		t.Fatalf("no entry for order %s", order)
		return ""
	}
	_, evidenceAdmin := w.e.user("evidence_admin_"+randomToken()[:6], "admin")
	const noHolders = "Neither party has messaged a current moderator or administrator since this dispute opened, nor one recorded as removed from staff after being messaged."
	for _, path := range []string{"/moderator", "/disputes"} {
		page := w.page(path, modB, 200)
		block := holders(page)
		if !regexp.MustCompile(`Buyer → ` + modA + ` \(1` + latest + `</li>`).MatchString(block) {
			t.Fatalf("%s: buyer holder missing: %s", path, block)
		}
		mustContain(t, block, "Vendor → none</li>")
		mustNotContain(t, page, "BEGIN PGP MESSAGE", ciphertext[40:120])
		adminPage := w.page(path, evidenceAdmin, 200)
		mustContain(t, entryFor(adminPage, order), "For any earlier evidence sent privately, ask the staff members listed under Private staff correspondence")
	}
	mustContain(t, w.page("/moderator", evidenceAdmin, 200), "For any earlier evidence sent privately, ask the staff members listed under Private staff correspondence")
	mustNotContain(t, w.page("/order?id="+order, modB, 200), "BEGIN PGP MESSAGE", ciphertext[40:120])

	// A second buyer message and a vendor message to B.
	send(w.buyer, modA, keyA)
	send(w.vendor, modBHandle, keyB)
	block := holders(w.page("/moderator", modB, 200))
	if !regexp.MustCompile(`Buyer → `+modA+` \(2`+latest+`</li>`).MatchString(block) ||
		!regexp.MustCompile(`Vendor → `+modBHandle+` \(1`+latest+`</li>`).MatchString(block) {
		t.Fatalf("holders: %s", block)
	}

	// The parties see no holders list; the dispute-contacts copy tells them what staff can see.
	for _, s := range []string{w.buyer, w.vendor} {
		mustNotContain(t, w.page("/disputes", s, 200), `class="dispute-evidence"`)
		mustContain(t, w.page("/order?id="+order, s, 200), "Other staff can see whom you contacted and when.")
	}

	// With neither party having messaged staff since the dispute opened, the form says so.
	_, _ = w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "Wrong item in the parcel, not the one listed.")
	mustContain(t, w.page("/moderator", evidenceAdmin, 200), noHolders)

	// A staff member who is a party to a dispute sees no holders list for it (whom the other party messaged).
	_, _ = w.disputedFor(w.modID, w.mod, w.vendorID, w.vendor, w.mod, "Bought as a customer; the parcel never came.")
	send(w.vendor, modBHandle, keyB)
	if n := strings.Count(w.page("/moderator", w.mod, 200), `class="dispute-evidence"`); n != 2 {
		t.Fatalf("party moderator sees %d holders lists, want 2 (not the dispute on its own order)", n)
	}
	if n := strings.Count(w.page("/moderator", modB, 200), `class="dispute-evidence"`); n != 3 {
		t.Fatalf("independent moderator sees %d holders lists, want 3", n)
	}

	// A-285: a counterparty who is staff holds no evidence. An administrator sells the listing and the buyer messages
	// that administrator (ordinary messages to the vendor): the independent moderator's block says "Buyer → none".
	adminVendorID, _ := w.e.user("a285_admin_vendor", "admin")
	adminOrder, _ := w.disputedFor(w.buyerID, w.buyer, adminVendorID, "", w.buyer, "The administrator's parcel never arrived.")
	if _, err := w.e.DB.Exec("INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'x',true)", randomToken(), w.buyerID, adminVendorID); err != nil {
		t.Fatal(err)
	}
	block = holders(entryFor(w.page("/moderator", modB, 200), adminOrder))
	mustContain(t, block, "Buyer → none</li>", "Vendor → none</li>")
	mustNotContain(t, block, "a285_admin_vendor")

	// A-295 (H3): the same for a buyer who is staff. A moderator buys from another vendor, who messages that
	// moderator (ordinary messages to the buyer): the independent moderator's block says "Vendor → none".
	staffBuyerID, staffBuyer := w.e.user("a295_staff_buyer", "moderator")
	vendor2ID, _ := w.e.user("a295_vendor", "vendor")
	staffOrder, _ := w.disputedFor(staffBuyerID, staffBuyer, vendor2ID, "", staffBuyer, "Bought as a customer; the parcel was empty.")
	w.e.mustExec("INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'x',true)", randomToken(), vendor2ID, staffBuyerID)
	block = holders(entryFor(w.page("/moderator", modB, 200), staffOrder))
	mustContain(t, block, "Buyer → none</li>", "Vendor → none</li>")
	mustNotContain(t, block, "a295_staff_buyer")

	// A-294: a holder removed from staff after being messaged is still listed, labelled "(no longer staff)", so the
	// resolver asks them. Messages to that account after the change do not count, nor do messages to an account whose
	// role changed without it ever being staff. The role changes go through the administrator's form and the host
	// command (their audit rows).
	_, admin := w.e.user("a294_admin", "admin")
	buyer3ID, buyer3 := w.e.user("a294_buyer", "buyer")
	vendor3ID, _ := w.e.user("a294_vendor", "vendor")
	holderID, _ := w.e.user("a294_holder", "moderator")
	plainID, _ := w.e.user("a294_plain", "buyer")
	adminHolderID, _ := w.e.user("a294_admin_holder", "admin")
	demotedOrder, _ := w.disputedFor(buyer3ID, buyer3, vendor3ID, "", buyer3, "The parcel was opened before it arrived.")
	message := func(from, to string) {
		t.Helper()
		w.e.mustExec("INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'x',true)", randomToken(), from, to)
	}
	message(buyer3ID, holderID)
	message(buyer3ID, adminHolderID)
	message(vendor3ID, holderID)
	message(vendor3ID, plainID)
	block = holders(entryFor(w.page("/moderator", modB, 200), demotedOrder))
	if !regexp.MustCompile(`Buyer → a294_admin_holder \(1`+latest+`, a294_holder \(1`+latest+`</li>`).MatchString(block) ||
		!regexp.MustCompile(`Vendor → a294_holder \(1`+latest+`</li>`).MatchString(block) {
		t.Fatalf("holder before the role change: %s", block)
	}
	w.expect("/admin", admin, form("action", "role", "password", testPassword, "role", "buyer", "handle", "a294_holder"), 303, "")
	w.expect("/admin", admin, form("action", "role", "password", testPassword, "role", "vendor", "handle", "a294_plain"), 303, "")
	if _, err := SetRole(context.Background(), w.e.DB, "a294_admin_holder", "buyer"); err != nil {
		t.Fatal(err)
	}
	message(buyer3ID, holderID)
	message(buyer3ID, adminHolderID)
	for _, path := range []string{"/moderator", "/disputes"} {
		entry := entryFor(w.page(path, modB, 200), demotedOrder)
		block = holders(entry)
		if !regexp.MustCompile(`Buyer → a294_admin_holder \(no longer staff\) \(1`+latest+`, a294_holder \(no longer staff\) \(1`+latest+`</li>`).MatchString(block) ||
			!regexp.MustCompile(`Vendor → a294_holder \(no longer staff\) \(1`+latest+`</li>`).MatchString(block) {
			t.Fatalf("%s: demoted holder not listed with the messages before the change, labelled: %s", path, block)
		}
		mustNotContain(t, block, "a294_plain")
		mustNotContain(t, entry, "For any earlier evidence sent privately, ask the staff members listed under Private staff correspondence")
		mustNotContain(t, entry, noHolders, "Neither party has messaged a moderator")
	}

	// A-304: the leave time is the latest leave row, not the first. A holder is demoted, promoted back, messaged
	// during the open dispute, then demoted again: the message was sent before the latest leave, so it counts.
	rejoinOrder, _ := w.disputedFor(buyer3ID, buyer3, vendor3ID, "", buyer3, "The parcel held a different item.")
	rejoinID, _ := w.e.user("a304_holder", "moderator")
	w.expect("/admin", admin, form("action", "role", "password", testPassword, "role", "buyer", "handle", "a304_holder"), 303, "")
	w.expect("/admin", admin, form("action", "role", "password", testPassword, "role", "moderator", "handle", "a304_holder"), 303, "")
	message(buyer3ID, rejoinID)
	w.expect("/admin", admin, form("action", "role", "password", testPassword, "role", "buyer", "handle", "a304_holder"), 303, "")
	block = holders(entryFor(w.page("/moderator", modB, 200), rejoinOrder))
	if !regexp.MustCompile(`Buyer → a304_holder \(no longer staff\) \(1` + latest + `</li>`).MatchString(block) {
		t.Fatalf("holder messaged between two leaves not listed from the latest leave: %s", block)
	}

	// A-280: messages to a staff member who is now suspended still count, labelled "(suspended)": that account
	// cannot sign in or answer until an administrator restores it.
	if _, err := w.e.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", w.modID); err != nil {
		t.Fatal(err)
	}
	// (The first dispute's buyer has since messaged the administrator who sells the other listing, too.)
	block = holders(w.page("/moderator", modB, 200))
	if !regexp.MustCompile(`Buyer → a285_admin_vendor \(1`+latest+`, `+modA+` \(suspended\) \(2`+latest+`</li>`).MatchString(block) ||
		!regexp.MustCompile(`Vendor → `+modBHandle+` \(2`+latest+`</li>`).MatchString(block) {
		t.Fatalf("suspended holder not labelled, or an active one labelled: %s", block)
	}
	// A-295 (S3): the label is on the vendor's line too. The vendor messages the suspended staff member.
	w.e.mustExec("INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES($1,$2,$3,'x',true)", randomToken(), w.vendorID, w.modID)
	vendorLine := regexp.MustCompile(`Vendor → .*</li>`).FindString(holders(w.page("/moderator", modB, 200)))
	if !regexp.MustCompile(`(→ |, )`+modA+` \(suspended\) \(1`+latest).MatchString(vendorLine) ||
		!regexp.MustCompile(`(→ |, )`+modBHandle+` \(2`+latest).MatchString(vendorLine) {
		t.Fatalf("vendor line: suspended holder not labelled, or an active one labelled: %s", vendorLine)
	}
}

// A-301: every staff desk load with an open dispute reads the role-change audit rows (A-294). audit_events grows with
// every sign-in, so that read must go through the partial index on role-change rows (migration 072), never a scan
// of the whole table. With sequential scans priced out, the plan names the index and no other access to the table.
func TestDisputeEvidenceReadsRoleChangesByIndex(t *testing.T) {
	e := newTestApp(t)
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SET LOCAL enable_seqscan=off"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query("EXPLAIN "+disputeEvidenceQuery, []string{})
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	// The index holds the role-change rows only: a full index would still be read end to end.
	var def string
	if err = tx.QueryRow("SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname='audit_events_role_change'").Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(def, "(user_id, created) WHERE (action ~~ 'Role changed from %'::text)") {
		t.Errorf("audit_events_role_change is not the partial index on role-change rows: %s", def)
	}
	// Each read of the table is an index scan of audit_events_role_change, or a bitmap heap scan fed by one.
	text := strings.Join(plan, "\n")
	if !strings.Contains(text, " audit_events_role_change") {
		t.Fatalf("the plan does not use audit_events_role_change:\n%s", text)
	}
	for _, line := range plan {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "->"))
		if strings.Contains(line, " on audit_events") && !strings.Contains(line, "audit_events_role_change") && !strings.HasPrefix(line, "Bitmap Heap Scan on audit_events ") {
			t.Errorf("audit_events read other than through the role-change index: %s\n%s", line, text)
		}
	}
}

// A-280: a resolve form loaded before the other party's reply said "has not replied". It carries what it showed
// (reply_seen); once a reply is on record, a form that did not show it is refused with 409 and the desk again,
// showing the reply and keeping the typed decision, and nothing changes. The reloaded form resolves.
func TestResolveRefusesFormLoadedBeforeReply(t *testing.T) {
	w := newDisputeOrderWorld(t)
	_, modB := w.e.user("a280_modb_"+randomToken()[:6], "moderator")
	_, admin := w.e.user("a280_admin_"+randomToken()[:6], "admin")
	order, disputeID := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The parcel never arrived at the agreed place.")
	if strings.Contains(w.page("/moderator", modB, 200), `action="/resolve"`) {
		t.Fatal("moderator can resolve an escrow dispute")
	}
	// A second open dispute (A-295, R6): its form on the 409 page keeps an empty decision.
	otherOrder, otherID := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The second parcel never arrived either.")
	// seen reads reply_seen from dispute id's resolve form.
	seen := func(page, id string) string {
		t.Helper()
		m := regexp.MustCompile(`name="id" value="`+id+`">\n<input type="hidden" name="reply_seen" value="([01])"`).FindAllStringSubmatch(page, -1)
		if len(m) != 1 {
			t.Fatalf("resolve forms with reply_seen for %s: %d", id, len(m))
		}
		return m[0][1]
	}
	for _, path := range []string{"/moderator", "/disputes"} {
		page := w.page(path, admin, 200)
		mustContain(t, page, "The vendor has not replied")
		if v := seen(page, disputeID); v != "0" {
			t.Fatalf("%s: reply_seen=%s before the reply", path, v)
		}
	}
	decision := "Refund: the courier has no record of delivery at the agreed place."
	stale := resolveConfirmed(form("id", disputeID, "outcome", "refund", "resolution", decision, "reply_seen", "0"), xmrListingPrice)
	events := w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order)

	reply := "The parcel was handed over at the agreed place; the courier has a signature."
	w.expect("/disputes/reply", w.vendor, form("order_id", order, "reply", reply), 303, "")
	events++
	refused := func(v url.Values) {
		t.Helper()
		r := w.post("/resolve", admin, v)
		body := r.Body.String()
		if r.Code != 409 {
			t.Fatalf("stale resolve: status %d %q", r.Code, body)
		}
		mustContain(t, body, `<div class="alert error" role="alert">The vendor replied to this dispute after the form was loaded. Read the reply below and decide again. Nothing was changed.</div>`,
			"Reply from the vendor", reply, "The vendor replied; the reply is shown above.", `aria-describedby="resolution-help-`+disputeID+`">`+"\n"+decision+"</textarea>",
			`aria-describedby="resolution-help-`+otherID+`">`+"\n</textarea>")
		if seen(body, disputeID) != "1" || seen(body, otherID) != "0" || strings.Count(body, decision) != 1 {
			t.Fatal("the 409 page does not show the reply in that dispute's form, or keeps the decision in another dispute's form")
		}
		if w.state(order) != stateDisputed || w.count("SELECT count(*) FROM disputes WHERE id=$1 AND status='Open' AND outcome='' AND resolution=''", disputeID) != 1 ||
			w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != events || w.count("SELECT count(*) FROM payouts WHERE order_id=$1", order) != 0 {
			t.Fatal("a refused resolve changed the dispute, the order or the payouts")
		}
	}
	refused(stale)
	// A post without the field counts as a form that showed no reply.
	missing := resolveConfirmed(form("id", disputeID, "outcome", "refund", "resolution", decision), xmrListingPrice)
	refused(missing)
	// Another resolver's desk is unaffected: it shows the reply and its form says so.
	if v := seen(w.page("/moderator", admin, 200), disputeID); v != "1" {
		t.Fatalf("reply_seen=%s after the reply", v)
	}
	// The reloaded form resolves.
	w.expect("/resolve", admin, resolveConfirmed(form("id", disputeID, "outcome", "refund", "resolution", decision, "reply_seen", "1"), xmrListingPrice), 303, "")
	if w.state(order) != stateResolved {
		t.Fatalf("state %s after resolving with the reloaded form", w.state(order))
	}

	// A-295 (R5): the reply is read after the dispute row is locked, not before. The test holds that row, which the
	// resolve's UPDATE waits on; while it waits, the reply is recorded and committed. A resolve that had read the
	// reply before taking the lock would then accept a form that never showed it.
	if v := seen(w.page("/moderator", admin, 200), otherID); v != "0" {
		t.Fatalf("reply_seen=%s before the second dispute's reply", v)
	}
	otherEvents := w.count("SELECT count(*) FROM order_events WHERE order_id=$1", otherOrder)
	hold, err := w.e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	if _, err = hold.Exec("SELECT 1 FROM disputes WHERE id=$1 FOR UPDATE", otherID); err != nil {
		t.Fatal(err)
	}
	result := make(chan agResult, 1)
	go func() {
		r := w.post("/resolve", admin, resolveConfirmed(form("id", otherID, "outcome", "release", "resolution", "Release: the courier record shows delivery.", "reply_seen", "0"), xmrListingPrice))
		result <- agResult{r.Code, r.Body.String()}
	}()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting int
		if err = w.e.DB.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'UPDATE disputes%'").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		if len(result) > 0 || time.Now().After(deadline) {
			t.Fatalf("the resolve is not waiting on the dispute row (waiting %d, answered %d)", waiting, len(result))
		}
	}
	if _, err = hold.Exec("INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,'disputed','disputed',$2,$3)", otherOrder, w.vendorID, disputeReplyPrefix+reply); err != nil {
		t.Fatal(err)
	}
	if err = hold.Commit(); err != nil {
		t.Fatal(err)
	}
	if r := <-result; r.code != 409 || !strings.Contains(r.body, "The vendor replied to this dispute after the form was loaded.") {
		t.Fatalf("resolve racing the reply: status %d %q", r.code, r.body)
	}
	if w.state(otherOrder) != stateDisputed || w.count("SELECT count(*) FROM disputes WHERE id=$1 AND status='Open' AND outcome='' AND resolution=''", otherID) != 1 ||
		w.count("SELECT count(*) FROM order_events WHERE order_id=$1", otherOrder) != otherEvents+1 || w.count("SELECT count(*) FROM payouts WHERE order_id=$1", otherOrder) != 0 {
		t.Fatal("a resolve racing the reply changed the dispute, the order or the payouts")
	}
}
