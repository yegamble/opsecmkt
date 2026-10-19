package market

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A-547: the vendor bond paid automatically in XMR (vendor_bond.go). DB integration tests against the simulated
// wallet-rpc and monerod of escrow_xmr_fake_test.go (simulated RPC, not chain evidence; TestXMREscrowRegtestBond is the
// real-chain leg). The bond wallet is a second wallet file on the same fake.

const (
	testBondPico   = 250_000_000_000 // 0.25 XMR
	testBondWallet = "bond-01"
)

// newVendorBondEnv is newXMREscrowEnv with a vendor policy of a 0.25 XMR bond and approval ("manual" or "automatic"),
// and an administrator with a session for the decision forms.
func newVendorBondEnv(t *testing.T, approval string) (*xmrEscrowEnv, string) {
	t.Helper()
	x := newXMREscrowEnv(t)
	setVendorPolicy(x.testEnv, strconv.FormatInt(testBondPico, 10), approval, "Pay the bond by hand and message the staff.")
	_, adminSess := x.user("bondadmin_"+randomToken()[:6], "admin")
	return x, adminSess
}

// bondApplicant is a new buyer account that applied to sell from /account: its id, session and application.
func (x *xmrEscrowEnv) bondApplicant() (uid, sess, app string) {
	x.t.Helper()
	uid, sess = x.user("bond_"+randomToken()[:8], "buyer")
	x.check(x.do("POST", "/account/vendor-application", sess, url.Values{"action": {"apply"}}), 303)
	app = latestVendorApp(x.testEnv, uid).ID
	if app == "" {
		x.t.Fatal("no application")
	}
	return uid, sess, app
}

// bondAddress is application app's bond address, "" when it has none.
func (x *xmrEscrowEnv) bondAddress(app string) string {
	x.t.Helper()
	if got := agStrings(x.t, x.testEnv, "SELECT address FROM vendor_bond_accounts WHERE application_id=$1", app); len(got) == 1 {
		return got[0]
	}
	return ""
}

// mustBondAddress is application app's bond address, after a pass made it.
func (x *xmrEscrowEnv) mustBondAddress(app string) string {
	x.t.Helper()
	addr := x.bondAddress(app)
	if addr == "" {
		x.t.Fatal("no bond address was issued")
	}
	return addr
}

func (x *xmrEscrowEnv) appState(app string) string {
	return agStr(x.testEnv, "SELECT state||':'||coalesce(bond_received_how,'') FROM vendor_applications WHERE id=$1", app)
}

func (x *xmrEscrowEnv) accountPage(sess string) string {
	x.t.Helper()
	return html.UnescapeString(x.body("GET", "/account", sess, nil, 200))
}

// adminDecide posts an administrator's decision on application app.
func (x *xmrEscrowEnv) adminDecide(adminSess, app, action string) {
	x.t.Helper()
	adminVendorPost(x.testEnv, adminSess, "/admin/vendor-applications", url.Values{"id": {app}, "action": {action}}, 303)
}

func (x *xmrEscrowEnv) adminsTold(part string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role='admin' AND strpos(n.body,$1)>0", part)
}

const (
	bondLateNotice   = "stopped waiting for its bond"
	bondLockedNotice = "has an unlock time"
	bondReorgNotice  = "Nothing was changed: the application"
	bondOverNotice   = "over the bond is kept"
	bondRowless      = "account(s) the market has no record of"
	bondUnclaimed    = "belongs to no vendor application"
)

// The bond is paid in full, short, over, late or locked: only counted deposits (at least the escrow threshold, no
// unlock time, while the application waits for its bond) record it, and every other deposit is kept and flagged once.
func TestVendorBondFundsApplication(t *testing.T) {
	t.Run("exact", func(t *testing.T) {
		x, adminSess := newVendorBondEnv(t, "manual")
		uid, sess, app := x.bondApplicant()
		page := x.accountPage(sess)
		mustContain(t, page, "preparing a bond address")
		x.mustPoll()
		addr := x.mustBondAddress(app)
		if name, account := x.fake.accountOf(addr); name != testBondWallet || account != 1 {
			t.Fatalf("bond address is account %d of %s", account, name)
		}
		if got := x.wallets(); slices.Contains(got, testBondWallet+":active") || agStr(x.testEnv, "SELECT state FROM xmr_bond_wallets") != "sealed" {
			t.Fatalf("wallets %v", got)
		}
		file, _ := x.fake.file(testBondWallet)
		if file.password != xmrWalletPassword(testXMRSecret, testBondWallet) || x.unsealBond() != file.mnemonic {
			t.Fatal("the bond wallet's password or sealed seed is not the market's")
		}
		page = x.accountPage(sess)
		terms, at := strings.Index(page, "never sends coins back"), strings.Index(page, addr)
		if terms < 0 || at < 0 || terms > at || strings.Index(page, "a non-refundable fee") > at {
			t.Fatalf("the non-refundable and over-payment notice must come before the address (%d, %d)", terms, at)
		}
		mustContain(t, page, "0 XMR with at least 10 confirmations", "Still to pay</dt><dd>0.25 XMR")
		if strings.Contains(page, "Pay the bond by hand") {
			t.Fatal("the manual instructions show beside the bond address")
		}
		tx := hex64("bond exact")
		x.fake.payAddress(addr, tx, testBondPico, 0)
		x.mustPoll()
		mustContain(t, x.accountPage(sess), "Waiting for confirmations</dt><dd>0.25 XMR, 0 of 10 confirmations")
		x.fake.setConfs(tx, 9)
		x.mustPoll()
		if got := x.appState(app); got != "awaiting_bond:" {
			t.Fatalf("at 9 confirmations: %s", got)
		}
		x.fake.setConfs(tx, 10)
		x.mustPoll()
		if got := x.appState(app); got != "awaiting_approval:xmr" {
			t.Fatalf("at 10 confirmations: %s", got)
		}
		if agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits WHERE credited AND NOT flagged") != 1 || userRole(x.testEnv, uid) != "buyer" {
			t.Fatal("the deposit was not credited, or the manual application approved itself")
		}
		if !hasAudit(x.testEnv, uid, "received on the Monero network: 0.25 XMR in 1 deposit(s) with at least 10 confirmations") ||
			!hasNote(x.testEnv, uid, "now waits for administrator approval") {
			t.Fatalf("audit %v notes %v", x.audits(uid), x.notes(uid))
		}
		admin := html.UnescapeString(x.body("GET", "/admin/vendors", adminSess, nil, 200))
		mustContain(t, admin, addr, "0.25 XMR received with at least 10 confirmations", "detected on the Monero network")
		x.adminDecide(adminSess, app, "approve")
		if userRole(x.testEnv, uid) != "vendor" {
			t.Fatal("approval did not make a vendor")
		}
	})
	t.Run("automatic", func(t *testing.T) {
		x, _ := newVendorBondEnv(t, "automatic")
		uid, _, app := x.bondApplicant()
		x.mustPoll()
		x.fake.payAddress(x.mustBondAddress(app), hex64("bond automatic"), testBondPico, 12)
		x.mustPoll()
		if got := x.appState(app); got != "approved:xmr" || userRole(x.testEnv, uid) != "vendor" || !hasNote(x.testEnv, uid, "Your vendor application was approved") {
			t.Fatalf("automatic approval: %s, role %s", got, userRole(x.testEnv, uid))
		}
	})
	t.Run("under", func(t *testing.T) {
		x, _ := newVendorBondEnv(t, "manual")
		_, sess, app := x.bondApplicant()
		x.mustPoll()
		addr := x.mustBondAddress(app)
		x.fake.payAddress(addr, hex64("bond part 1"), 100_000_000_000, 12)
		x.mustPoll()
		x.mustPoll()
		if got := x.appState(app); got != "awaiting_bond:" {
			t.Fatalf("an under-payment recorded the bond: %s", got)
		}
		mustContain(t, x.accountPage(sess), "0.1 XMR with at least 10 confirmations", "Still to pay</dt><dd>0.15 XMR")
		x.fake.payAddress(addr, hex64("bond part 2"), 150_000_000_000, 12)
		x.mustPoll()
		if got := x.appState(app); got != "awaiting_approval:xmr" || agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits WHERE credited") != 2 {
			t.Fatalf("the topped-up bond: %s", got)
		}
	})
	t.Run("over", func(t *testing.T) {
		x, _ := newVendorBondEnv(t, "manual")
		uid, _, app := x.bondApplicant()
		x.mustPoll()
		x.fake.payAddress(x.mustBondAddress(app), hex64("bond over"), 300_000_000_000, 12)
		x.mustPoll()
		x.mustPoll()
		if got := x.appState(app); got != "awaiting_approval:xmr" {
			t.Fatalf("over-payment: %s", got)
		}
		if x.adminsTold("the 0.05 XMR "+bondOverNotice) != 2 || !hasNote(x.testEnv, uid, "You paid 0.05 XMR more than the bond; it is kept") {
			t.Fatalf("over-payment notices: admins %d, applicant %v", x.adminsTold(bondOverNotice), x.notes(uid))
		}
	})
	t.Run("over, still confirming", func(t *testing.T) {
		x, adminSess := newVendorBondEnv(t, "manual")
		uid, _, app := x.bondApplicant()
		x.mustPoll()
		addr := x.mustBondAddress(app)
		extra := hex64("bond extra")
		x.fake.payAddress(addr, hex64("bond full"), testBondPico, 12)
		x.fake.payAddress(addr, extra, 50_000_000_000, 2)
		x.mustPoll()
		x.mustPoll()
		if got := x.appState(app); got != "awaiting_approval:xmr" || x.adminsTold(bondOverNotice) != 0 {
			t.Fatalf("the bond with 0.05 XMR still confirming: %s, over notices %d", got, x.adminsTold(bondOverNotice))
		}
		x.fake.setConfs(extra, 12)
		x.mustPoll()
		x.mustPoll()
		if x.adminsTold("the 0.05 XMR "+bondOverNotice) != 2 || x.adminsTold(bondLateNotice) != 0 {
			t.Fatalf("the extra 0.05 XMR confirmed: over notices %d", x.adminsTold(bondOverNotice))
		}
		admin := x.body("GET", adminUserURL(x.handleOf(uid)), adminSess, nil, 200)
		mustContain(t, admin, "0.05 XMR arrived before the bond was recorded but was not counted toward it")
	})
	t.Run("late", func(t *testing.T) {
		x, adminSess := newVendorBondEnv(t, "manual")
		cases := []struct {
			name, state string
			decide      func(sess, app string)
		}{
			{"withdrawn", "withdrawn:", func(sess, _ string) {
				x.check(x.do("POST", "/account/vendor-application", sess, url.Values{"action": {"withdraw"}}), 303)
			}},
			{"rejected", "rejected:", func(_, app string) { x.adminDecide(adminSess, app, "reject") }},
			{"waived", "awaiting_approval:waived", func(_, app string) { x.adminDecide(adminSess, app, "waive") }},
			{"recorded", "awaiting_approval:manual", func(_, app string) { x.adminDecide(adminSess, app, "bond") }},
			{"approved", "approved:waived", func(_, app string) { x.adminDecide(adminSess, app, "waive"); x.adminDecide(adminSess, app, "approve") }},
		}
		for _, c := range cases {
			uid, sess, app := x.bondApplicant()
			x.mustPoll()
			addr := x.mustBondAddress(app)
			c.decide(sess, app)
			x.fake.payAddress(addr, hex64("bond late "+c.name), testBondPico, 12)
			x.mustPoll()
			x.mustPoll()
			if got := x.appState(app); got != c.state {
				t.Fatalf("%s: a late deposit changed the application to %s", c.name, got)
			}
			if n := agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits d JOIN vendor_bond_accounts c ON c.id=d.account_id WHERE c.application_id=$1 AND d.late AND d.flagged AND NOT d.credited", app); n != 1 {
				t.Fatalf("%s: late deposit rows %d", c.name, n)
			}
			if wantRole := map[bool]string{true: "vendor", false: "buyer"}[c.name == "approved"]; userRole(x.testEnv, uid) != wantRole {
				t.Fatalf("%s: role %s", c.name, userRole(x.testEnv, uid))
			}
		}
		if n := x.adminsTold(bondLateNotice); n != 2*len(cases) { // two administrators, once per deposit
			t.Fatalf("late notices %d", n)
		}
		admin := html.UnescapeString(x.body("GET", adminUserURL(agStr(x.testEnv, "SELECT handle FROM users u JOIN vendor_applications a ON a.user_id=u.id WHERE a.state='rejected'")), adminSess, nil, 200))
		mustContain(t, admin, "0.25 XMR arrived after the application stopped waiting for its bond: kept, not counted.")
	})
	t.Run("locked", func(t *testing.T) {
		x, _ := newVendorBondEnv(t, "manual")
		_, _, app := x.bondApplicant()
		x.mustPoll()
		addr := x.mustBondAddress(app)
		locked := hex64("bond locked")
		x.fake.payAddress(addr, locked, testBondPico, 12)
		x.fake.setTransfer(locked, func(t *fakeXMRTransfer) { t.UnlockTime = 5000 })
		x.mustPoll()
		x.mustPoll()
		if got := x.appState(app); got != "awaiting_bond:" || x.adminsTold(bondLockedNotice) != 2 {
			t.Fatalf("a locked deposit: %s, notices %d", got, x.adminsTold(bondLockedNotice))
		}
		x.fake.payAddress(addr, hex64("bond after locked"), testBondPico, 12)
		x.mustPoll()
		if got := x.appState(app); got != "awaiting_approval:xmr" || agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits WHERE locked AND credited") != 0 {
			t.Fatalf("after an ordinary deposit: %s", got)
		}
	})
}

// unsealBond decrypts the bond wallet's sealed seed with the test's private seed key.
func (x *xmrEscrowEnv) unsealBond() string {
	x.t.Helper()
	return x.unsealArmored(agStr(x.testEnv, "SELECT seed_sealed FROM xmr_bond_wallets WHERE name=$1", testBondWallet))
}

// A counted deposit that a reorganisation takes back below the threshold after approval is flagged to the
// administrators once; the application stays approved and the account a vendor.
func TestVendorBondReorgAfterApprovalFlagsOnly(t *testing.T) {
	x, _ := newVendorBondEnv(t, "automatic")
	uid, _, app := x.bondApplicant()
	x.mustPoll()
	tx := hex64("bond reorg")
	x.fake.payAddress(x.mustBondAddress(app), tx, testBondPico, 12)
	x.mustPoll()
	if x.appState(app) != "approved:xmr" {
		t.Fatal("not approved")
	}
	x.fake.setConfs(tx, 3)
	x.mustPoll()
	x.fake.setTransfer(tx, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
	x.mustPoll()
	if got := x.appState(app); got != "approved:xmr" || userRole(x.testEnv, uid) != "vendor" {
		t.Fatalf("after the reorganisation: %s, role %s", got, userRole(x.testEnv, uid))
	}
	if n := x.adminsTold(bondReorgNotice); n != 2 {
		t.Fatalf("reorganisation notices %d", n)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits WHERE credited AND reorg_flagged AND confirmations=-1") != 1 {
		t.Fatal("the deposit row")
	}
}

// Repeated and concurrent watcher passes, and concurrent credits, record each deposit once and the bond once.
func TestVendorBondWatcherIdempotent(t *testing.T) {
	x, _ := newVendorBondEnv(t, "automatic")
	uid, _, app := x.bondApplicant()
	x.mustPoll()
	addr := x.mustBondAddress(app)
	x.fake.payAddress(addr, hex64("bond idem 1"), 100_000_000_000, 12)
	x.fake.payAddress(addr, hex64("bond idem 2"), 150_000_000_000, 12)
	run := func(n int, f func() error) {
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for range n {
			wg.Go(func() { errs <- f() })
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	// Record the deposits without the gate's credit first (paused), then credit from four goroutines at once.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	run(4, x.poll)
	x.mustPoll()
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
	run(4, func() error { return x.A.xmrBondCredit(context.Background(), app) })
	run(4, x.poll)
	for range 2 {
		x.mustPoll()
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits"); n != 2 {
		t.Fatalf("deposit rows %d", n)
	}
	if got := x.appState(app); got != "approved:xmr" || userRole(x.testEnv, uid) != "vendor" {
		t.Fatalf("state %s", got)
	}
	audits := 0
	for _, a := range x.audits(uid) {
		if strings.Contains(a, "received on the Monero network") {
			audits++
		}
	}
	if audits != 1 || agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,'was received on the Monero network')>0", uid) != 1 {
		t.Fatalf("credit audits %d", audits)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_accounts"); n != 1 {
		t.Fatalf("bond accounts %d", n)
	}
}

// Under the restore gate or the sweep, with XMR intake paused or Monero escrow not ready, no bond account is created
// and no bond address shown (the page says an administrator records the bond); deposits are still read under the
// gate, but no bond is recorded and nothing approved until it clears.
func TestVendorBondGateHoldsIssuanceAndApproval(t *testing.T) {
	x, _ := newVendorBondEnv(t, "automatic")
	manual := "The bond is recorded by hand: an administrator records it as received"
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	uid, sess, app := x.bondApplicant()
	x.mustPoll()
	if x.bondAddress(app) != "" || agInt(x.testEnv, "SELECT count(*) FROM xmr_bond_wallets") != 0 {
		t.Fatal("the gate let the watcher create a bond wallet or account")
	}
	if _, ok := x.fake.file(testBondWallet); ok {
		t.Fatal("a bond wallet file was created under the gate")
	}
	page := x.accountPage(sess)
	mustContain(t, page, manual, "Pay the bond by hand")
	if strings.Contains(page, "preparing a bond address") {
		t.Fatal("the page promised an address under the gate")
	}
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
	// With no bond wallet yet and ESCROW_XMR_SEED_KEY not the pinned key, none could be created: no address is promised.
	x.A.escrowXMR.canCreate.Store(false)
	if page := x.accountPage(sess); strings.Contains(page, "preparing a bond address") || !strings.Contains(page, manual) {
		t.Fatal("an address was promised while no bond wallet can be created")
	}
	x.A.escrowXMR.canCreate.Store(true)
	x.mustPoll()
	addr := x.mustBondAddress(app)
	mustContain(t, x.accountPage(sess), addr)

	// With an address already issued, the page hides it but says payments to it still count (QA67-1); it never says
	// the bond is only recorded by hand.
	hidden := func(why string) {
		t.Helper()
		page := x.accountPage(sess)
		if strings.Contains(page, addr) || strings.Contains(page, manual) {
			t.Fatalf("%s: the bond address is shown, or the page says the bond is recorded by hand only", why)
		}
		mustContain(t, page, "Payments already sent to this application's bond address are still counted automatically",
			"New bond addresses are not being issued right now", "Pay the bond by hand")
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	hidden("restore gate")
	x.fake.payAddress(addr, hex64("bond under gate"), testBondPico, 12)
	x.mustPoll()
	x.mustPoll()
	if got := x.appState(app); got != "awaiting_bond:" || userRole(x.testEnv, uid) != "buyer" {
		t.Fatalf("under the gate: %s, role %s", got, userRole(x.testEnv, uid))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits WHERE confirmations>=10 AND NOT credited") != 1 {
		t.Fatal("the deposit was not read under the gate")
	}
	mustContain(t, x.accountPage(sess), "so far 0.25 XMR has them")
	// A second applicant under the gate gets no account.
	_, _, app2 := x.bondApplicant()
	x.mustPoll()
	if x.bondAddress(app2) != "" {
		t.Fatal("an account was issued under the gate")
	}
	agExec(x.testEnv, "UPDATE settings SET value='false' WHERE key='payments_recovery_required'")
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrEscrowSweepKey)
	hidden("restore sweep")
	agExec(x.testEnv, "DELETE FROM settings WHERE key=$1", xmrEscrowSweepKey)
	agExec(x.testEnv, "UPDATE settings SET value='false' WHERE key='payment_intake_XMR'")
	hidden("intake paused")
	// Intake paused holds issuance only: the deposit already sent is recorded and the held approval happens.
	x.mustPoll()
	if x.bondAddress(app2) != "" || x.appState(app) != "approved:xmr" {
		t.Fatalf("intake paused: second address %q; the first application %s (held approval resumes once the gate clears)", x.bondAddress(app2), x.appState(app))
	}
	if userRole(x.testEnv, uid) != "vendor" {
		t.Fatal("the held approval did not make a vendor")
	}
	agExec(x.testEnv, "UPDATE settings SET value='escrow' WHERE key='payment_intake_XMR'")
	x.A.escrowXMR.ready.Store(false)
	_, sess3, _ := x.bondApplicant()
	mustContain(t, x.accountPage(sess3), manual)
	x.A.escrowXMR.ready.Store(true)
	x.mustPoll()
	if x.bondAddress(app2) == "" {
		t.Fatal("no address once issuance resumed")
	}
}

// The bond wallet is Monero escrow state for the network pin, and its addresses are of the verified network.
func TestVendorBondNetworkPin(t *testing.T) {
	t.Run("pin", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		ctx := context.Background()
		if pinned := agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrNetworkPin); pinned != "fakechain" {
			t.Fatalf("pinned %q", pinned)
		}
		agExec(x.testEnv, "INSERT INTO xmr_bond_wallets(name,state) VALUES('bond-01','orphan')")
		if pinned, ok, err := pinXMRNetwork(ctx, x.A.db, "stagenet"); err != nil || ok || pinned != "fakechain" {
			t.Fatalf("a database holding a bond wallet was re-pinned: %q %v %v", pinned, ok, err)
		}
		agExec(x.testEnv, "DELETE FROM xmr_bond_wallets")
		if pinned, ok, err := pinXMRNetwork(ctx, x.A.db, "stagenet"); err != nil || !ok || pinned != "stagenet" {
			t.Fatalf("without the bond wallet: %q %v %v", pinned, ok, err)
		}
	})
	t.Run("stagenet", func(t *testing.T) {
		x := newXMREscrowEnvWith(t, onStagenet)
		setVendorPolicy(x.testEnv, strconv.FormatInt(testBondPico, 10), "manual", "")
		_, sess, app := x.bondApplicant()
		x.mustPoll()
		addr := x.mustBondAddress(app)
		primary := agStr(x.testEnv, "SELECT primary_address FROM xmr_bond_wallets")
		for _, a := range []string{addr, primary} {
			if _, err := parseXMRAddress(a, "stagenet"); err != nil {
				t.Fatalf("a bond address is not a stagenet address: %v", err)
			}
			if _, err := parseXMRAddress(a, "fakechain"); err == nil {
				t.Fatal("a bond address parses for another network")
			}
		}
		mustContain(t, x.accountPage(sess), addr, "Monero (Monero stagenet)")
		// A stored address of another network is never shown.
		other := testXMRAddrOn("fakechain", "bond other network", true)
		agExec(x.testEnv, "UPDATE vendor_bond_accounts SET address=$2 WHERE application_id=$1", app, other)
		if page := x.accountPage(sess); strings.Contains(page, other) {
			t.Fatal("an address of another network was shown")
		}
	})
	t.Run("wallet answers another network", func(t *testing.T) {
		x, _ := newVendorBondEnv(t, "manual")
		_, _, app := x.bondApplicant()
		// create_account answers a stagenet address on a fakechain market: refused, nothing written.
		x.fake.handle("create_account", func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
			n := len(f.open.accounts)
			f.open.accounts = append(f.open.accounts, testXMRAddrOn("stagenet", "wrong network "+strconv.Itoa(n), true))
			return map[string]any{"account_index": n, "address": f.open.accounts[n]}, 0, ""
		})
		if err := x.poll(); err == nil {
			t.Fatal("a pass that met another network's address passed")
		}
		if x.bondAddress(app) != "" {
			t.Fatal("an address of another network was issued")
		}
	})
}

// An account of the bond wallet with no row is recorded for no application and the administrators told once; coins at
// it fund nothing and are reported once; the next application gets a fresh account.
func TestVendorBondRowlessAccountRetired(t *testing.T) {
	x, _ := newVendorBondEnv(t, "manual")
	_, _, app := x.bondApplicant()
	x.mustPoll()
	x.mustBondAddress(app)
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.files[testBondWallet]
		w.accounts = append(w.accounts, testXMRAddr("fake "+testBondWallet+" account 2", true))
	})
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT account_index||':'||coalesce(application_id,'none') FROM vendor_bond_accounts ORDER BY account_index"); !slices.Equal(got, []string{"1:" + app, "2:none"}) {
		t.Fatalf("bond accounts %v", got)
	}
	if x.adminsTold(bondRowless) != 2 || agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id IS NULL AND action LIKE 'Vendor bond wallet bond-01: recorded 1 account(s)%'") != 1 {
		t.Fatalf("row-less notices %d", x.adminsTold(bondRowless))
	}
	x.fake.pay(testBondWallet, 2, hex64("bond to rowless"), testBondPico, 12)
	x.mustPoll()
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits") != 0 || x.appState(app) != "awaiting_bond:" {
		t.Fatal("a deposit to a row-less account was recorded or counted")
	}
	if x.adminsTold(bondUnclaimed) != 2 || x.adminsTold(bondRowless) != 2 {
		t.Fatalf("unclaimed notices %d", x.adminsTold(bondUnclaimed))
	}
	// Coins at the bond wallet's account 0 are reported once while they are there.
	x.fake.pay(testBondWallet, 0, hex64("bond to account 0"), testBondPico, 12)
	x.mustPoll()
	x.mustPoll()
	if n := x.adminsTold("coins arrived at its primary account (account 0)"); n != 2 || agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits") != 0 {
		t.Fatalf("account 0 notices %d", n)
	}
	// A create_account whose answer is lost: the account is found row-less on the next pass, and the application waiting
	// for an address gets a fresh one.
	_, _, app2 := x.bondApplicant()
	x.fake.fault("create_account", fakeXMRDrop)
	if err := x.poll(); err == nil {
		t.Fatal("a create_account with no answer passed")
	}
	x.mustPoll()
	if _, account := x.fake.accountOf(x.mustBondAddress(app2)); account != 4 {
		t.Fatalf("the application after a lost answer got account %d", account)
	}
	if got := agStr(x.testEnv, "SELECT coalesce(application_id,'none') FROM vendor_bond_accounts WHERE account_index=3"); got != "none" {
		t.Fatalf("account 3: %s", got)
	}
	if x.adminsTold(bondRowless) != 4 {
		t.Fatalf("row-less notices after the lost answer %d", x.adminsTold(bondRowless))
	}
}

// After a restore: a bond wallet created after the backup is found by the sweep and recorded as an orphan with its
// seed sealed; a bond wallet older than the database gets a lookahead past its highest recorded account and sees a
// deposit to an account it did not know; a bond wallet that does not open is reported and the sweep completes.
func TestVendorBondRestoreSweep(t *testing.T) {
	x, _ := newVendorBondEnv(t, "manual")
	_, _, app := x.bondApplicant()
	x.mustPoll()
	addr := x.mustBondAddress(app)
	// The wallet file is older than the database: it knows account 0 alone, with the default lookahead of 1 so that an
	// ordinary refresh misses account 1.
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.files[testBondWallet]
		w.accounts, w.lookahead = w.accounts[:1], 1
	})
	x.fake.pay(testBondWallet, 1, hex64("bond after restore"), testBondPico, 12)
	// A bond wallet created after the backup.
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.newFile("bond-02", xmrWalletPassword(testXMRSecret, "bond-02"))
		w.accounts = append(w.accounts, testXMRAddr("fake bond-02 account 1", true), testXMRAddr("fake bond-02 account 2", true))
		w.transfers = append(w.transfers, &fakeXMRTransfer{TxID: hex64("lost bond"), Account: 1, Amount: testBondPico, Confs: 20})
	})
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrEscrowSweepKey)
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1", xmrEscrowSweepKey) != 0 {
		t.Fatal("the sweep did not complete")
	}
	if got := agStrings(t, x.testEnv, "SELECT name||':'||state||':'||(seed_sealed IS NOT NULL) FROM xmr_bond_wallets ORDER BY id"); !slices.Equal(got, []string{"bond-01:sealed:true", "bond-02:orphan:true"}) {
		t.Fatalf("bond wallets %v", got)
	}
	if x.adminsTold("account 1 (0.25 XMR)") != 2 {
		t.Fatal("the orphan bond wallet's funded account was not listed")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits d JOIN vendor_bond_accounts c ON c.id=d.account_id WHERE c.address=$1", addr) != 1 {
		t.Fatal("the lookahead did not reveal the deposit to the recorded account")
	}
	x.mustPoll()
	if x.appState(app) != "awaiting_approval:xmr" {
		t.Fatalf("after the sweep: %s", x.appState(app))
	}
	// PE-2: the orphan is read like a closed wallet. Its funded account was listed already and is not reported again; a
	// later deposit to its other account is reported once to each administrator and funds nothing.
	if x.adminsTold(bondUnclaimed) != 0 {
		t.Fatal("the orphan's listed account was reported again")
	}
	x.fake.pay("bond-02", 2, hex64("later to the orphan"), testBondPico, 12)
	x.mustPoll()
	x.mustPoll()
	if x.adminsTold(bondUnclaimed) != 2 || x.adminsTold("bond-02: account 2") != 2 || agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits") != 1 {
		t.Fatalf("a later deposit to the orphan: %d notices", x.adminsTold(bondUnclaimed))
	}
	// The bond wallet stops opening (another password): reported once, and a sweep still completes.
	x.fake.edit(func(f *fakeXMREscrow) { f.files[testBondWallet].password = "not the market's" })
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrEscrowSweepKey)
	if err := x.poll(); err == nil {
		t.Fatal("a pass with an unopenable bond wallet reported no error")
	}
	x.poll()
	if x.adminsTold("does not open on the escrow wallet-rpc") != 2 || agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1", xmrEscrowSweepKey) != 0 {
		t.Fatal("an unopenable bond wallet was not reported once, or held the sweep")
	}
	if got := agStrings(t, x.testEnv, "SELECT name FROM xmr_bond_wallets WHERE state IN ('creating','sealed')"); !slices.Equal(got, []string{testBondWallet}) {
		t.Fatalf("a replacement bond wallet was created: %v", got)
	}
	// PE-3: while the bond wallet in use does not open, no address is promised: a new applicant sees the manual text.
	_, sess, _ := x.bondApplicant()
	if page := x.accountPage(sess); strings.Contains(page, "preparing a bond address") || !strings.Contains(page, "The bond is recorded by hand") {
		t.Fatal("an address was promised while the bond wallet does not open")
	}
}
