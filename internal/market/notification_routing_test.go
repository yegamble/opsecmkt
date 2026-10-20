package market

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Assert actual event writers enqueue exactly their frontend recipients, including audience and destination.
func requireQueuedRouting(e *testEnv, bodyPattern string, expected map[string]string) {
	e.t.Helper()
	got := agStrings(e.t, e, `SELECT n.user_id||':'||n.audience FROM xmpp_outbox q JOIN notifications n ON n.id=q.notification_id
		JOIN users u ON u.id=n.user_id WHERE n.body LIKE $1 AND q.destination=u.xmpp AND q.preference_version=u.xmpp_version`, bodyPattern)
	want := make([]string, 0, len(expected))
	for id, audience := range expected {
		want = append(want, id+":"+audience)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		e.t.Fatalf("queued recipients for %q = %v, want %v", bodyPattern, got, want)
	}
	if n := agInt(e, `SELECT count(*) FROM xmpp_outbox q JOIN notifications n ON n.id=q.notification_id WHERE n.body LIKE $1`, bodyPattern); n != len(want) {
		e.t.Fatalf("outbox has extra or incorrectly addressed delivery for %q: %d, want %d", bodyPattern, n, len(want))
	}
}

func TestNotificationRoutingVendorRequestsAndBondsAreAdministratorOnly(t *testing.T) {
	e := newTestApp(t)
	admin := e.setup()
	adminID := agStr(e, "SELECT id FROM users WHERE handle='admin_user'")
	secondAdmin, _ := e.user("routing_other_admin", "admin")
	modID, mod := e.user("routing_moderator", "moderator")
	vendorID, vendor := e.user("routing_vendor", "vendor")
	strangerID, stranger := e.user("routing_stranger", "buyer")
	applicantID, applicant := e.user("routing_applicant", "buyer")
	inactive := []string{}
	for _, field := range []string{"suspended_at", "closed_at", "retention_restricted_at"} {
		id, _ := e.user("routing_inactive_"+field, "admin")
		agExec(e, "UPDATE users SET "+field+"=now() WHERE id=$1", id)
		inactive = append(inactive, id)
	}
	enableXMPPDelivery(e)
	for _, id := range append([]string{adminID, secondAdmin, modID, vendorID, strangerID, applicantID}, inactive...) {
		xmppOptIn(e, id)
	}
	setVendorPolicy(e, halfXMR, "manual", "Test bond instructions.")
	e.check(e.do("POST", "/account/vendor-application", applicant, url.Values{"action": {"apply"}}), 303)
	app := latestVendorApp(e, applicantID)
	for _, id := range []string{adminID, secondAdmin} {
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND audience='admin' AND body LIKE 'New vendor application from routing_applicant:%'", id); n != 1 {
			t.Fatalf("admin %s application notices=%d, want 1", id, n)
		}
	}
	requireQueuedRouting(e, "New vendor application from routing_applicant:%", map[string]string{adminID: "admin", secondAdmin: "admin"})
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"bond"}}, 303)
	for _, id := range []string{adminID, secondAdmin} {
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND audience='admin' AND body LIKE 'Vendor application from routing_applicant: bond %waiting for administrator approval.%'", id); n != 1 {
			t.Fatalf("admin %s bond-ready notices=%d, want 1", id, n)
		}
	}
	requireQueuedRouting(e, "Vendor application from routing_applicant: bond %", map[string]string{adminID: "admin", secondAdmin: "admin"})
	requireQueuedRouting(e, "An administrator recorded your vendor bond%", map[string]string{applicantID: "personal"})
	if !hasNote(e, applicantID, "Your application now waits for administrator approval.") {
		t.Fatal("applicant did not receive their personal bond status")
	}
	for _, id := range append([]string{modID, vendorID, strangerID, applicantID}, inactive...) {
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND audience='admin'", id); n != 0 {
			t.Fatalf("admin-only notification leaked to %s: %d", id, n)
		}
	}
	for _, session := range []string{mod, vendor, stranger, applicant} {
		body := e.body("GET", "/notifications", session, nil, 200)
		mustNotContain(t, body, "New vendor application from", "Vendor application from routing_applicant:")
	}
	// A replay cannot manufacture a second bond-ready event.
	adminVendorPost(e, admin, "/admin/vendor-applications", url.Values{"id": {app.ID}, "action": {"bond"}}, 409)
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE audience='admin' AND body LIKE 'Vendor application from routing_applicant: bond %'"); n != 2 {
		t.Fatalf("repeated bond step duplicated administrator notices: %d", n)
	}
}

func TestNotificationRoutingPaymentReviewDoesNotBroadcastToModerators(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("routing_payment_admin", "admin")
	modID, mod := e.user("routing_payment_mod", "moderator")
	vendorID, vendor := e.user("routing_payment_vendor", "vendor")
	strangerID, stranger := e.user("routing_payment_stranger", "buyer")
	enableXMPPDelivery(e)
	for _, id := range []string{adminID, modID, vendorID, strangerID} {
		xmppOptIn(e, id)
	}
	order := e.order(modID, e.product(vendorID, "physical"), "XMR", statePaid)
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(t.Context(), orderQuery+" WHERE o.id=$1", order))
	if err != nil {
		t.Fatal(err)
	}
	if err = e.A.flagOrder(t.Context(), tx, &o, "Routing administrator payment anomaly.", "Your own order needs payment review."); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND audience='admin' AND body LIKE 'Payment review needed for order%'", adminID); n != 1 {
		t.Fatalf("administrator payment notices=%d, want 1", n)
	}
	for _, id := range []string{modID, vendorID} {
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND audience='personal' AND body='Your own order needs payment review.'", id); n != 1 {
			t.Fatalf("party %s personal payment notices=%d, want 1", id, n)
		}
	}
	for _, id := range []string{modID, vendorID, strangerID} {
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND audience='admin'", id); n != 0 {
			t.Fatalf("administrator payment anomaly leaked to %s", id)
		}
	}
	mustContain(t, e.body("GET", "/notifications", admin, nil, 200), "Routing administrator payment anomaly.")
	for _, session := range []string{mod, vendor, stranger} {
		mustNotContain(t, e.body("GET", "/notifications", session, nil, 200), "Routing administrator payment anomaly.")
	}
	requireQueuedRouting(e, "Payment review needed for order%", map[string]string{adminID: "admin"})
	requireQueuedRouting(e, "Your own order needs payment review.", map[string]string{modID: "personal", vendorID: "personal"})
}

func TestNotificationRoutingDisputeLifecycleAndResolutionPermissions(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	adminID, admin := e.user("routing_dispute_admin", "admin")
	otherAdminID, otherAdmin := e.user("routing_dispute_other_admin", "admin")
	inactiveMod, _ := e.user("routing_dispute_inactive", "moderator")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", inactiveMod)
	enableXMPPDelivery(e)
	for _, id := range []string{adminID, otherAdminID, w.modID, w.buyerID, w.vendorID, w.otherID, inactiveMod} {
		xmppOptIn(e, id)
	}
	order, dispute := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "Private dispute reason that must stay on the dispute record.")
	for _, id := range []string{adminID, otherAdminID, w.modID} {
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND audience='dispute' AND order_id=$2", id, order); n != 1 {
			t.Fatalf("observer %s opening notices=%d, want 1", id, n)
		}
	}
	requireQueuedRouting(e, "Dispute opened on order "+shortID(order)+"%", map[string]string{adminID: "dispute", otherAdminID: "dispute", w.modID: "dispute"})
	requireQueuedRouting(e, "Order "+shortID(order)+" is now Disputed.%", map[string]string{w.vendorID: "personal"})
	// Moderators may follow the dispute, while an escrow decision still requires an administrator.
	resolution := payoutConfirmed(url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"The delivered item differs from the agreed description."}, "password": {testPassword}, "reply_seen": {"1"}}, xmrListingPrice)
	e.check(e.do("POST", "/resolve", w.mod, cloneValues(resolution)), 403)
	if state := w.state(order); state != stateDisputed {
		t.Fatalf("moderator changed escrow dispute state: %s", state)
	}
	const privateReply = "Private vendor evidence must remain on the dispute record."
	e.check(e.do("POST", "/disputes/reply", w.vendor, url.Values{"order_id": {order}, "reply": {privateReply}}), 303)
	body := "Dispute on order " + shortID(order) + " has a new reply from the vendor. Review it on the order page."
	for _, id := range []string{w.buyerID, adminID, otherAdminID, w.modID} {
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2 AND order_id=$3", id, body, order); n != 1 {
			t.Fatalf("recipient %s reply notices=%d, want 1", id, n)
		}
	}
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", w.vendorID, body); n != 0 {
		t.Fatal("replying vendor received its own reply alert")
	}
	requireQueuedRouting(e, body, map[string]string{w.buyerID: "personal", adminID: "dispute", otherAdminID: "dispute", w.modID: "dispute"})
	e.check(e.do("POST", "/disputes/reply", w.vendor, url.Values{"order_id": {order}, "reply": {privateReply}}), 409)
	e.check(e.do("POST", "/resolve", admin, resolution), 303)
	closed := "Dispute on order " + shortID(order) + " is now " + disputeOutcomes["refund"] + ". Review the order history."
	for _, id := range []string{otherAdminID, w.modID} {
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2 AND audience='dispute'", id, closed); n != 1 {
			t.Fatalf("observer %s closure notices=%d, want 1", id, n)
		}
	}
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", adminID, closed); n != 0 {
		t.Fatal("resolving administrator received its own staff closure alert")
	}
	requireQueuedRouting(e, closed, map[string]string{otherAdminID: "dispute", w.modID: "dispute"})
	requireQueuedRouting(e, "Order "+shortID(order)+" is now Resolved%", map[string]string{w.buyerID: "personal", w.vendorID: "personal"})
	if n := agInt(e, `SELECT count(*) FROM xmpp_outbox q JOIN notifications n ON n.id=q.notification_id
		WHERE n.body LIKE '%Private dispute reason%' OR n.body LIKE '%Private vendor evidence%'`); n != 0 {
		t.Fatal("private dispute evidence was copied to an external alert")
	}
	for _, id := range []string{w.otherID, inactiveMod} {
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND order_id=$2", id, order); n != 0 {
			t.Fatalf("dispute event leaked to unrelated/inactive recipient %s", id)
		}
	}
	for _, session := range []string{w.mod, otherAdmin} {
		page := e.body("GET", "/notifications", session, nil, 200)
		mustContain(t, page, "new reply from the vendor", "Review the order history.")
		mustNotContain(t, page, privateReply, "Private dispute reason")
	}
}

func TestNotificationRoutingDemotionHidesStaffAlertsButKeepsPersonalNotices(t *testing.T) {
	e := newTestApp(t)
	adminID, session := e.user("routing_demotion_admin", "admin")
	tx, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = notifyAdmins(t.Context(), tx, "Private administrator routing marker."); err != nil {
		t.Fatal(err)
	}
	if _, err = insertScopedNotification(t.Context(), tx, adminID, "Private dispute routing marker.", "", "dispute"); err != nil {
		t.Fatal(err)
	}
	if _, err = insertNotification(t.Context(), tx, adminID, "Your personal routing marker.", ""); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", "moderator", "buyer", "vendor"} {
		agExec(e, "UPDATE users SET role=$2 WHERE id=$1", adminID, role)
		page := e.body("GET", "/notifications", session, nil, 200)
		if strings.Contains(page, "Private administrator routing marker.") != (role == "admin") {
			t.Fatalf("admin alert visibility is wrong for role %s", role)
		}
		if strings.Contains(page, "Private dispute routing marker.") != (role == "admin" || role == "moderator") {
			t.Fatalf("dispute alert visibility is wrong for role %s", role)
		}
		mustContain(t, page, "Your personal routing marker.")
	}
}

func TestNotificationRoutingMessagesOnlyReachTheirAddressee(t *testing.T) {
	e := newTestApp(t)
	enableXMPPDelivery(e)
	for _, role := range []string{"buyer", "vendor", "moderator", "admin"} {
		recipientID, recipient := e.user("routing_message_"+role, role)
		senderID, sender := e.user("routing_sender_"+role, "admin")
		e.enableOrderPGP(senderID)
		xmppOptIn(e, recipientID)
		xmppOptIn(e, senderID)
		key, public := testPGPKey(t, "routing-message-"+role)
		e.setKey(recipientID, public)
		body, err := encryptTo(key, "Private message content for one recipient only.")
		if err != nil {
			t.Fatal(err)
		}
		e.check(e.do("POST", "/messages", sender, url.Values{"recipient": {"routing_message_" + role}, "body": {body}}), 303)
		notice := "New message from routing_sender_" + role
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE body=$1", notice); n != 1 {
			t.Fatalf("%s message created %d notifications, want only the addressee", role, n)
		}
		if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2 AND audience='personal'", recipientID, notice); n != 1 {
			t.Fatalf("%s recipient did not receive personal message alert", role)
		}
		page := e.body("GET", "/notifications", recipient, nil, 200)
		mustContain(t, page, notice)
		mustNotContain(t, page, "Private message content", "-----BEGIN PGP MESSAGE-----")
		requireQueuedRouting(e, notice, map[string]string{recipientID: "personal"})
	}
}

func TestNotificationRoutingConfirmedPaymentAndShippingForModeratorOwnOrder(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	adminID, _ := e.user("routing_shipment_admin", "admin")
	otherModID, _ := e.user("routing_shipment_other_mod", "moderator")
	enableXMPPDelivery(e)
	for _, id := range []string{w.modID, w.vendorID, w.otherID, w.buyerID, adminID, otherModID} {
		xmppOptIn(e, id)
	}
	// A real fake-wallet deposit passes through confirmation, the watcher and the paid transition.
	order := w.fundedPhysicalFor(w.modID, w.mod, w.vendorID, w.vendor)
	requireQueuedRouting(e, "Order "+shortID(order)+" is now Paid%", map[string]string{w.modID: "personal", w.vendorID: "personal"})
	page := e.body("GET", orderPage(order), w.vendor, nil, 200)
	nonce := renderedFormNonce(t, page, "/escrow-xmr/ship")
	e.check(e.do("POST", "/escrow-xmr/ship", w.vendor, url.Values{"order_id": {order}, "address": {testXMRAddr("routing-shipment-claim", true)}, "password": {testPassword}, formNonceField: {nonce}}), 303)
	if state := w.state(order); state != stateShipped {
		t.Fatalf("shipping form did not transition order: %s", state)
	}
	requireQueuedRouting(e, "Order "+shortID(order)+" is now Shipped%", map[string]string{w.modID: "personal"})
	if n := agInt(e, `SELECT count(*) FROM xmpp_outbox q JOIN notifications n ON n.id=q.notification_id
		WHERE n.order_id=$1 AND n.user_id NOT IN ($2,$3)`, order, w.modID, w.vendorID); n != 0 {
		t.Fatalf("own-order payment/shipping leaked to unrelated staff or users: %d", n)
	}
}

func TestNotificationRoutingConfirmedVendorBondQueuesOnlyApplicantAndAdministrators(t *testing.T) {
	x := newXMREscrowEnv(t)
	e := x.testEnv
	setVendorPolicy(e, "250000000000", "manual", "Test bond instructions.")
	otherAdmin, _ := e.user("routing_bond_admin", "admin")
	modID, _ := e.user("routing_bond_mod", "moderator")
	applicantID, applicant := e.user("routing_bond_applicant", "buyer")
	enableXMPPDelivery(e)
	for _, id := range []string{x.admin, x.vendor, x.buyer, otherAdmin, modID, applicantID} {
		xmppOptIn(e, id)
	}
	e.check(e.do("POST", "/account/vendor-application", applicant, url.Values{"action": {"apply"}}), 303)
	app := latestVendorApp(e, applicantID)
	x.mustPoll()
	deposit := hex64("routing-confirmed-bond")
	x.fake.payAddress(x.mustBondAddress(app.ID), deposit, testBondPico, 9)
	x.mustPoll()
	requireQueuedRouting(e, "Vendor bond received from routing_bond_applicant:%", nil)
	x.fake.setConfs(deposit, 10)
	x.mustPoll()
	if state := x.appState(app.ID); state != "awaiting_approval:xmr" {
		t.Fatalf("confirmed bond did not wait for approval: %s", state)
	}
	admins := map[string]string{x.admin: "admin", otherAdmin: "admin"}
	requireQueuedRouting(e, "Vendor bond received from routing_bond_applicant:%waiting for administrator approval.%", admins)
	requireQueuedRouting(e, "Your vendor bond%was received on the Monero network%", map[string]string{applicantID: "personal"})
	x.mustPoll()
	requireQueuedRouting(e, "Vendor bond received from routing_bond_applicant:%", admins)
}
