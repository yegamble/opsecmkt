package market

import (
	"net/http"
	"net/http/httptest"
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
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", stateShipped)
	reason := "The parcel arrived empty.\nThe seal on the box was broken."
	w.expect("/disputes", w.buyer, form("order_id", order, "reason", reason), 303, "")
	events := w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order)

	// The dispute-opened notice tells the vendor (who did not open it) that a reply exists; the buyer gets none.
	notice := "Order " + shortID(order) + " is now Disputed. You may put one reply to the dispute on record from the order page before a moderator decides."
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", w.vendorID, notice); n != 1 {
		t.Fatalf("vendor dispute notices with the reply hint: %d", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Order %'", w.buyerID); n != 0 {
		t.Fatalf("the opener got %d order notices", n)
	}

	// Line breaks in the reason survive (pre-wrap) on the order page and the desk; the vendor is offered the form,
	// the buyer (the opener) is not.
	vendorPage := w.page("/order?id="+order, w.vendor, 200)
	mustContain(t, vendorPage, `<p class="dispute-text">The parcel arrived empty.`+"\n"+`The seal on the box was broken.</p>`,
		`action="/disputes/reply"`, `aria-describedby="dispute-reply-help"`, "Reason given by the buyer",
		// A-80 copy, and with no staff key usable, no pointer to encrypted staff messages.
		`<a class="text-link" href="/canary#records">Stored unencrypted.</a> The buyer reads it on this order, as does every moderator and administrator, and the operator and anyone with a backup. Never include an address, real name or tracking number`,
		"No independent moderator or administrator has a usable PGP key")
	mustNotContain(t, vendorPage, "Send evidence encrypted first")
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

	// With a usable staff key the help points to encrypted staff messages first.
	_, public := testPGPKey(t, "staff")
	if _, err := w.e.DB.Exec("UPDATE users SET pgp=$1 WHERE id=$2", public, w.modID); err != nil {
		t.Fatal(err)
	}
	mustContain(t, w.page("/order?id="+order, w.vendor, 200), "Send evidence encrypted first")

	// The desk says the vendor has not replied yet, in the entry and on the resolve form.
	desk := w.page("/moderator", w.mod, 200)
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
	desk = w.page("/moderator", w.mod, 200)
	mustContain(t, desk, "Reply from the vendor", shown, "The vendor replied; the reply is shown above.")
	mustNotContain(t, desk, "has not replied")
	mustContain(t, w.page("/order?id="+order, w.mod, 200), "Reply from the vendor", shown)

	// The vendor can open a dispute too; the buyer then replies. After resolution no reply is offered (409).
	other := w.e.order(w.buyerID, product, "BTC", stateShipped)
	w.expect("/disputes", w.vendor, form("order_id", other, "reason", "The buyer confirmed receipt by message and then stopped answering."), 303, "")
	mustContain(t, w.page("/order?id="+other, w.buyer, 200), `action="/disputes/reply"`, "The vendor reads it on this order")
	var disputeID string
	if err := w.e.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", other).Scan(&disputeID); err != nil {
		t.Fatal(err)
	}
	w.expect("/resolve", w.mod, resolveConfirmed(form("id", disputeID, "outcome", "release", "resolution", "Delivery confirmed by the courier; release."), 0), 303, "")
	mustNotContain(t, w.page("/order?id="+other, w.buyer, 200), `action="/disputes/reply"`, "may put one reply")
	w.expect("/disputes/reply", w.buyer, form("order_id", other, "reply", reply), 409, "resolved")
	if n := w.replyEvents(other); n != 0 {
		t.Fatalf("reply stored after resolution: %d", n)
	}

	// A dispute with no recorded opener (a fixture or old import) offers no reply and refuses one.
	bare := w.e.order(w.buyerID, product, "BTC", stateDisputed)
	if _, err := w.e.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Imported dispute with no opening event.')", randomToken(), bare); err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, w.page("/order?id="+bare, w.vendor, 200), `action="/disputes/reply"`)
	w.expect("/disputes/reply", w.vendor, form("order_id", bare, "reply", reply), 409, "")
	// No dispute at all: 409.
	w.expect("/disputes/reply", w.vendor, form("order_id", w.e.order(w.buyerID, product, "BTC", stateShipped), "reply", reply), 409, "")
}

// A-162: the resolve form and desk entry name, per open dispute, the staff members each party messaged since it
// opened (count and latest time, from message metadata only), so a resolver consults them first.
func TestDeskShowsWhoHoldsDisputeEvidence(t *testing.T) {
	w := newOrderWorld(t)
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

	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", stateShipped)
	// A message before the dispute opened does not count.
	send(w.buyer, modA, keyA)
	if _, err := w.e.DB.Exec("UPDATE messages SET created=created-interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	w.expect("/disputes", w.buyer, form("order_id", order, "reason", "The parcel never arrived at the agreed place."), 303, "")
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
	for _, path := range []string{"/moderator", "/disputes"} {
		page := w.page(path, modB, 200)
		block := holders(page)
		if !regexp.MustCompile(`Buyer → ` + modA + ` \(1` + latest + `</li>`).MatchString(block) {
			t.Fatalf("%s: buyer holder missing: %s", path, block)
		}
		mustContain(t, block, "Vendor → none</li>")
		mustContain(t, page, "Before deciding, ask each staff member listed under Encrypted evidence holders")
		mustNotContain(t, page, "BEGIN PGP MESSAGE", ciphertext[40:120])
	}
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
		mustContain(t, w.page("/order?id="+order, s, 200), "Other moderators and administrators can see which staff member you messaged and when, never what.")
	}

	// With neither party having messaged staff since the dispute opened, the form says so.
	quiet := w.e.order(w.buyerID, product, "BTC", stateShipped)
	w.expect("/disputes", w.buyer, form("order_id", quiet, "reason", "Wrong item in the parcel, not the one listed."), 303, "")
	mustContain(t, w.page("/moderator", modB, 200), "Neither party has messaged a moderator or administrator since this dispute opened.")

	// A staff member who is a party to a dispute sees no holders list for it (whom the other party messaged).
	own := w.e.order(w.modID, product, "BTC", stateShipped)
	w.expect("/disputes", w.mod, form("order_id", own, "reason", "Bought as a customer; the parcel never came."), 303, "")
	send(w.vendor, modBHandle, keyB)
	if n := strings.Count(w.page("/moderator", w.mod, 200), `class="dispute-evidence"`); n != 2 {
		t.Fatalf("party moderator sees %d holders lists, want 2 (not the dispute on its own order)", n)
	}
	if n := strings.Count(w.page("/moderator", modB, 200), `class="dispute-evidence"`); n != 3 {
		t.Fatalf("independent moderator sees %d holders lists, want 3", n)
	}
}
