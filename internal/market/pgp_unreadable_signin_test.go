package market

import (
	"context"
	"html"
	"net/url"
	"sync"
	"testing"
	"time"
)

// A-272: PGP sign-in stays required while the saved key cannot be read. With pgp_2fa on and a stored key that no
// longer parses (garbage, or the A-266 shape saved before that refusal), a correct password must not open a session
// by itself: /login refuses and names the problem and the way back in, or, with TOTP enrolled, /challenge asks for the
// authenticator. The owner is notified once, in plain text.

const (
	unreadableKeyRefusalText = "your saved PGP key can no longer be read"
	unreadableKeyNote        = "PGP sign-in could not be used because your saved PGP key can no longer be read"
	garbageArmoredKey        = "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nbm90IGEga2V5IGF0IGFsbA==\n=AAAA\n-----END PGP PUBLIC KEY BLOCK-----\n"
	garbageFingerprint       = "0123456789ABCDEF0123456789ABCDEF01234567"
	// adminFactorsResetLine is the only way back named to an administrator whose sole factor is lost: the web reset
	// (/admin/reset-factors) refuses administrator targets.
	adminFactorsResetLine = "The operator can turn it off on the server with -reset-admin-factors"
)

// unreadableKeyShapes stores, per shape, a key /account would refuse today.
func unreadableKeyShapes(t *testing.T, name string) map[string][2]string {
	t.Helper()
	armored, fp := mixedRevokedUserIDsKey(t, name)
	return map[string][2]string{"garbage": {garbageArmoredKey, garbageFingerprint}, "a266": {armored, fp}}
}

func TestUnreadableKeyRefusesPasswordOnlySignIn(t *testing.T) {
	e := newTestApp(t)
	_, adminSess := e.user("a272_resetter", "admin")
	for _, role := range []string{"vendor", "admin"} {
		for shape, key := range unreadableKeyShapes(t, "a272_"+role) {
			handle := "a272_" + role + "_" + shape
			id, _ := e.user(handle, role)
			storeKeyBySQL(e, id, key[0], key[1])
			login := url.Values{"handle": {handle}, "password": {testPassword}}
			for attempt := range 2 {
				w := e.do("POST", "/login", randomToken(), login)
				body := html.UnescapeString(w.Body.String())
				if w.Code != 409 || e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" {
					t.Fatalf("%s attempt %d: status=%d cookies=%v body=%q", handle, attempt+1, w.Code, w.Header().Values("Set-Cookie"), body)
				}
				mustContain(t, body, unreadableKeyRefusalText, keyNoOtherMethod, askOperatorLine)
				if role == "admin" {
					// The web reset refuses an administrator target (QA19-1): only the operator's host command is named.
					mustContain(t, body, adminFactorsResetLine)
					mustNotContain(t, body, "Another administrator", "Reset a user's second factors", "A market administrator can turn it off")
				} else {
					mustContain(t, body, "A market administrator can turn it off for you with Reset a user's second factors")
					mustNotContain(t, body, "-reset-admin-factors")
				}
				mustNotContain(t, body, key[1], formatFingerprint(key[1]))
			}
			if agUserSessions(e, id) != 1 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 0 {
				t.Fatalf("%s: the refused sign-in left a session or pending sign-in", handle)
			}
			// Notified once, without key material; a wrong password tells nobody anything.
			agExpect(e, "/login", randomToken(), url.Values{"handle": {handle}, "password": {"wrong-password-123"}}, 401, "Invalid handle or password")
			if n := a152Notes(e, id, unreadableKeyNote); n != 1 {
				t.Fatalf("%s: %d notifications, want 1", handle, n)
			}
			note := agStr(e, "SELECT body FROM notifications WHERE user_id=$1", id)
			// The owner notice names both ways back (A-292): the web reset and, for an administrator, the operator.
			mustContain(t, note, "contact the market staff", "by a market administrator; for an administrator account, by the operator on the server")
			mustNotContain(t, note, key[1], formatFingerprint(key[1]), "BEGIN PGP")
			// The way back in for a non-administrator: the administrator's reset turns PGP sign-in off.
			if role != "admin" {
				e.check(e.do("POST", "/admin/reset-factors", adminSess, url.Values{"handle": {handle}, "password": {testPassword}}), 303)
				w := e.do("POST", "/login", randomToken(), login)
				e.check(w, 303)
				e.session(w)
			}
		}
	}
}

// The staff contact, when set, is named in the refusal.
func TestUnreadableKeyRefusalNamesStaffContact(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a272_contact", "buyer")
	storeKeyBySQL(e, id, garbageArmoredKey, garbageFingerprint)
	agExec(e, "INSERT INTO settings(key,value) VALUES('staff_contact',$1)", staffContactValue)
	b := html.UnescapeString(e.body("POST", "/login", randomToken(), url.Values{"handle": {"a272_contact"}, "password": {testPassword}}, 409))
	mustContain(t, b, unreadableKeyRefusalText, "Contact the market staff: "+staffContactValue+".")
	mustNotContain(t, b, askOperatorLine)
}

func TestUnreadableKeyWithTOTPSignsInWithAuthenticator(t *testing.T) {
	e := newTestApp(t)
	for shape, key := range unreadableKeyShapes(t, "a272_totp") {
		handle := "a272_totp_" + shape
		id, _ := e.user(handle, "vendor")
		storeKeyBySQL(e, id, key[0], key[1])
		totpKey, _ := agEnableTOTP(e, id)
		anon, pc := e.pendingSignIn(handle)
		page := e.body("GET", "/challenge", anon, nil, 200, pc)
		mustContain(t, page, `class="factor-totp"`)
		mustNotContain(t, page, `class="pgp-factor"`)
		e.pendingSignIn(handle) // a second sign-in adds no second notification
		if n := a152Notes(e, id, unreadableKeyNote); n != 1 {
			t.Fatalf("%s: %d notifications, want 1", handle, n)
		}
		w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(totpKey)}}, pc)
		e.check(w, 303)
		e.session(w)
		if agUserSessions(e, id) != 2 {
			t.Fatalf("%s: the authenticator did not sign in", handle)
		}
	}
}

// A new key saved after the notice starts a new "once": the next unreadable key is notified again.
func TestUnreadableKeyNoticeOncePerSavedKey(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a272_again", "buyer")
	storeKeyBySQL(e, id, garbageArmoredKey, garbageFingerprint)
	login := url.Values{"handle": {"a272_again"}, "password": {testPassword}}
	e.check(e.do("POST", "/login", randomToken(), login), 409)
	e.check(e.do("POST", "/login", randomToken(), login), 409)
	agExec(e, "UPDATE notifications SET created=created-interval '1 minute' WHERE user_id=$1", id)
	agExec(e, "UPDATE users SET pgp_changed_at=now() WHERE id=$1", id)
	e.check(e.do("POST", "/login", randomToken(), login), 409)
	if n := a152Notes(e, id, unreadableKeyNote); n != 2 {
		t.Fatalf("%d notifications, want 2", n)
	}
}

// Concurrent sign-ins send one notice (N5): notifyUnreadableKey locks the account row before it looks for an earlier
// notice. The calls are made directly (bcrypt slots cap concurrent /login requests at four) while the test holds the
// row, so every call is in flight at once and none can finish before the others start; without the row lock the calls
// do not wait, which fails the test deterministically.
func TestUnreadableKeyNoticeConcurrentCallsSendOne(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a272_burst", "vendor")
	storeKeyBySQL(e, id, garbageArmoredKey, garbageFingerprint)
	const n = 6 // plus the holder and the poll below, within the application pool of 12
	ctx := context.Background()
	holder, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err = holder.Exec("SELECT 1 FROM users WHERE id=$1 FOR NO KEY UPDATE", id); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- e.A.notifyUnreadableKey(ctx, id) }()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		select {
		case <-finished:
			holder.Rollback()
			t.Fatalf("notifyUnreadableKey finished while the account row was locked (%d notices): it does not lock the row", a152Notes(e, id, unreadableKeyNote))
		default:
		}
		// Only this database's backends (A-293): another test process on the same server may be running this test.
		if agInt(e, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT pgp_changed_at FROM users WHERE id=$1 FOR NO KEY UPDATE%'") >= n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the calls never waited on the account row")
		}
	}
	if err = holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	<-finished
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := a152Notes(e, id, unreadableKeyNote); got != 1 {
		t.Fatalf("%d concurrent calls sent %d notices, want 1", n, got)
	}
}
