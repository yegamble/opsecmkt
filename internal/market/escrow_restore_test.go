package market

import (
	"html"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Escrow Phase 1, slice P1-d1 (ES-6): the restore gate pauses escrow co-signing, builds and broadcasts, and an
// administrator confirms each held rebroadcast. Fake escrow Core (simulated RPC, not chain evidence).

// restoreHold is what scripts/restore.sh (and the manual protection SQL in UPGRADING.md section 6) writes on every
// restored escrow settlement that has a final transaction (restoredSettlementHold).
func (x *escrowEnv) restoreHold(ids ...int64) {
	x.t.Helper()
	for _, id := range ids {
		if _, err := x.DB.Exec("UPDATE settlements SET send_error=$2 WHERE id=$1 AND raw_tx IS NOT NULL AND state IN ('signed','broadcast')", id, restoredSettlementHold); err != nil {
			x.t.Fatal(err)
		}
	}
}

// setGate sets or clears the restore gate (payments_recovery_required), as scripts/restore.sh and the runbook's
// clear-gate SQL do.
func (x *escrowEnv) setGate(on bool) {
	x.t.Helper()
	if _, err := x.DB.Exec("INSERT INTO settings(key,value) VALUES('payments_recovery_required',$1) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", strconv.FormatBool(on)); err != nil {
		x.t.Fatal(err)
	}
}

// sendsOf counts the sendrawtransaction calls carrying raw.
func (x *escrowEnv) sendsOf(raw string) int {
	x.core.mu.Lock()
	defer x.core.mu.Unlock()
	n := 0
	for _, c := range x.core.calls {
		if c.Method == "sendrawtransaction" && strings.Contains(string(c.Params), `"`+raw+`"`) {
			n++
		}
	}
	return n
}

// rebroadcast posts the administrator's confirmation that a held settlement's transaction was not already sent or
// replaced.
func (x *escrowEnv) rebroadcast(sess string, id int64, form url.Values) (int, string) {
	f := url.Values{"settlement_id": {strconv.FormatInt(id, 10)}}
	for k, v := range form {
		f[k] = v
	}
	w := x.do("POST", "/admin/escrow-rebroadcast", sess, f)
	return w.Code, w.Body.String()
}

// ES-6: after a restore from backup (the restore gate set, restored settlements with a final transaction held by
// scripts/restore.sh), the escrow watcher builds nothing, co-signs nothing and broadcasts nothing, while it still
// follows broadcast settlements to their confirmations; the order pages and /admin say why. An administrator confirms
// on /admin, with password step-up and an audit row, that a held settlement's transaction was not already sent or
// replaced, and the watcher then broadcasts it exactly once. Once the gate is cleared the market builds, co-signs and
// broadcasts again, but a held settlement still waits for its confirmation.
func TestEscrowRestoreGatePausesAndAdminRebroadcasts(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("radmin", "admin")
	_, mod := x.user("rmod", "moderator")
	// D: broadcast, one confirmation for now (not sent again at 1 or 2).
	orderD, idD := x.built(regtestAddr("refund-rd"), regtestAddr("claim-rd"), 100000)
	x.signAs(orderD, roleVendor, idD)
	x.poll()
	d := x.settleByID(idD)
	x.core.setTxConfs(d.TxID, 1, "")
	orderA := x.completed(regtestAddr("refund-ra"), 100000)
	orderB, idB := x.built(regtestAddr("refund-rb"), regtestAddr("claim-rb"), 100000)
	orderC, idC := x.built(regtestAddr("refund-rc"), regtestAddr("claim-rc"), 100000)
	// C: co-signed and stored, its broadcast killed.
	x.signAs(orderC, roleVendor, idC)
	x.A.escrowPollOnce(x.killAtSend())
	x.sendNormally()
	c := x.settleByID(idC)
	// sendsOf counts the killed attempt too (the node never received it); every count below is one above the real sends.
	if c.State != "signed" || c.Raw == "" || x.sendsOf(c.Raw) != 1 {
		t.Fatalf("C before the restore: %+v", c)
	}
	// B: party-signed; A: a claim requested, not built.
	x.signAs(orderB, roleVendor, idB)
	if code, body := x.claim(orderA, x.vendorSess, regtestAddr("claim-ra")); code != 303 {
		t.Fatalf("claim A: %d %s", code, body)
	}
	idA, _, stateA, _ := x.settlement(orderA)
	if stateA != "building" || x.settleByID(idB).State != "party_signed" {
		t.Fatalf("before the restore: A %s, B %+v", stateA, x.settleByID(idB))
	}
	x.core.setTxConfs(d.TxID, 0, "") // unconfirmed again: without the gate it would be sent again

	// The restore: the gate and the holds scripts/restore.sh writes.
	x.setGate(true)
	x.restoreHold(idC, idD)
	calls := len(x.core.methods())
	x.poll()
	x.poll()
	for _, m := range x.core.methods()[calls:] {
		if slices.Contains([]string{"createpsbt", "utxoupdatepsbt", "walletprocesspsbt", "combinepsbt", "finalizepsbt", "testmempoolaccept", "sendrawtransaction", "keypoolrefill"}, m) {
			t.Fatalf("under the restore gate the watcher called %s", m)
		}
	}
	for id, want := range map[int64]string{idA: "building", idB: "party_signed", idC: "signed", idD: "broadcast"} {
		if r := x.settleByID(id); r.State != want {
			t.Fatalf("under the gate settlement %d: %+v, want %s", id, r, want)
		}
	}
	// The pages say what is paused and why.
	const why = "restored its records from a backup"
	for order, want := range map[string]string{orderA: "builds no settlement", orderB: "co-signs it only once", orderC: "not already sent or replaced", orderD: "not already sent or replaced"} {
		for _, sess := range []string{x.buyerSess, x.vendorSess} {
			page := html.UnescapeString(x.page("/order?id="+order, sess))
			if !strings.Contains(page, why) || !strings.Contains(page, want) || !strings.Contains(page, "Paused after a restore from backup") {
				t.Fatalf("order page of %s under the gate lacks %q:\n%s", shortID(order), want, page)
			}
		}
	}
	adminPage := html.UnescapeString(x.page("/admin", admin))
	for _, want := range []string{"Bitcoin escrow paused for restore reconciliation", "builds, co-signs and broadcasts no escrow settlement", shortID(orderC), shortID(orderD), `action="/admin/escrow-rebroadcast"`} {
		if !strings.Contains(adminPage, want) {
			t.Fatalf("/admin under the gate lacks %q:\n%s", want, adminPage)
		}
	}
	// Watching continues: D confirms, and its hold ends with it.
	x.core.setTxConfs(d.TxID, 3, "")
	x.poll()
	if r := x.settleByID(idD); r.State != "confirmed" || r.Error != "" {
		t.Fatalf("D confirmed under the gate: %+v", r)
	}

	// The administrator's confirmation: administrators only, the explicit check, password step-up.
	ok := url.Values{"not_sent": {"confirmed"}, "password": {testPassword}}
	for _, c := range []struct {
		sess string
		form url.Values
		code int
	}{
		{mod, ok, 403},
		{x.buyerSess, ok, 403},
		{admin, url.Values{"password": {testPassword}}, 400},
		{admin, url.Values{"not_sent": {"confirmed"}}, 400}, // no password
		{admin, url.Values{"not_sent": {"confirmed"}, "password": {"wrong-password-here"}}, 401},
	} {
		if code, body := x.rebroadcast(c.sess, idC, c.form); code != c.code {
			t.Fatalf("rebroadcast %v: %d, want %d: %s", c.form, code, c.code, body)
		}
	}
	if code, _ := x.rebroadcast(admin, idD, ok); code != 409 {
		t.Fatalf("rebroadcast of a confirmed settlement: %d", code)
	}
	if code, _ := x.rebroadcast(admin, 999999, ok); code != 404 {
		t.Fatalf("rebroadcast of an unknown settlement: %d", code)
	}
	if r := x.settleByID(idC); r.Error != restoredSettlementHold {
		t.Fatalf("a refused confirmation changed C: %+v", r)
	}
	if code, body := x.rebroadcast(admin, idC, ok); code != 303 {
		t.Fatalf("rebroadcast: %d %s", code, body)
	}
	if code, _ := x.rebroadcast(admin, idC, ok); code != 409 {
		t.Fatalf("a second confirmation: %d", code)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", adminID, "Confirmed the rebroadcast of escrow settlement "+strconv.FormatInt(idC, 10)+"%order "+shortID(orderC)+"%not already sent or replaced%") != 1 {
		t.Fatal("no audit row for the confirmation")
	}
	if ev := strings.Join(x.events(orderC), "\n"); !strings.Contains(ev, "An administrator checked that this escrow settlement's transaction was not already sent or replaced") {
		t.Fatalf("no order note for the confirmation:\n%s", ev)
	}
	x.poll()
	if n := x.sendsOf(c.Raw); n != 2 {
		t.Fatalf("after the confirmation C was sent %d times, want 1 after the killed attempt", n)
	}
	if r := x.settleByID(idC); r.State != "broadcast" || r.Raw != c.Raw {
		t.Fatalf("C after its rebroadcast: %+v", r)
	}
	x.poll()
	x.poll()
	if n := x.sendsOf(c.Raw); n != 2 {
		t.Fatalf("C was sent again under the gate: %d sends", n)
	}
	if x.settleByID(idA).State != "building" || x.settleByID(idB).State != "party_signed" {
		t.Fatal("the confirmation unpaused builds or co-signing")
	}

	// The operator clears the gate: builds, co-signing and broadcasts resume.
	x.setGate(false)
	x.poll()
	if _, _, state, _ := x.settlement(orderA); state != "awaiting_signature" {
		t.Fatalf("A after the gate: %s", state)
	}
	if r := x.settleByID(idB); r.State != "broadcast" {
		t.Fatalf("B after the gate: %+v", r)
	}
	if n := x.sendsOf(c.Raw); n != 3 {
		t.Fatalf("C (unconfirmed) after the gate: %d sends, want 2", n)
	}
	if page := x.page("/order?id="+orderB, x.buyerSess); strings.Contains(page, "Paused after a restore from backup") {
		t.Fatal("the order page still says paused after the gate was cleared")
	}

	// A settlement a restore held still waits for its confirmation after the gate is cleared.
	for _, id := range []int64{idB, idC} {
		x.core.setTxConfs(x.settleByID(id).TxID, 1, "")
	}
	orderE, idE := x.built(regtestAddr("refund-re"), regtestAddr("claim-re"), 100000)
	x.signAs(orderE, roleVendor, idE)
	x.A.escrowPollOnce(x.killAtSend())
	x.sendNormally()
	e := x.settleByID(idE)
	x.restoreHold(idE)
	x.poll()
	x.poll()
	if n := x.sendsOf(e.Raw); n != 1 || x.settleByID(idE).State != "signed" {
		t.Fatalf("a held settlement without the gate was sent %d times", n)
	}
	if !strings.Contains(html.UnescapeString(x.page("/order?id="+orderE, x.buyerSess)), "not already sent or replaced") {
		t.Fatal("the held settlement's page does not say why")
	}
	if code, body := x.rebroadcast(admin, idE, ok); code != 303 {
		t.Fatalf("rebroadcast E: %d %s", code, body)
	}
	x.poll()
	if n := x.sendsOf(e.Raw); n != 2 || x.settleByID(idE).State != "broadcast" {
		t.Fatalf("E after its confirmation: %d sends, %+v", n, x.settleByID(idE))
	}
	x.noPayouts(orderE)
}

// The hold text the application matches is the one scripts/restore.sh and the manual protection SQL write.
func TestEscrowRestoreHoldTextMatchesTheScripts(t *testing.T) {
	if !strings.HasPrefix(restoredSettlementHold, restoredHoldPrefix) {
		t.Fatal("the escrow hold does not start with the restore marker")
	}
	sqlText := strings.ReplaceAll(restoredSettlementHold, "'", "''")
	for _, f := range []string{"scripts/restore.sh", "UPGRADING.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "send_error='"+sqlText+"'") || !strings.Contains(string(b), "UPDATE settlements SET") {
			t.Errorf("%s does not hold restored escrow settlements with %q", f, restoredSettlementHold)
		}
	}
}

// Watching continues for a held settlement that is only signed: once the watch-only wallet has its transaction (sent
// after the backup the database was restored from), it is recorded as broadcast without anything being sent, and the
// restore's hold on it ends; a held one the wallet does not know stays signed and unsent.
func TestEscrowHeldSettlementSeenOnTheNetwork(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("refund-seen"), regtestAddr("claim-seen"), 100000)
	x.signAs(order, roleVendor, id)
	x.A.escrowPollOnce(x.killAtSend())
	x.sendNormally()
	r := x.settleByID(id)
	x.setGate(true)
	x.restoreHold(id)
	x.poll()
	if got := x.settleByID(id); got.State != "signed" || x.sendsOf(r.Raw) != 1 {
		t.Fatalf("held, unknown to the wallet: %+v, %d sends (1 is the killed attempt)", got, x.sendsOf(r.Raw))
	}
	x.core.mu.Lock()
	if x.core.broadcast == nil {
		x.core.broadcast, x.core.txConfs = map[string]string{}, map[string]int64{}
	}
	x.core.broadcast[r.TxID], x.core.txConfs[r.TxID] = r.Raw, 0 // sent after the backup: in the mempool
	x.core.mu.Unlock()
	x.poll()
	got := x.settleByID(id)
	if got.State != "broadcast" || got.Error != "" || x.sendsOf(r.Raw) != 1 {
		t.Fatalf("held, in the wallet: %+v, %d sends", got, x.sendsOf(r.Raw))
	}
	if ev := strings.Join(x.events(order), "\n"); !strings.Contains(ev, "is already on the regtest network: the market's watch-only wallet has it, although the market did not send it") {
		t.Fatalf("no honest note:\n%s", ev)
	}
	x.poll()
	if x.sendsOf(r.Raw) != 1 {
		t.Fatal("sent under the gate after it was seen")
	}
}
