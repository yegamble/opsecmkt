package market

import (
	"html"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Escrow Phase 3, slice X-f (WR-escrow A-248, P3-R9, P3-R10, A-418, ES-8): Monero escrow wallets kept 90 days after
// their last order closed and then destroyed, and a database restored from a backup (scripts/restore.sh and the sweep
// that follows it), against the simulated wallet-rpc and monerod of escrow_xmr_fake_test.go. DB integration tests
// (simulated RPC, not chain evidence; scripts/test-escrow-xmr-regtest.sh has the real-chain legs Restore, Lookahead,
// WrongSecret and Destroy).

// restoreXMRSQL is the Monero escrow block of scripts/restore.sh (between its xmr-escrow-restore markers), run here as
// the script runs it, so these tests exercise the script's own SQL.
func restoreXMRSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("scripts/restore.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(b), "-- xmr-escrow-restore: begin\n")
	block, _, ok2 := strings.Cut(block, "-- xmr-escrow-restore: end\n")
	if !ok || !ok2 || strings.Contains(block, `\`) {
		t.Fatal("scripts/restore.sh has no plain-SQL xmr-escrow-restore block")
	}
	return block
}

// restore does to this database what scripts/restore.sh does to a restored one for Monero escrow: the restore gate and
// the sweep marker its settings block writes, then its Monero escrow block, whose counts it returns.
func (x *xmrEscrowEnv) restore() (held, retired, closed int) {
	x.t.Helper()
	for _, key := range []string{"payments_recovery_required", xmrEscrowSweepKey} {
		agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", key)
	}
	if err := x.DB.QueryRow(restoreXMRSQL(x.t)).Scan(&held, &retired, &closed); err != nil {
		x.t.Fatalf("the restore block: %v", err)
	}
	return held, retired, closed
}

func (x *xmrEscrowEnv) clearGate() {
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
}

func (x *xmrEscrowEnv) sweepRequired() bool {
	return agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1 AND value='true'", xmrEscrowSweepKey) == 1
}

// walletOf is the name of the wallet whose account order's escrow uses.
func (x *xmrEscrowEnv) walletOf(order string) string {
	return agStrings(x.t, x.testEnv, `SELECT w.name FROM escrows es JOIN xmr_escrow_slots s ON s.id=es.xmr_slot_id JOIN xmr_escrow_wallets w ON w.id=s.wallet_id
		WHERE es.order_id=$1`, order)[0]
}

// opens counts the open_wallet requests (the sweep's probes included) for wallet name.
func (x *xmrEscrowEnv) opens(name string) int {
	n := 0
	for _, c := range x.fake.callLog() {
		if c.Method == "open_wallet" && strings.Contains(c.Params, `"`+name+`"`) {
			n++
		}
	}
	return n
}

// settled is a digital order of the active wallet completed on funding whose release is confirmed 110 blocks deep (so
// no longer followed), and its escrow address.
func (x *xmrEscrowEnv) settled() (string, string) {
	x.t.Helper()
	order, addr, _ := x.completed()
	id, _ := x.release(order)
	x.mustPoll()
	x.fake.mine(10)
	x.mustPoll()
	x.fake.mine(100)
	x.mustPoll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE id=$1 AND state='confirmed' AND reorg_safe_at IS NOT NULL", id); n != 1 {
		x.t.Fatalf("release %d not confirmed 100 deep: %+v", id, x.settlement(id))
	}
	return order, addr
}

// rotate moves the watcher's clock to November's start: October's wallet closes (and, with no open order, is retained).
func (x *xmrEscrowEnv) rotate() {
	x.t.Helper()
	x.now = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	x.mustPoll()
}

// days moves the watcher's clock to n days after the last order close recorded for wallet name.
func (x *xmrEscrowEnv) days(name string, n int) {
	x.t.Helper()
	at := agStrings(x.t, x.testEnv, "SELECT to_char(last_closed_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') FROM xmr_escrow_wallets WHERE name=$1", name)[0]
	last, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		x.t.Fatalf("last close of %s: %q", name, at)
	}
	x.now = last.Add(time.Duration(n) * 24 * time.Hour)
}

// walletState is wallet name's state.
func (x *xmrEscrowEnv) walletState(name string) string {
	return agStrings(x.t, x.testEnv, "SELECT state FROM xmr_escrow_wallets WHERE name=$1", name)[0]
}

// P3-R10: scripts/restore.sh holds every built or relayed Monero settlement (restoredSettlementHold, as Bitcoin
// escrow's), retires every free slot, moves every active (and sealed) wallet to closing and asks for the sweep; it
// reports what it did. No restored wallet issues a slot again: after the sweep the market creates the month's next
// wallet, and checkout claims from it.
func TestXMREscrowRestoreRetiresSlots(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	built, _, _ := x.completed()
	relayed, _, _ := x.completed()
	confirmed, _ := x.settled()
	open, _ := x.freshCheckout(x.product)
	idBuilt, _ := x.release(built)
	x.fake.fault("relay_tx", fakeXMRNotOpen)
	x.poll()
	idRelayed, _ := x.release(relayed)
	x.mustPoll()
	idRequested, _ := x.release(open) // not built: its order is still awaiting payment
	x.now = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	x.mustPoll() // next month's wallet, sealed
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active", "escrow-202611-01:sealed"}) {
		t.Fatalf("before the restore: %v", got)
	}
	free := x.slots("escrow-202610-01", "free")
	held, retired, closed := x.restore()
	if held != 2 || retired != free || closed != 2 {
		t.Fatalf("restore counts: held %d retired %d (of %d free) closed %d", held, retired, free, closed)
	}
	for id, want := range map[int64]string{idBuilt: restoredSettlementHold, idRelayed: restoredSettlementHold, idRequested: ""} {
		if got := x.waitClass(id); got != want {
			t.Errorf("settlement %d (%s): send_error %q", id, x.settlement(id).State, got)
		}
	}
	if got := x.waitClass(x.settlementOf(confirmed, "release").ID); got != "" {
		t.Errorf("a confirmed settlement was held: %q", got)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='free'"); n != 0 {
		t.Fatalf("%d free slots after the restore", n)
	}
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:closing", "escrow-202611-01:closing"}) || !x.sweepRequired() {
		t.Fatalf("after the restore: %v, sweep %v", got, x.sweepRequired())
	}
	// The sweep runs under the gate; nothing is created and no slot is made free.
	x.mustPoll()
	if x.sweepRequired() || agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='free'") != 0 || len(x.wallets()) != 2 {
		t.Fatalf("after the sweep under the gate: %v, sweep %v", x.wallets(), x.sweepRequired())
	}
	x.clearGate()
	x.mustPoll()
	// November's wallet, closed by the restore with no order, is retained.
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:closing", "escrow-202611-01:retained", "escrow-202610-02:active", "escrow-202611-02:sealed"}) {
		t.Fatalf("after the gate: %v", got)
	}
	order := x.draft(x.product)
	x.mustCheckout(order)
	if w := x.walletOf(order); w != "escrow-202610-02" {
		t.Fatalf("checkout after the restore claimed from %s", w)
	}
	// The manual protection in UPGRADING.md section 6 runs the same statements (scripts/test-backup-restore.sh compares
	// the two restored databases).
	for _, f := range []string{"scripts/restore.sh", "UPGRADING.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"UPDATE xmr_settlements SET send_error='" + strings.ReplaceAll(restoredSettlementHold, "'", "''") + "', updated=now()",
			"WHERE state IN ('built','relayed')", "UPDATE xmr_escrow_slots SET state='retired', updated=now() WHERE state='free'",
			"UPDATE xmr_escrow_wallets SET state='closing', updated=now() WHERE state IN ('active','sealed')", "('xmr_escrow_sweep_required','true')"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s lacks %q", f, want)
			}
		}
	}
}

// QA-Xc (P3-R10): after a restore, no account of a restored wallet that the restored records lack (made after the
// backup, perhaps funded by an order they lost) ever becomes a free slot, even when an administrator clears the
// restore gate, and the sweep marker too, before the watcher's first pass: restore.sh moved the wallet to closing, and
// the reconcile adds free slots only to the active wallet.
func TestXMREscrowRestoreNoUnrecordedSlot(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	// After the backup: three more accounts, one of them funded by an order the restored records lack.
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.files["escrow-202610-01"]
		for n := 6; n <= 8; n++ {
			w.accounts = append(w.accounts, testXMRAddr("fake escrow-202610-01 account "+strconv.Itoa(n), true))
		}
	})
	x.fake.pay("escrow-202610-01", 7, hex64("lost order"), xmrListingPrice, 12)
	x.restore()
	x.clearGate()
	agExec(x.testEnv, "DELETE FROM settings WHERE key=$1", xmrEscrowSweepKey)
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT account_index||':'||state FROM xmr_escrow_slots WHERE account_index>5 ORDER BY account_index"); !slices.Equal(got, []string{"6:retired", "7:retired", "8:retired"}) {
		t.Fatalf("unrecorded accounts: %v", got)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE s.state='free' AND w.state<>'active'"); n != 0 {
		t.Fatalf("%d free slots on a wallet that is not active", n)
	}
	if x.notified(x.admin, "account 7, which no order had claimed") != 1 {
		t.Fatal("the funded unrecorded account was not reported")
	}
	for range 5 {
		order, _ := x.freshCheckout(x.product)
		if w := x.walletOf(order); w != "escrow-202610-02" {
			t.Fatalf("a checkout after the restore claimed from %s", w)
		}
	}
}

// P3-R9: a closing wallet whose orders have all closed is retained, with last_closed_at its last order's close and
// destroy_after 90 days later; it is still read, so a deposit to a closed order's escrow within those 90 days is
// returned by a stray refund. It is destroyed only once destroy_after has passed with no open order, no live
// settlement (requested, built or relayed), its last settlement 100 confirmations deep, a zero balance and unlocked
// balance over all accounts, and nothing in the pool; each of those alone keeps it.
func TestXMREscrowDestroyPreconditions(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr := x.settled()
	open, _ := x.freshCheckout(x.product)
	x.rotate()
	const name = "escrow-202610-01"
	if x.walletState(name) != "closing" {
		t.Fatalf("with an open order: %s", x.walletState(name))
	}
	derived := xmrWalletPassword(testXMRSecret, name)
	kept := func(why string) {
		t.Helper()
		x.mustPoll()
		if file, _ := x.fake.file(name); x.walletState(name) == "destroyed" || file.password != derived {
			t.Fatalf("destroyed with %s", why)
		}
	}
	// An open order alone (the wallet forced to retained).
	agExec(x.testEnv, "UPDATE xmr_escrow_wallets SET state='retained' WHERE name=$1", name)
	x.days(name, 91)
	kept("an open order")

	// The open order closes (in that pass, after the wallet's): the wallet is retained from its close, and destroyed no
	// earlier than 90 days after it.
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", open)
	x.mustPoll()
	if x.state(open) != stateCancelled {
		t.Fatalf("the open order: %s", x.state(open))
	}
	x.days(name, 30)
	x.mustPoll()
	if n := agInt(x.testEnv, `SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND state='retained'
		AND last_closed_at=(SELECT max(created) FROM order_events WHERE order_id=$2 AND to_state='cancelled') AND destroy_after=last_closed_at+interval '2160 hours'`, name, open); n != 1 {
		t.Fatalf("retention dates: %v", agStrings(t, x.testEnv, "SELECT state||' '||last_closed_at||' '||destroy_after FROM xmr_escrow_wallets WHERE name=$1", name))
	}
	// Day 89: a deposit to the completed order's escrow is returned to its refund address by a stray refund.
	x.days(name, 89)
	x.fake.payAddress(addr, hex64("day 89"), 70000000000, 12)
	x.mustPoll()
	x.mustPoll()
	stray := x.settlementOf(order, "stray_refund")
	refund := agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
	if stray.State != "relayed" || agStrings(t, x.testEnv, "SELECT destination FROM xmr_settlements WHERE id=$1", stray.ID)[0] != refund {
		t.Fatalf("the day-89 deposit: %+v", stray)
	}
	x.fake.mine(10)
	kept("destroy_after not passed")
	if x.settlement(stray.ID).State != "confirmed" {
		t.Fatalf("stray refund: %+v", x.settlement(stray.ID))
	}

	// Day 91: the stray refund is not 100 confirmations deep yet.
	x.days(name, 91)
	kept("a settlement fewer than 100 confirmations deep")
	// A live settlement: a release requested on the cancelled order, which the market never builds.
	var live int64
	if err := x.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2) RETURNING id", open, testXMRAddr("claim live", true)).Scan(&live); err != nil {
		t.Fatal(err)
	}
	x.fake.mine(100)
	kept("a requested settlement")
	if agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE id=$1 AND reorg_safe_at IS NOT NULL", stray.ID) != 1 {
		t.Fatal("the stray refund is not 100 confirmations deep")
	}
	// A balance: coins at an account no order claimed.
	unclaimed := agStrings(t, x.testEnv, "SELECT s.account_index::text FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE w.name=$1 AND s.state='retired' ORDER BY s.account_index LIMIT 2", name)
	a1, _ := strconv.ParseInt(unclaimed[0], 10, 64)
	a2, _ := strconv.ParseInt(unclaimed[1], 10, 64)
	x.fake.pay(name, a1, hex64("stray coins"), 5000000000, 12)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='superseded' WHERE id=$1", live)
	kept("a balance")
	// Nothing but a transfer in the pool.
	x.fake.pay(name, a2, hex64("pool coins"), 5000000000, 0)
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.files[name]
		w.transfers = slices.DeleteFunc(w.transfers, func(t *fakeXMRTransfer) bool { return t.TxID == hex64("stray coins") })
	})
	kept("a transfer in the pool")
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.files[name]
		w.transfers = slices.DeleteFunc(w.transfers, func(t *fakeXMRTransfer) bool { return t.TxID == hex64("pool coins") })
	})
	x.mustPoll()
	if got := x.walletState(name); got != "destroyed" {
		t.Fatalf("with every precondition met: %s", got)
	}
}

// P3-R9: destruction changes the wallet's password to 32 random bytes that are stored nowhere, stores and closes it,
// then wipes the sealed seed and records destroyed_at in one transaction; the wallet is never opened again, and /admin
// lists its files for the operator to delete.
func TestXMREscrowDestroyShreds(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	const name = "escrow-202610-01"
	x.rotate()
	if got := x.walletState(name); got != "retained" {
		t.Fatalf("a closed wallet with no order: %s", got)
	}
	x.days(name, 90)
	x.now = x.now.Add(-time.Minute)
	x.mustPoll()
	if x.walletState(name) != "retained" {
		t.Fatal("destroyed before destroy_after")
	}
	logs := captureLog(t)
	x.days(name, 90)
	derived := xmrWalletPassword(testXMRSecret, name)
	from := len(x.fake.callLog())
	x.mustPoll()
	file, _ := x.fake.file(name)
	if file.password == derived || len(file.password) != 64 {
		t.Fatalf("the password was not replaced by 32 random bytes: %d characters", len(file.password))
	}
	var seq []string
	calls := x.fake.callLog()[from:]
	if i := slices.IndexFunc(calls, func(c fakeXMRCall) bool { return c.Method == "change_wallet_password" }); i >= 0 && i+2 < len(calls) {
		seq = []string{calls[i].Method, calls[i+1].Method, calls[i+2].Method}
		if calls[i].Params != `{"new_password":"`+file.password+`","old_password":"`+derived+`"}` {
			t.Fatal("change_wallet_password was not sent the derived password and the new one")
		}
	}
	if !slices.Equal(seq, []string{"change_wallet_password", "store", "close_wallet"}) {
		t.Fatalf("destruction calls %v", seq)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND state='destroyed' AND seed_sealed IS NULL AND destroyed_at=$2", name, x.now); n != 1 {
		t.Fatalf("the row: %v", agStrings(t, x.testEnv, "SELECT state||' '||COALESCE(destroyed_at::text,'-')||' '||(seed_sealed IS NULL) FROM xmr_escrow_wallets WHERE name=$1", name))
	}
	for _, q := range []string{"SELECT string_agg(key||value,'') FROM settings", "SELECT string_agg(action,'') FROM audit_events", "SELECT string_agg(body,'') FROM notifications",
		"SELECT string_agg(note,'') FROM order_events", "SELECT string_agg(row_to_json(w)::text,'') FROM xmr_escrow_wallets w"} {
		var all *string
		if err := x.DB.QueryRow(q).Scan(&all); err != nil {
			t.Fatal(err)
		}
		if all != nil && strings.Contains(*all, file.password) {
			t.Fatalf("the new password is stored (%s)", q)
		}
	}
	if strings.Contains(logs.String(), file.password) || !strings.Contains(logs.String(), name) {
		t.Fatalf("log:\n%s", logs.String())
	}
	before := x.opens(name)
	x.mustPoll()
	x.now = x.now.AddDate(0, 2, 0)
	x.mustPoll()
	if x.opens(name) != before {
		t.Fatal("a destroyed wallet was opened again")
	}
	_, adminSess := x.user("xf_admin_"+randomToken()[:6], "admin")
	page := html.UnescapeString(x.page("/admin", adminSess))
	if !strings.Contains(page, name) || !strings.Contains(page, "Destroyed Monero escrow wallets") || !strings.Contains(page, name+".keys") {
		t.Fatalf("/admin does not list the destroyed wallet's files:\n%s", page)
	}
}

// ES-8: once the wallet that held an order's account is destroyed, the order page says that Monero sent to the order's
// address after the destruction cannot be recovered; before, it does not.
func TestXMREscrowAfterDestroyText(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	x.rotate()
	const name = "escrow-202610-01"
	sess := x.sessOf(order, roleBuyer)
	if p := x.panel(order, sess); strings.Contains(p, "cannot be recovered") {
		t.Fatalf("before destruction:\n%s", p)
	}
	x.days(name, 91)
	x.mustPoll()
	if x.walletState(name) != "destroyed" {
		t.Fatalf("not destroyed: %s", x.walletState(name))
	}
	want := "Monero sent to this order's address after " + x.now.Format("2006-01-02 15:04 UTC") + " cannot be recovered"
	for _, viewer := range []string{roleBuyer, roleVendor} {
		if p := x.panel(order, x.sessOf(order, viewer)); !strings.Contains(p, want) {
			t.Fatalf("%s's page lacks %q:\n%s", viewer, want, p)
		}
	}
}

// P3-R9: a restored row past destroy_after whose wallet no longer opens (destroyed after the backup) is destroyed
// again, its restored seal wiped, once the restored records allow its destruction; until they do (a live settlement)
// it is missing and the administrators are told, once.
func TestXMREscrowRestoredPastDestroy(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.settled()
	cancelled, _ := x.freshCheckout(x.product)
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", cancelled)
	x.mustPoll()
	var live int64
	if err := x.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2) RETURNING id", cancelled, testXMRAddr("claim restored", true)).Scan(&live); err != nil {
		t.Fatal(err)
	}
	x.rotate()
	const name = "escrow-202610-01"
	if x.walletState(name) != "retained" {
		t.Fatalf("closed: %s", x.walletState(name))
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.files[name].password = "replaced after the backup" })
	x.days(name, 91)
	x.mustPoll()
	x.mustPoll()
	if got := x.walletState(name); got != "missing" || x.notified(x.admin, name) != 1 {
		t.Fatalf("with a live settlement: %s, %d notices", got, x.notified(x.admin, name))
	}
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='superseded' WHERE id=$1", live)
	x.mustPoll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND state='destroyed' AND seed_sealed IS NULL AND destroyed_at IS NOT NULL", name); n != 1 {
		t.Fatalf("not destroyed again: %s", x.walletState(name))
	}
	if f, _ := x.fake.file(name); f.password != "replaced after the backup" {
		t.Fatal("a wallet that does not open was written to")
	}
}

// P3-R9: a wallet not yet due that cannot be opened becomes missing (its free slots retired) and the administrators are
// told once; the month gets its next wallet; when the file opens again the wallet is read again, closing.
func TestXMREscrowMissingWalletAlerts(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr := x.freshCheckout(x.product)
	const name = "escrow-202610-01"
	var file *fakeXMRWalletFile
	x.fake.edit(func(f *fakeXMREscrow) { file = f.files[name]; delete(f.files, name) })
	x.mustPoll()
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{name + ":missing", "escrow-202610-02:active"}) {
		t.Fatalf("with its file gone: %v", got)
	}
	if x.notified(x.admin, name) != 1 || x.slots(name, "free") != 0 {
		t.Fatalf("notices %d, free slots %d", x.notified(x.admin, name), x.slots(name, "free"))
	}
	other, _ := x.freshCheckout(x.product)
	if w := x.walletOf(other); w != "escrow-202610-02" {
		t.Fatalf("checkout claimed from %s", w)
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.files[name] = file })
	x.fake.payAddress(addr, hex64("back"), xmrListingPrice, 10)
	x.mustPoll()
	if x.walletState(name) != "closing" || x.state(order) != stateCompleted {
		t.Fatalf("with its file back: %s, order %s", x.walletState(name), x.state(order))
	}
}

// P3-R10: the sweep probes the wallet names after the newest row. A wallet that opens with the derived password and
// passes the network check is recorded as an orphan, its seed sealed to the pinned key, its funded accounts listed for
// the administrators, and it is retained: never a slot. A file that does not open is recorded as an orphan and left
// alone; the probe goes on past it.
func TestXMREscrowOrphanSwept(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	ours := func(name string, accounts int) {
		x.fake.edit(func(f *fakeXMREscrow) {
			w := f.newFile(name, xmrWalletPassword(testXMRSecret, name))
			for n := 1; n <= accounts; n++ {
				w.accounts = append(w.accounts, testXMRAddr("fake "+name+" account "+strconv.Itoa(n), true))
			}
		})
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.newFile("escrow-202610-02", "someone else's password") })
	ours("escrow-202610-03", 3)
	x.fake.pay("escrow-202610-03", 2, hex64("lost escrow"), xmrListingPrice, 12)
	ours("escrow-202611-01", 2)
	x.restore()
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:retained", "escrow-202610-02:orphan", "escrow-202610-03:retained", "escrow-202611-01:retained"}) {
		t.Fatalf("after the sweep: %v", got)
	}
	for _, name := range []string{"escrow-202610-03", "escrow-202611-01"} {
		file, _ := x.fake.file(name)
		if x.unseal(name) != file.mnemonic {
			t.Fatalf("%s: the seal is not its seed", name)
		}
		row := agStrings(t, x.testEnv, "SELECT primary_address||' '||restore_height||' '||seed_key_fpr FROM xmr_escrow_wallets WHERE name=$1", name)[0]
		if row != file.accounts[0]+" 0 "+x.A.escrowXMR.seedKey.fpr {
			t.Fatalf("%s: %q", name, row)
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_wallets WHERE name='escrow-202610-02' AND seed_sealed IS NULL") != 1 || x.notified(x.admin, "escrow-202610-02") != 1 {
		t.Fatal("the file that does not open")
	}
	if x.notified(x.admin, "wallet escrow-202610-03 was found") != 1 || x.notified(x.admin, "account 2 (0.5 XMR)") != 1 {
		t.Fatalf("the orphan's funded account was not listed: %v", agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1", x.admin))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE w.name IN ('escrow-202610-03','escrow-202611-01') AND s.state<>'retired'"); n != 0 {
		t.Fatalf("%d slots of an orphan are not retired", n)
	}
	if x.sweepRequired() {
		t.Fatal("the sweep did not finish")
	}
	x.clearGate()
	x.mustPoll()
	order, _ := x.freshCheckout(x.product)
	if w := x.walletOf(order); w != "escrow-202610-04" {
		t.Fatalf("checkout claimed from %s", w)
	}
	x.mustPoll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE w.name IN ('escrow-202610-03','escrow-202611-01') AND s.state<>'retired'"); n != 0 {
		t.Fatalf("%d slots of an orphan are not retired", n)
	}
}

// A-418: when create_wallet finds the file of a row left creating but open_wallet refuses it (a file left under another
// password), the row becomes an orphan, the administrators are told once and the month's next wallet is created; a
// transport error on open_wallet keeps the row creating and is retried.
func TestXMREscrowResumeOpenFailsOrphan(t *testing.T) {
	x := newXMREscrowEnv(t)
	const name = "escrow-202610-01"
	x.fake.edit(func(f *fakeXMREscrow) { f.newFile(name, "an earlier install's password") })
	agExec(x.testEnv, "INSERT INTO xmr_escrow_wallets(name,month) VALUES($1,'2026-10-01')", name)
	x.fake.fault("open_wallet", fakeXMRDrop)
	if err := x.poll(); err == nil {
		t.Fatal("a lost open_wallet answer passed")
	}
	if got := x.wallets(); !slices.Equal(got, []string{name + ":creating"}) || x.notified(x.admin, name) != 0 {
		t.Fatalf("after a transport error: %v", got)
	}
	x.mustPoll()
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{name + ":orphan", "escrow-202610-02:active"}) || x.notified(x.admin, name) != 1 {
		t.Fatalf("after open_wallet refused: %v, %d notices", got, x.notified(x.admin, name))
	}
	if x.slots("escrow-202610-02", "free") != 5 {
		t.Fatal("the next wallet has no free slots")
	}
	if f, _ := x.fake.file(name); f.password != "an earlier install's password" {
		t.Fatal("the foreign file was changed")
	}
}

// P3-R10, P3-R6: a settlement held by a restore is listed on /admin; an administrator confirms, with a password step-up
// and an audit row, that its stored transaction was not already relayed or replaced, and the watcher then relays the
// stored metadata once, under the gate too, and never builds it again.
func TestXMREscrowHeldSettlementConfirm(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.completed()
	lost, _, _ := x.completed()
	idLost, _ := x.release(lost)
	x.mustPoll()
	hash := x.settlement(idLost).TxHash
	x.fake.flushPool(hash) // relayed before the backup, then dropped by the daemon
	id, _ := x.release(order)
	x.fake.fault("relay_tx", fakeXMRNotOpen) // the lost one's relay again
	x.fake.fault("relay_tx", fakeXMRNotOpen) // the new one's first relay: it stays built
	x.poll()
	if x.settlement(id).State != "built" || x.settlement(idLost).State != "relayed" {
		t.Fatalf("before the restore: %+v %+v", x.settlement(id), x.settlement(idLost))
	}
	x.restore()
	transfers, relays := x.fake.countOf("transfer"), x.fake.countOf("relay_tx")
	x.mustPoll()
	x.mustPoll()
	if x.fake.countOf("relay_tx") != relays || x.settlement(id).State != "built" {
		t.Fatalf("a held settlement was relayed: %+v", x.settlement(id))
	}
	adminID, adminSess := x.user("xf_admin_"+randomToken()[:6], "admin")
	_, modSess := x.user("xf_mod_"+randomToken()[:6], "moderator")
	page := html.UnescapeString(x.page("/admin", adminSess))
	if !strings.Contains(page, `action="/admin/escrow-xmr-rebroadcast"`) || !strings.Contains(page, shortID(order)) || !strings.Contains(page, shortID(lost)) {
		t.Fatalf("/admin does not list the held settlements:\n%s", page)
	}
	confirm := func(sess string, s int64, form url.Values) int {
		f := url.Values{"settlement_id": {strconv.FormatInt(s, 10)}, "not_sent": {"confirmed"}, "password": {testPassword}}
		for k, v := range form {
			f[k] = v
		}
		return x.do("POST", "/admin/escrow-xmr-rebroadcast", sess, f).Code
	}
	for what, code := range map[string]int{
		"a moderator":       confirm(modSess, id, nil),
		"no password":       confirm(adminSess, id, url.Values{"password": {""}}),
		"wrong password":    confirm(adminSess, id, url.Values{"password": {"not the password"}}),
		"no explicit check": confirm(adminSess, id, url.Values{"not_sent": {""}}),
	} {
		if code == 303 || x.waitClass(id) != restoredSettlementHold {
			t.Fatalf("%s confirmed: %d", what, code)
		}
	}
	if code := confirm(adminSess, id, nil); code != 303 {
		t.Fatalf("confirmation: %d", code)
	}
	if code := confirm(adminSess, id, nil); code != 409 {
		t.Fatalf("a second confirmation: %d", code)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", adminID, "Confirmed the relay of Monero escrow settlement "+strconv.FormatInt(id, 10)+"%order "+shortID(order)+"%") != 1 {
		t.Fatal("no audit row for the confirmation")
	}
	if !strings.Contains(strings.Join(x.events(order), "\n"), "An administrator checked that this Monero escrow settlement's transaction was not already relayed or replaced") {
		t.Fatal("no order note for the confirmation")
	}
	x.mustPoll()
	x.mustPoll()
	if x.fake.countOf("relay_tx") != relays+1 || x.fake.countOf("transfer") != transfers || x.settlement(id).State != "relayed" || x.waitClass(id) != "" {
		t.Fatalf("after the confirmation: %d relays, %d transfers, %+v %q", x.fake.countOf("relay_tx")-relays, x.fake.countOf("transfer")-transfers, x.settlement(id), x.waitClass(id))
	}
	// The relayed one the daemon lost: relayed again once after its confirmation, with the same transaction.
	if code := confirm(adminSess, idLost, nil); code != 303 {
		t.Fatalf("confirmation of the relayed settlement: %d", code)
	}
	x.mustPoll()
	x.mustPoll()
	if x.fake.countOf("relay_tx") != relays+2 || x.fake.countOf("transfer") != transfers || x.waitClass(idLost) != "" {
		t.Fatalf("the relayed settlement: %d relays, %d transfers, %q", x.fake.countOf("relay_tx")-relays, x.fake.countOf("transfer")-transfers, x.waitClass(idLost))
	}
	if tx, ok := x.fake.tx(hash); !ok || tx.Height != 0 {
		t.Fatal("the stored transaction is not back in the pool")
	}
}

// P3-R10: after a restore, Monero escrow checkout answers 503 until the sweep has run in full: a probe or a wallet the
// sweep could not read leaves the marker set.
func TestXMREscrowSweepGatesCheckout(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.restore()
	x.clearGate()
	draft := x.draft(x.product)
	refused := func(when string) {
		t.Helper()
		w := x.checkout(x.buyerSess, draft, testXMRAddr("refund "+draft, true))
		if w.Code != 503 || !strings.Contains(html.UnescapeString(w.Body.String()), "has checked its escrow wallets after restoring its records from a backup") {
			t.Fatalf("checkout %s: %d %s", when, w.Code, w.Body.String())
		}
	}
	refused("before the sweep")
	x.fake.fault("open_wallet", fakeXMRDrop) // the first probe
	x.poll()
	if !x.sweepRequired() {
		t.Fatal("the sweep marker was cleared after a failed probe")
	}
	refused("after a failed probe")
	x.fake.fault("get_transfers", fakeXMRNotOpen) // the wallet's read
	x.poll()
	if !x.sweepRequired() {
		t.Fatal("the sweep marker was cleared after a failed wallet read")
	}
	x.mustPoll()
	if x.sweepRequired() {
		t.Fatal("the sweep marker stayed after a full sweep")
	}
	x.mustPoll()
	x.mustCheckout(draft)
	if w := x.walletOf(draft); w != "escrow-202610-02" {
		t.Fatalf("checkout claimed from %s", w)
	}
}

// P3-R10: a wallet older than the database (restored from an older copy) does not know accounts the restored records
// have. Beyond its default lookahead a deposit to such an account is missed; the sweep raises the subaddress
// lookahead past the highest slot (with the wallet's password) and rescans, and the deposit is seen and recorded.
func TestXMREscrowLookaheadForOlderWallet(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	const name = "escrow-202610-01"
	addr := testXMRAddr("fake "+name+" account 300", true)
	agExec(x.testEnv, "UPDATE xmr_escrow_slots SET state='retired' WHERE state='free'")
	agExec(x.testEnv, "INSERT INTO xmr_escrow_slots(wallet_id,account_index,address) SELECT id,300,$1 FROM xmr_escrow_wallets WHERE name=$2", addr, name)
	order := x.draft(x.product)
	if got := x.mustCheckout(order); got != addr {
		t.Fatalf("checkout took %s", got)
	}
	x.restore()
	// The wallet file is an older copy: it knows account 0 only.
	x.fake.edit(func(f *fakeXMREscrow) { f.files[name].accounts = f.files[name].accounts[:1] })
	x.fake.pay(name, 300, hex64("deep account"), xmrListingPrice, 12)
	// Read as it is (its own refresh, the default lookahead of 50 accounts), the wallet misses the deposit.
	ctx := t.Context()
	rpc := x.A.escrowXMR.rpc
	if err := rpc.openWallet(ctx, name, xmrWalletPassword(testXMRSecret, name)); err != nil {
		t.Fatal(err)
	}
	if err := rpc.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if tr, err := rpc.transfers(ctx); err != nil || len(tr) != 0 {
		t.Fatalf("the default lookahead saw %d transfers (%v)", len(tr), err)
	}
	x.mustPoll()
	if x.state(order) != stateCompleted {
		t.Fatalf("the deposit to account 300 was not recorded: %s", x.state(order))
	}
	var set string
	for _, c := range x.fake.callLog() {
		if c.Method == "set_subaddress_lookahead" {
			set = c.Params
		}
	}
	if !strings.Contains(set, `"password":"`+xmrWalletPassword(testXMRSecret, name)+`"`) || x.fake.countOf("rescan_blockchain") != 1 {
		t.Fatalf("lookahead %s, %d rescans", set, x.fake.countOf("rescan_blockchain"))
	}
	if x.sweepRequired() {
		t.Fatal("the sweep did not finish")
	}
}
