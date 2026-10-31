package market

import (
	"html"
	"testing"
)

// A-716 (BUY98-1): signed-out /register says, beside the PGP field and under both sign-in policies, what the key shows
// and to whom, and asks for a key made only for this market.
func TestRegistrationCopyNamesWhoSeesThePGPKey(t *testing.T) {
	e := newTestApp(t)
	e.user("regkey_buyer", "buyer")
	const warning = "Every vendor you trade with or message sees this key, including its user IDs (names and email addresses) and creation date."
	const advice = "Use a new key made only for this market, with a pseudonym and no real email."
	for _, policy := range []string{"true", "false"} {
		agExec(e, "UPDATE settings SET value=$2 WHERE key=$1", settingAllowLoginWithoutPGP, policy)
		page := html.UnescapeString(e.body("GET", "/register", "", nil, 200))
		mustContain(t, page, warning, advice, `aria-describedby="pgp-visibility-help"`, `id="pgp-visibility-help"`)
	}
}

// A-716 (BUY98-2): the buyer's shipping panel on a paid physical order gives the steps of an encryption that writes no
// plaintext file, and says what the vendor can do with the address. /messages' command gains the import step.
func TestOrderCopyGivesShippingAddressSteps(t *testing.T) {
	x := newXMREscrowEnv(t)
	product := x.physical(x.vendor)
	order := x.order(x.buyer, product, "XMR", statePaid)
	buyer := x.page("/order?id="+order, x.buyerSess)
	mustContain(t, buyer, "<code>gpg --import vendor.asc</code>", "<code>gpg --armor --encrypt --recipient &lt;fingerprint&gt;</code>", "<code>-----BEGIN PGP MESSAGE-----</code>")
	mustContain(t, html.UnescapeString(buyer),
		"Send your shipping address",
		"matches the fingerprint shown below",
		"end with Ctrl+D",
		"no plaintext file is written",
		"Or use your PGP application's import, then encrypt.",
		"reads your address and can keep it; the market cannot delete their copy.",
	)
	vendor := x.page("/order?id="+order, x.vendorSess)
	mustNotContain(t, vendor, "gpg --import vendor.asc", "the market cannot delete their copy")

	agExec(x.testEnv, "UPDATE users SET handle='ship_copy_vendor' WHERE id=$1", x.vendor)
	messages := x.page("/messages?to=ship_copy_vendor", x.buyerSess)
	mustContain(t, messages, "<code>gpg --import key.asc</code>", "<code>gpg --armor --encrypt --recipient &lt;fingerprint&gt; message.txt</code>")
}

// A-716 (BUY98-5): the review form says, before the button, how public and how final a review is.
func TestOrderCopyReviewIsPublicAndFinal(t *testing.T) {
	x := newXMREscrowEnv(t)
	order := x.order(x.buyer, x.product, "XMR", stateCompleted)
	page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	mustContain(t, page, "Published at once to anyone who can see this listing, on the listing and vendor pages as a verified purchase", "You cannot edit or remove it.")
}

// A-716 (BUY98-3): the dispute evidence panel follows retention on and off, as the correspondence panel does, and names
// the operator and backup holders.
func TestDisputeEvidenceCopyFollowsRetention(t *testing.T) {
	p, _, _ := evidenceEnv(t)
	const holders = "The operator and anyone with a backup can also see these images and their descriptions."
	const on = "These images are scheduled for deletion with the order’s private content, 30 days after financial closure, unless a scoped preservation duty applies."
	const off = "Automatic retention is off; the server keeps these images until the operator removes them."
	page := html.UnescapeString(p.page("/order?id="+p.order, p.buyerSess))
	mustContain(t, page, holders, off, "If the operator runs a moderation model, it can read a description, never the image")
	mustNotContain(t, page, on)
	if err := p.A.initRetention(t.Context(), retentionConfig{Enabled: true, StateDir: t.TempDir() + "/private-retention"}); err != nil {
		t.Fatal(err)
	}
	page = html.UnescapeString(p.page("/order?id="+p.order, p.buyerSess))
	mustContain(t, page, holders, on)
	mustNotContain(t, page, off)
}

// A-716 (BUY98-4): the XMPP opt-in names both servers, what alerts carry, the direct connection and that the market
// stores the address.
func TestXMPPCopyNamesServersAndContents(t *testing.T) {
	e := newTestApp(t)
	_, sess := e.user("xmpp_copy", "buyer")
	page := html.UnescapeString(e.body("GET", "/account", sess, nil, 200))
	mustContain(t, page,
		"the market's sending XMPP server, which the operator chooses, and your XMPP provider",
		"Alerts include the handles you trade with and order references.",
		"The market connects to its sending server directly, not through Tor, even when the market itself runs on Tor.",
		"Use a dedicated address made only for this market.",
		"The market stores this address with your delivery preferences",
	)
}

// A-716 (PRIV98-2, PRIV98-4): /canary#records names exactly what a model on the operator's infrastructure may read, what
// it never reads and that it decides nothing, and its escrow entry covers the digital deadlines.
func TestCanaryCopyNamesModelInputsAndDigitalDeadlines(t *testing.T) {
	e := newTestApp(t)
	e.user("canary_copy", "buyer")
	page := html.UnescapeString(e.body("GET", "/canary", "", nil, 200))
	mustContain(t, page,
		"Only if the operator runs a moderation model on its own infrastructure",
		"a listing's title, description, category, region and kind",
		"an upload's description, including the private description of dispute evidence",
		"a dispute's reason and resolution",
		"a review's body and rating",
		"a payment flag's disposition, without transaction IDs or addresses",
		"It never reads direct messages, support tickets, order correspondence, deliveries or image pixels.",
		"It only proposes for staff and decides nothing itself.",
		"For a digital order's escrow also its delivery deadline, after which the buyer may cancel it if it is not delivered, and the end of its dispute window",
	)
}

// A-716 (PRIV98-2): the dispute form points at what a model may read.
func TestDisputeFormCopyPointsAtTheModel(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.order(x.buyer, x.product, "XMR", statePaid)
	mustContain(t, html.UnescapeString(x.page("/disputes", x.buyerSess)), "If the operator runs a moderation model, it can read this reason and only proposes for staff")
}

// A-716 (PRIV98-5): both key-replacement forms carry the guide's warning about the audit-exported note.
func TestKeyReplacementCopyWarnsAboutTheNote(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("note_copy_admin", "admin")
	e.user("note_copy_target", "buyer")
	const warning = "Do not put the holder's real-world identity, contact details or other private evidence in it."
	for _, path := range []string{"/admin/users?handle=note_copy_target", "/admin/users"} {
		form := adminForm(t, e.body("GET", path, admin, nil, 200), "/admin/reset-factors", "")
		mustContain(t, html.UnescapeString(form), "signed audit export", warning)
	}
}
