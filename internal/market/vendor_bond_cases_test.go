package market

import (
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// A-547 fix round (iteration 67): restore closing the bond wallet, and the cases the verifiers probed by hand, as DB
// integration tests against the simulated wallet-rpc of escrow_xmr_fake_test.go (simulated RPC, not chain evidence).

// restoreBondSQL is the vendor bond block of scripts/restore.sh (between its xmr-bond-restore markers), run here as the
// script runs it, so these tests exercise the script's own SQL.
func restoreBondSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("scripts/restore.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(b), "-- xmr-bond-restore: begin\n")
	block, _, ok2 := strings.Cut(block, "-- xmr-bond-restore: end\n")
	if !ok || !ok2 || strings.Contains(block, `\`) {
		t.Fatal("scripts/restore.sh has no plain-SQL xmr-bond-restore block")
	}
	return block
}

// restoreWithBonds runs scripts/restore.sh's settings, Monero escrow and vendor bond blocks on this database and
// returns the number of bond wallets it closed.
func (x *xmrEscrowEnv) restoreWithBonds() int {
	x.t.Helper()
	x.restoreBackup()
	var closed int
	if err := x.DB.QueryRow(restoreBondSQL(x.t)).Scan(&closed); err != nil {
		x.t.Fatalf("the bond restore block: %v", err)
	}
	return closed
}

func (x *xmrEscrowEnv) handleOf(uid string) string {
	return agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", uid)
}

// PE-1: after a full restore (database and wallet volume to the backup), the restored bond wallet never issues again,
// so an index it gave out after the backup is never given to another applicant; an application recorded on it is still
// credited from its recorded account; a deposit to an index the restored records lack funds nothing and is reported.
func TestVendorBondRestoreClosesWallet(t *testing.T) {
	x, _ := newVendorBondEnv(t, "manual")
	_, sessA, appA := x.bondApplicant()
	x.mustPoll()
	addrA := x.mustBondAddress(appA)
	// The backup is taken here. Afterwards B applies and is given account 2; then the database and the wallet volume are
	// restored to the backup: B's application and account row are gone, and the wallet file knows accounts 0 and 1.
	_, _, appB := x.bondApplicant()
	x.mustPoll()
	addrB := x.mustBondAddress(appB)
	if _, account := x.fake.accountOf(addrB); account != 2 {
		t.Fatalf("B's account %d", account)
	}
	agExec(x.testEnv, "DELETE FROM vendor_bond_accounts WHERE application_id=$1", appB)
	agExec(x.testEnv, "DELETE FROM vendor_applications WHERE id=$1", appB)
	x.fake.edit(func(f *fakeXMREscrow) { w := f.files[testBondWallet]; w.accounts = w.accounts[:2] })
	if closed := x.restoreWithBonds(); closed != 1 || agStr(x.testEnv, "SELECT state FROM xmr_bond_wallets WHERE name=$1", testBondWallet) != "closed" {
		t.Fatalf("the restore closed %d bond wallet(s)", closed)
	}
	_, sessC, appC := x.bondApplicant()
	x.mustPoll() // the sweep: nothing issued under the gate
	if x.bondAddress(appC) != "" {
		t.Fatal("an address was issued under the restore gate")
	}
	x.clearGate()
	x.mustPoll()
	addrC := x.mustBondAddress(appC)
	if name, account := x.fake.accountOf(addrC); name != "bond-02" || account != 1 || addrC == addrB {
		t.Fatalf("after the restore the new application got account %d of %s", account, name)
	}
	if got := agStrings(t, x.testEnv, "SELECT name||':'||state FROM xmr_bond_wallets ORDER BY id"); strings.Join(got, ",") != testBondWallet+":closed,bond-02:sealed" {
		t.Fatalf("bond wallets %v", got)
	}
	mustContain(t, x.accountPage(sessA), addrA)
	mustContain(t, x.accountPage(sessC), addrC)
	// B pays the address it was given after the backup; A pays its recorded one.
	x.fake.pay(testBondWallet, 2, hex64("B after restore"), testBondPico, 12)
	x.fake.payAddress(addrA, hex64("A after restore"), testBondPico, 12)
	x.mustPoll()
	x.mustPoll()
	if x.appState(appA) != "awaiting_approval:xmr" || x.appState(appC) != "awaiting_bond:" {
		t.Fatalf("A %s, C %s", x.appState(appA), x.appState(appC))
	}
	if x.adminsTold(bondUnclaimed) != 2 || x.adminsTold(bondRowless) != 2 {
		t.Fatalf("unclaimed notices %d, row-less notices %d", x.adminsTold(bondUnclaimed), x.adminsTold(bondRowless))
	}
	if got := agStr(x.testEnv, "SELECT coalesce(application_id,'none') FROM vendor_bond_accounts c JOIN xmr_bond_wallets w ON w.id=c.wallet_id WHERE w.name=$1 AND c.account_index=2", testBondWallet); got != "none" {
		t.Fatalf("bond-01 account 2 is recorded for %s", got)
	}
}

// Under automatic approval, a suspended applicant's bond is recorded but not approved.
func TestVendorBondSuspendedApplicantWaits(t *testing.T) {
	x, _ := newVendorBondEnv(t, "automatic")
	uid, _, app := x.bondApplicant()
	x.mustPoll()
	addr := x.mustBondAddress(app)
	agExec(x.testEnv, "UPDATE users SET suspended_at=now() WHERE id=$1", uid)
	x.fake.payAddress(addr, hex64("suspended bond"), testBondPico, 12)
	x.mustPoll()
	if got := x.appState(app); got != "awaiting_approval:xmr" || userRole(x.testEnv, uid) != "buyer" {
		t.Fatalf("suspended applicant: %s, role %s", got, userRole(x.testEnv, uid))
	}
}

// A ban withdraws the application; a later deposit is late, flagged once and changes nothing.
func TestVendorBondBanThenDeposit(t *testing.T) {
	x, adminSess := newVendorBondEnv(t, "automatic")
	uid, _, app := x.bondApplicant()
	x.mustPoll()
	addr := x.mustBondAddress(app)
	agExpect(x.testEnv, "/admin/ban", adminSess, banForm("ban", x.handleOf(uid), testBanReason), 303, "")
	if got := x.appState(app); got != "withdrawn:" {
		t.Fatalf("after the ban: %s", got)
	}
	x.fake.payAddress(addr, hex64("banned bond"), testBondPico, 12)
	x.mustPoll()
	x.mustPoll()
	if got := x.appState(app); got != "withdrawn:" || userRole(x.testEnv, uid) != "buyer" || x.adminsTold(bondLateNotice) != 2 {
		t.Fatalf("a deposit after the ban: %s, role %s, notices %d", got, userRole(x.testEnv, uid), x.adminsTold(bondLateNotice))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits WHERE late AND flagged AND NOT credited") != 1 {
		t.Fatal("the deposit is not recorded late and flagged")
	}
}

// An application an administrator opened for a buyer (A-545) gets an address, and its XMR credit keeps its manual
// approval although the policy is automatic.
func TestVendorBondAdminOpenedApplication(t *testing.T) {
	x, adminSess := newVendorBondEnv(t, "automatic")
	uid, _ := x.user("bond_opened_"+randomToken()[:6], "buyer")
	w := x.do("POST", "/admin/vendor-apply", adminSess, url.Values{"handle": {x.handleOf(uid)}, "start": {"awaiting_bond"}, "password": {testPassword}})
	x.check(w, 303)
	app := latestVendorApp(x.testEnv, uid)
	if app.State != "awaiting_bond" || app.Approval != "manual" {
		t.Fatalf("opened application %+v", app)
	}
	x.mustPoll()
	x.fake.payAddress(x.mustBondAddress(app.ID), hex64("admin opened bond"), testBondPico, 12)
	x.mustPoll()
	if got := x.appState(app.ID); got != "awaiting_approval:xmr" || userRole(x.testEnv, uid) != "buyer" {
		t.Fatalf("after the deposit: %s, role %s", got, userRole(x.testEnv, uid))
	}
}

// A bond address is shown to its applicant and to administrators only.
func TestVendorBondAddressAccess(t *testing.T) {
	x, adminSess := newVendorBondEnv(t, "manual")
	uid, sess, app := x.bondApplicant()
	x.mustPoll()
	addr := x.mustBondAddress(app)
	handle := x.handleOf(uid)
	mustContain(t, x.accountPage(sess), addr)
	for _, path := range []string{"/admin/vendors", adminUserURL(handle)} {
		mustContain(t, x.body("GET", path, adminSess, nil, 200), addr)
	}
	_, otherBuyer := x.user("bond_other_"+randomToken()[:6], "buyer")
	_, vendor := x.user("bond_vendor_"+randomToken()[:6], "vendor")
	_, moderator := x.user("bond_mod_"+randomToken()[:6], "moderator")
	for who, sess := range map[string]string{"another buyer": otherBuyer, "a vendor": vendor, "a moderator": moderator, "anonymous": randomToken()} {
		for _, path := range []string{"/account", "/admin/vendors", adminUserURL(handle)} {
			w := x.do("GET", path, sess, nil)
			if strings.Contains(w.Body.String(), addr) {
				t.Errorf("%s sees the bond address on %s (%d)", who, path, w.Code)
			}
			if strings.HasPrefix(path, "/admin") && w.Code == 200 {
				t.Errorf("%s reached %s", who, path)
			}
		}
	}
}

// Withdrawing and applying again gives a fresh address; a deposit to the old one does not fund the new application.
func TestVendorBondReapplyFreshAddress(t *testing.T) {
	x, _ := newVendorBondEnv(t, "manual")
	uid, sess, first := x.bondApplicant()
	x.mustPoll()
	old := x.mustBondAddress(first)
	x.check(x.do("POST", "/account/vendor-application", sess, url.Values{"action": {"withdraw"}}), 303)
	x.check(x.do("POST", "/account/vendor-application", sess, url.Values{"action": {"apply"}}), 303)
	second := latestVendorApp(x.testEnv, uid).ID
	x.mustPoll()
	fresh := x.mustBondAddress(second)
	if fresh == old || second == first {
		t.Fatal("the new application reused the old address")
	}
	page := x.accountPage(sess)
	if !strings.Contains(page, fresh) || strings.Contains(page, old) {
		t.Fatal("the page does not show only the new application's address")
	}
	x.fake.payAddress(old, hex64("old address"), testBondPico, 12)
	x.mustPoll()
	x.mustPoll()
	if x.appState(second) != "awaiting_bond:" || x.adminsTold(bondLateNotice) != 2 {
		t.Fatalf("a deposit to the old address: %s, notices %d", x.appState(second), x.adminsTold(bondLateNotice))
	}
}

// Retention erasure of an applicant with a bond address and a deposit: the bond records never block the erasure, and
// coins at the address afterwards fund nothing and are reported once.
func TestVendorBondRetentionErasure(t *testing.T) {
	x, _ := newVendorBondEnv(t, "manual")
	uid, _, app := x.bondApplicant()
	x.mustPoll()
	addr := x.mustBondAddress(app)
	x.fake.payAddress(addr, hex64("before erasure"), 100_000_000_000, 12)
	x.mustPoll()
	tx, err := x.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = x.A.applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account_close", ID: uid, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Its sessions, notices and audit rows have gone by their own retention by the time the account itself is erased
	// (retention maintenance, as the purge runs it).
	if _, err = tx.Exec("SELECT set_config('opsecmkt.retention_purge','on',true)"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "notifications", "audit_events"} {
		if _, err = tx.Exec("DELETE FROM "+table+" WHERE user_id=$1", uid); err != nil {
			t.Fatal(err)
		}
	}
	if needed, err := retentionUserReferenced(t.Context(), tx, uid); err != nil || needed {
		t.Fatalf("bond records blocked the erasure: %t %v", needed, err)
	}
	if err = x.A.applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account", ID: uid, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := agStr(x.testEnv, "SELECT coalesce(application_id,'none')||' '||(SELECT count(*) FROM vendor_bond_deposits) FROM vendor_bond_accounts WHERE address=$1", addr); got != "none 1" {
		t.Fatalf("after erasure: %s", got)
	}
	x.fake.payAddress(addr, hex64("after erasure"), 150_000_000_000, 12)
	x.mustPoll()
	x.mustPoll()
	if x.adminsTold(bondUnclaimed) != 2 || agInt(x.testEnv, "SELECT count(*) FROM vendor_bond_deposits") != 1 {
		t.Fatalf("unclaimed notices %d", x.adminsTold(bondUnclaimed))
	}
}

// Migration 124 on a populated database: an application already waiting for its bond is marked as opened before bond
// addresses, gets an address on the first pass, and is told not to pay again if it already paid by hand (PE-5).
func TestVendorBondMigrationExistingApplication(t *testing.T) {
	x, _ := newVendorBondEnv(t, "manual")
	_, sess, app := x.bondApplicant()
	if strings.Contains(x.accountPage(sess), "opened before the market gave out bond addresses") {
		t.Fatal("a new application is told it predates bond addresses")
	}
	// Put the database back as migration 123 left it, with the application in it, and migrate again.
	agExec(x.testEnv, `DELETE FROM schema_migrations WHERE version=124;
		DROP TABLE vendor_bond_deposits, vendor_bond_accounts, xmr_bond_wallets;
		DROP FUNCTION xmr_bond_wallets_kept();
		ALTER TABLE vendor_applications DROP COLUMN before_bond_addresses`)
	if err := applyLatestMigrations(t, x.testEnv); err != nil {
		t.Fatal(err)
	}
	if agStr(x.testEnv, "SELECT before_bond_addresses::text FROM vendor_applications WHERE id=$1", app) != "true" {
		t.Fatal("the existing application is not marked")
	}
	x.mustPoll()
	mustContain(t, x.accountPage(sess), x.mustBondAddress(app), "If you already paid the bond as the administrator's instructions said, do not pay again here")
	_, sess2, _ := x.bondApplicant()
	x.mustPoll()
	if strings.Contains(x.accountPage(sess2), "opened before the market gave out bond addresses") {
		t.Fatal("an application made after the migration is marked")
	}
}
