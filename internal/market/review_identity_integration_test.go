package market

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPGPLoginCodeRejectedAfterKeyRotation(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("rotation_user", "buyer")
	first, firstPub := testPGPKey(t, "first")
	second, secondPub := testPGPKey(t, "second")
	saveAndVerify := func(key *openpgp.Entity, public string) {
		t.Helper()
		e.check(e.do("POST", "/account", session, form("pgp", public, "password", testPassword)), 303)
		e.check(e.do("POST", "/pgp/challenge", session, form("kind", "decrypt")), 303)
		var challenge string
		if err := e.DB.QueryRow("SELECT challenge FROM pgp_challenges").Scan(&challenge); err != nil {
			t.Fatal(err)
		}
		e.check(e.do("POST", "/pgp/verify", session, form("response", testDecrypt(t, key, challenge))), 303)
		e.check(e.do("POST", "/pgp/2fa", session, form("enable", "1", "password", testPassword)), 303)
	}
	saveAndVerify(first, firstPub)
	anon := randomToken()
	login := e.do("POST", "/login", anon, form("handle", "rotation_user", "password", testPassword))
	e.check(login, 303)
	pending := &http.Cookie{Name: "pending", Value: e.cookie(login, "pending")}
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pending), 303)
	var challenge string
	if err := e.DB.QueryRow("SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pending.Value)).Scan(&challenge); err != nil {
		t.Fatal(err)
	}
	oldCode := testDecrypt(t, first, challenge)
	saveAndVerify(second, secondPub)
	e.check(e.do("POST", "/challenge", anon, form("method", "pgp", "pgp_code", oldCode), pending), 401)
	// Restoring the original public key creates a new ownership proof; an old
	// code must not become valid again just because its fingerprint matches.
	saveAndVerify(first, firstPub)
	e.check(e.do("POST", "/challenge", anon, form("method", "pgp", "pgp_code", oldCode), pending), 401)
	// A newly generated code still works; rotating a key does not strand sign-in.
	e.check(e.do("POST", "/challenge/pgp", anon, nil, pending), 303)
	if err := e.DB.QueryRow("SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pending.Value)).Scan(&challenge); err != nil {
		t.Fatal(err)
	}
	e.check(e.do("POST", "/challenge", anon, form("method", "pgp", "pgp_code", testDecrypt(t, first, challenge)), pending), 303)
}

// Factor enrollment must serialize with password-confirmed changes: it cannot
// turn on after the factor decision but before the sensitive write commits.
func TestSensitiveChangeSerializesFactorEnrollment(t *testing.T) {
	e := newTestApp(t)
	uid, _ := e.user("factor_serialization", "buyer")
	c := &actionCtx{A: e.A, User: e.loadUser(uid), R: httptest.NewRequest("POST", "/account", nil), Form: form("password", testPassword)}
	_, err := confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		concurrent, err := e.DB.BeginTx(context.Background(), nil)
		if err != nil {
			return actionResult{}, err
		}
		defer concurrent.Rollback()
		var id string
		err = concurrent.QueryRow("SELECT id FROM users WHERE id=$1 FOR UPDATE NOWAIT", uid).Scan(&id)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
			t.Fatalf("factor enrollment could overlap the sensitive change: %v", err)
		}
		// Notification inserts reference users with KEY SHARE. They must remain
		// possible while the account is protected, avoiding user/payout lock cycles.
		concurrent.Rollback()
		notifications, err := e.DB.BeginTx(context.Background(), nil)
		if err != nil {
			return actionResult{}, err
		}
		defer notifications.Rollback()
		if _, err = notifications.Exec("SET LOCAL lock_timeout = '500ms'"); err != nil {
			return actionResult{}, err
		}
		if _, err = notifications.Exec("INSERT INTO notifications(id,user_id,body) VALUES($1,$2,'serialization fixture')", randomToken(), uid); err != nil {
			t.Fatalf("sensitive confirmation blocks notification foreign keys: %v", err)
		}
		return actionResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
