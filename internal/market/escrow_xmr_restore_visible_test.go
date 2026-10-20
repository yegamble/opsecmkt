package market

import (
	"html"
	"strings"
	"testing"
)

// Texts the staff pages show while a restore pauses Monero escrow (A-559, STAFF66-2).
const (
	testGateAlert   = "Monero escrow is paused after a restore from backup (the restore gate is set)"
	testGateRuns    = "It still watches deposits and follows relayed settlements to their confirmations."
	testSweepAlert  = "Monero escrow checkout is paused after a restore from backup until the market has checked its escrow wallets"
	testGateFlag    = "Restore gate: set."
	testSweepDone   = "Monero escrow's restore sweep is complete"
	testReadyPaused = "XMR escrow: on; intake: paused (restore)."
	testReadyOpen   = "XMR escrow: on; intake: escrow."
)

// A-559: the restore gate is visible to staff. While it is set, /admin/payments flags it on the Monero escrow recovery
// view and readiness reads "paused (restore)", the overview and the moderation desk carry an alert naming what is
// paused and what still runs, a refused checkout is a 503 that is counted on the recovery view but not listed among the
// recent server errors (STAFF66-N3), and the sweep's completion under the gate tells each administrator once, however
// many watcher passes follow. Clearing the gate removes all of it; the sweep alone has its own, shorter alert.
func TestXMREscrowRestoreGateVisible(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, adminSess := x.user("xg_admin_"+randomToken()[:6], "admin")
	modID, modSess := x.user("xg_mod_"+randomToken()[:6], "moderator")
	page := func(path, sess string) string {
		t.Helper()
		return html.UnescapeString(x.page(path, sess))
	}
	has := func(where, body string, want, not []string) {
		t.Helper()
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Fatalf("%s lacks %q", where, s)
			}
		}
		for _, s := range not {
			if strings.Contains(body, s) {
				t.Fatalf("%s shows %q", where, s)
			}
		}
	}
	has("payments before the restore", page("/admin/payments", adminSess), []string{testReadyOpen}, []string{testGateFlag, testGateAlert, testReadyPaused})
	has("overview before the restore", page("/admin", adminSess), nil, []string{testGateAlert, testSweepAlert})

	x.restoreBackup() // the gate and the sweep
	has("payments under the gate", page("/admin/payments", adminSess), []string{testGateFlag + " Restore sweep: pending.", testGateAlert, testGateRuns, testReadyPaused,
		"The restore sweep has not completed yet", "Monero escrow checkouts refused while paused since this server started: 0"}, []string{testReadyOpen})
	has("overview under the gate", page("/admin", adminSess), []string{testGateAlert, testGateRuns}, nil)
	has("moderation desk under the gate", page("/moderator", modSess), []string{testGateAlert, testGateRuns}, nil)

	// STAFF66-N3: the refused checkout is a 503 the buyer reads, counted on the recovery view, not a server error.
	draft := x.draft(x.product)
	for range 2 {
		w := x.checkout(x.buyerSess, draft, testXMRAddr("refund "+draft, true))
		if w.Code != 503 || !strings.Contains(html.UnescapeString(w.Body.String()), errXMREscrowRestoring.Error()) || strings.Contains(w.Body.String(), "Reference:") || w.Header().Get("Retry-After") == "" {
			t.Fatalf("checkout under the gate: %d %v %s", w.Code, w.Header(), w.Body.String())
		}
	}
	if got := x.A.recentErrors.snapshot(); len(got) != 0 {
		t.Fatalf("restore-gate refusals listed as server errors: %#v", got)
	}
	has("payments after two refusals", page("/admin/payments", adminSess), []string{"Monero escrow checkouts refused while paused since this server started: 2"}, nil)
	has("system after two refusals", page("/admin/system", adminSess), []string{"No referenced server errors in this process."}, nil)

	// The sweep completes under the gate: one notice per administrator, none to the moderator, however many passes.
	x.mustPoll()
	if x.sweepRequired() {
		t.Fatal("the sweep did not complete")
	}
	for range 3 {
		x.mustPoll()
	}
	if n, m := x.notified(x.admin, testSweepDone), x.notified(modID, testSweepDone); n != 1 || m != 0 {
		t.Fatalf("sweep-complete notices: %d to the administrator, %d to the moderator", n, m)
	}
	if n := x.notified(x.admin, "The restore gate is still set"); n != 1 {
		t.Fatalf("the notice does not say the gate is still set (%d)", n)
	}
	has("payments after the sweep", page("/admin/payments", adminSess), []string{testGateFlag + " Restore sweep: not pending.", testGateAlert, testReadyPaused}, []string{"The restore sweep has not completed yet"})

	x.clearGate()
	has("payments after the gate", page("/admin/payments", adminSess), []string{testReadyOpen}, []string{testGateFlag, testGateAlert, testReadyPaused})
	has("overview after the gate", page("/admin", adminSess), nil, []string{testGateAlert, testSweepAlert})

	// The sweep alone (a wallet volume restored from an older copy): its own alert; checkout refused, settlements run.
	// The desk renders the same summary partial from the same loader as /admin; it is read only once here, as /moderator
	// times out on a loaded host (A-615).
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true')", xmrEscrowSweepKey)
	for _, p := range []struct{ path, sess string }{{"/admin", adminSess}, {"/admin/payments", adminSess}} {
		has(p.path+" under the sweep", page(p.path, p.sess), []string{testSweepAlert, "Settlements, relays and release deadlines run as usual."}, []string{testGateAlert, testGateFlag})
	}
	has("readiness under the sweep", page("/admin/payments", adminSess), []string{testReadyPaused, "Restore sweep: pending."}, nil)
	w := x.checkout(x.buyerSess, draft, testXMRAddr("refund "+draft, true))
	if w.Code != 503 || !strings.Contains(html.UnescapeString(w.Body.String()), errXMREscrowSweep.Error()) || len(x.A.recentErrors.snapshot()) != 0 {
		t.Fatalf("checkout under the sweep: %d %s", w.Code, w.Body.String())
	}
	x.mustPoll()
	if x.sweepRequired() || x.notified(x.admin, testSweepDone) != 1 {
		t.Fatalf("the sweep without the gate: required %v, %d notices", x.sweepRequired(), x.notified(x.admin, testSweepDone))
	}
	x.mustPoll()
	x.mustCheckout(draft)
}

// STAFF66-N3: a 5xx that is not a restore pause is still listed among the recent server errors with its reference.
func TestXMREscrowUnavailableStillServerError(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.A.escrowXMR.ready.Store(false)
	draft := x.draft(x.product)
	w := x.checkout(x.buyerSess, draft, testXMRAddr("refund "+draft, true))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Reference:") {
		t.Fatalf("checkout while unavailable: %d %s", w.Code, w.Body.String())
	}
	if got := x.A.recentErrors.snapshot(); len(got) != 1 || got[0].Status != 503 {
		t.Fatalf("unavailable escrow not listed: %#v", got)
	}
}

// With Monero escrow off a restore pauses nothing: no staff alert, no recovery flag, readiness unchanged (and, as
// TestEscrowRecoverySectionHiddenWithoutEscrow pins, no recovery section at all).
func TestXMREscrowRestoreGateQuietWhenOff(t *testing.T) {
	e := newTestApp(t)
	if e.A.escrowXMR != nil {
		t.Fatal("Monero escrow is on in a plain test app")
	}
	_, admin := e.user("xg_off_admin", "admin")
	for _, key := range []string{"payments_recovery_required", xmrEscrowSweepKey} {
		agExec(e, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", key)
	}
	agExec(e, "INSERT INTO settings(key,value) VALUES('payment_intake_XMR','escrow') ON CONFLICT (key) DO UPDATE SET value='escrow'")
	for _, path := range []string{"/admin", "/admin/payments"} { // the desk's summary is the same partial (A-615: /moderator is slow under load)
		w := e.do("GET", path, admin, nil)
		e.check(w, 200)
		body := html.UnescapeString(w.Body.String())
		for _, s := range []string{"Paused after a restore from backup", testGateFlag, "paused (restore)", "xmr-restore-gate"} {
			if strings.Contains(body, s) {
				t.Fatalf("%s with Monero escrow off shows %q", path, s)
			}
		}
	}
}
