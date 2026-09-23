package market

import (
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Two sessions of one account change the password at the same time. The confirmation locks the account
// row, so the changes run one after the other; the first ends the second's session, and the second must
// then be refused rather than overwrite the password on a revoked session.
func TestPasswordChangeConcurrentRevokedSession(t *testing.T) {
	e := newTestApp(t)
	id, first := e.user("pw_race_user", "buyer")
	second := agSession(e, id)
	tx, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT 1 FROM users WHERE id=$1 FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	type result struct {
		next string
		code int
	}
	done := make(chan result, 2)
	for _, s := range []struct{ token, next string }{{first, "first-new-password-1"}, {second, "second-new-password-2"}} {
		go func() {
			done <- result{s.next, e.do("POST", "/account/password", s.token, pwForm(testPassword, s.next)).Code}
		}()
	}
	// Both requests have passed the password check and wait on the account row inside their transactions.
	deadline := time.Now().Add(10 * time.Second)
	for agInt(e, "SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'SELECT totp_enabled FROM users WHERE id=$1 FOR NO KEY UPDATE%' AND wait_event_type='Lock'") < 2 {
		if time.Now().After(deadline) {
			t.Fatal("password changes did not both reach the account lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	a, b := <-done, <-done
	if a.code == b.code || (a.code != 303 && a.code != 401) || (b.code != 303 && b.code != 401) {
		t.Fatalf("want one 303 and one 401, got %d and %d", a.code, b.code)
	}
	winner, loser := a, b
	if b.code == 303 {
		winner, loser = b, a
	}
	hash := []byte(passwordHash(e, id))
	if bcrypt.CompareHashAndPassword(hash, []byte(winner.next)) != nil || bcrypt.CompareHashAndPassword(hash, []byte(loser.next)) == nil {
		t.Fatal("the refused change altered the password, or the accepted one did not apply")
	}
	if n := agInt(e, "SELECT count(*) FROM sessions WHERE user_id=$1", id); n != 1 {
		t.Fatalf("want only the winner's rotated session, got %d sessions", n)
	}
	if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Changed password%'", id); n != 1 {
		t.Fatalf("want one password-change audit row, got %d", n)
	}
}
