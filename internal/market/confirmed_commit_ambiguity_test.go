package market

import (
	"database/sql"
	"database/sql/driver"
	"net/url"
	"os"
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
