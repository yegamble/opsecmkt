package market

import (
	"strings"
	"testing"
)

// A market that never ran escrow shows no escrow recovery section on /admin, restore gate or not.
func TestEscrowRecoverySectionHiddenWithoutEscrow(t *testing.T) {
	t.Setenv("ESCROW_BTC", "")
	e := newTestApp(t)
	_, admin := e.user("plainadmin", "admin")
	if _, err := e.DB.Exec("INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'"); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/admin/payments", admin, nil)
	e.check(w, 200)
	if body := w.Body.String(); strings.Contains(body, "escrow-recovery") || strings.Contains(body, "Bitcoin escrow paused") {
		t.Fatalf("/admin on a market without escrow, gate set:\n%s", body)
	}
	if got := agStrings(t, e, "SELECT value FROM settings WHERE key='payments_recovery_required'"); len(got) != 1 || got[0] != "true" {
		t.Fatalf("hiding escrow recovery changed the restore gate: %v", got)
	}
}
