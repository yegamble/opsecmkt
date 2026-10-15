package market

import (
	"net/url"

	"strconv"
	"strings"
	"testing"
)

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

// A market that never ran escrow shows no escrow recovery section on /admin, restore gate or not.
func TestEscrowRecoverySectionHiddenWithoutEscrow(t *testing.T) {
	t.Setenv("ESCROW_BTC", "")
	e := newTestApp(t)
	_, admin := e.user("plainadmin", "admin")
	if _, err := e.DB.Exec("INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'"); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/admin", admin, nil)
	e.check(w, 200)
	if body := w.Body.String(); strings.Contains(body, "escrow-recovery") || strings.Contains(body, "Bitcoin escrow paused") {
		t.Fatalf("/admin on a market without escrow, gate set:\n%s", body)
	}
	if got := agStrings(t, e, "SELECT value FROM settings WHERE key='payments_recovery_required'"); len(got) != 1 || got[0] != "true" {
		t.Fatalf("hiding escrow recovery changed the restore gate: %v", got)
	}
}
