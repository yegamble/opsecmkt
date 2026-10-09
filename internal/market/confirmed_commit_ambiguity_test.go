package market

import (
	"database/sql"
	"database/sql/driver"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
)

// The driver lets Commit reach PostgreSQL and then loses the acknowledgement.
// Querying through the original connection proves the committed result independently
// of the action's HTTP response.
func loseNextActionCommit(t *testing.T, e *testEnv) *atomic.Bool {
	t.Helper()
	base, err := stdlib.GetDefaultDriver().(driver.DriverContext).OpenConnector(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	lost := &atomic.Bool{}
	lost.Store(true)
	wrapped := sql.OpenDB(&lostCommitConnector{Connector: base, lost: lost})
	original := e.DB
	e.A.db = wrapped
	t.Cleanup(func() { original.Close() })
	return lost
}

func TestConfirmedPayoutLostCommitAcknowledgementReportsUnknownOutcome(t *testing.T) {
	p := newPayEnv(t)
	order := p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor", "", false, "")
	id := p.payoutID(order)
	lost := loseNextActionCommit(t, p.testEnv)

	w := p.do("POST", "/admin/payout", p.adminSess, url.Values{
		"payout_id": {id}, "op": {"hold"}, "note": {"Check the wallet before paying by hand"}, "password": {testPassword},
	})
	if lost.Load() {
		t.Fatal("Commit did not reach the injected acknowledgement loss")
	}
	if w.Code != 500 || w.Header().Get("Retry-After") != "" ||
		!strings.Contains(w.Body.String(), "Payout outcome unknown") ||
		!strings.Contains(w.Body.String(), "may have been saved") ||
		!strings.Contains(w.Body.String(), "check the payout state and audit trail") ||
		strings.Contains(w.Body.String(), "Nothing was changed") {
		t.Fatalf("lost Commit acknowledgement: status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	var state string
	if err := p.DB.QueryRow("SELECT state FROM payouts WHERE id=$1", id).Scan(&state); err != nil || state != "held" {
		t.Fatalf("payout state = %q, err=%v; want committed hold", state, err)
	}
	var audits, events int
	if err := p.DB.QueryRow("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Held pending payout "+id+"%").Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("held payout audits = %d, err=%v; want 1", audits, err)
	}
	if err := p.DB.QueryRow("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%held by an administrator%'", order).Scan(&events); err != nil || events != 1 {
		t.Fatalf("held payout events = %d, err=%v; want 1", events, err)
	}
}

func TestTOTPDisableLostCommitAcknowledgementReportsUnknownOutcome(t *testing.T) {
	e := newTestApp(t)
	id, session := e.user("lost_totp_disable", "buyer")
	key, _ := agEnableTOTP(e, id)
	lost := loseNextActionCommit(t, e)

	form := url.Values{"password": {testPassword}, "code": {agTOTPNow(key)}, formNonceField: {randomToken()[:32]}}
	w := e.do("POST", "/totp/disable", session, form)
	if lost.Load() {
		t.Fatal("Commit did not reach the injected acknowledgement loss")
	}
	if w.Code != 500 || w.Header().Get("Retry-After") != "" ||
		!strings.Contains(w.Body.String(), "Authenticator change outcome unknown") ||
		!strings.Contains(w.Body.String(), "may have been saved") ||
		!strings.Contains(w.Body.String(), "Check your authenticator settings") ||
		!strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost Commit acknowledgement: status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	var enabled bool
	var recovery, audits, notices int
	if err := e.DB.QueryRow("SELECT totp_enabled FROM users WHERE id=$1", id).Scan(&enabled); err != nil || enabled {
		t.Fatalf("TOTP enabled=%t, err=%v; want disabled", enabled, err)
	}
	if err := e.DB.QueryRow("SELECT count(*) FROM recovery_codes WHERE user_id=$1", id).Scan(&recovery); err != nil || recovery != 0 {
		t.Fatalf("recovery codes=%d, err=%v; want none", recovery, err)
	}
	if err := e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Turned off TOTP%'", id).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("TOTP-disable audits=%d, err=%v; want 1", audits, err)
	}
	if err := e.DB.QueryRow("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Two-step sign-in%turned off%'", id).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("TOTP-disable notices=%d, err=%v; want 1", notices, err)
	}
	repeated := e.do("POST", "/totp/disable", session, form)
	e.check(repeated, 303)
	if got := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Turned off TOTP%'", id); got != 1 {
		t.Fatalf("replay after lost acknowledgment duplicated audit: %d", got)
	}
}

func TestConfirmedPayoutPrecommitBusyStaysRetryable(t *testing.T) {
	p := newPayEnv(t)
	order := p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor", "", false, "")
	id := p.payoutID(order)
	lock, err := p.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	var locked int64
	if err := lock.QueryRow("SELECT id FROM payouts WHERE id=$1 FOR UPDATE", id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	w := p.do("POST", "/admin/payout", p.adminSess, url.Values{
		"payout_id": {strconv.FormatInt(locked, 10)}, "op": {"hold"}, "note": {"Check the wallet before paying by hand"}, "password": {testPassword},
	})
	if w.Code != 503 || w.Header().Get("Retry-After") != busyRetryAfter || !strings.Contains(w.Body.String(), errBusyMsg) {
		t.Fatalf("precommit busy: status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	var state string
	if err := p.DB.QueryRow("SELECT state FROM payouts WHERE id=$1", id).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("payout state = %q, err=%v; want unchanged pending", state, err)
	}
	var audits int
	if err := p.DB.QueryRow("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Held pending payout "+id+"%").Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("held payout audits=%d, err=%v; want none", audits, err)
	}
}
