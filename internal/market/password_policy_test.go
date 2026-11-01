package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// SEC725-1: the rule for a new password, as a table. A passphrase passes; a listed password, or a pattern, does not.
func TestNewPasswordRule(t *testing.T) {
	for _, tc := range []struct {
		name, handle, password string
		refused                bool
	}{
		{"listed", "someone", "password1234", true},
		{"listed, any case", "someone", "PassWord1234", true},
		{"listed by this project", "someone", "correcthorsebatterystaple", true},
		{"one repeated character", "someone", "aaaaaaaaaaaa", true},
		{"one repeated symbol", "someone", "!!!!!!!!!!!!!!", true},
		{"short repeated unit", "someone", "abcxabcxabcx", true},
		{"numeric run", "someone", "123456789012", true},
		{"numeric run backwards", "someone", "987654321098", true},
		{"numeric run with one break", "someone", "123456712345", true},
		{"keyboard run", "someone", "qwertyuiopas", true},
		{"keyboard run backwards", "someone", "poiuytrewqlk", true},
		{"second keyboard row", "someone", "asdfghjklzxc", true},
		{"alphabet run", "someone", "abcdefghijklm", true},
		{"runs joined", "someone", "asdf1234qwer", true},
		{"doubled run", "someone", "112233445566", true},
		{"handle inside", "river_otter", "my-river_otter-password", true},
		{"handle inside, any case", "River_Otter", "my-RIVER_otter-password", true},
		{"three-letter handle inside", "bob", "stone-bob-lantern-field", true},

		// SEC725-7: a common word padded with digits or punctuation, leet undone.
		{"padded word", "someone", "letmein12345", true},
		{"padded word 2", "someone", "monkey123456", true},
		{"padded word 3", "someone", "dragon123456", true},
		{"padded word 4", "someone", "football1234", true},
		{"padded word 5", "someone", "admin1234567", true},
		{"leet and padding", "someone", "P@ssw0rd1234", true},
		{"1 read as i", "someone", "l3tm31n12345", true},
		{"1 read as l", "someone", "ki11er12345!", true},
		{"dollar read as s", "someone", "pa$$word!2026", true},
		{"punctuation padding", "someone", "sunshine!!!!", true},
		{"padding in front", "someone", "123456monkey", true},
		{"four-word passphrase", "someone", "lantern orbit velvet quarry", false},
		{"common words among others", "someone", "monkey dragon football 1234", false},
		{"a word that only holds a common word", "someone", "monkeypuzzletree1", false},
		{"passphrase with hyphens", "someone", "maple-torch-gravel-nimbus", false},
		{"a run inside something longer", "someone", "12345 lantern orbit quarry", false},
		{"non-ASCII passphrase", "someone", "Passwörd-Sonne-Birke-Kiesel", false},
		{"the test fixture", "someone", testPassword, false},
		{"handle not inside", "river_otter", "maple-torch-gravel-nimbus", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := weakPassword(tc.handle, tc.password); got != tc.refused {
				t.Fatalf("weakPassword(%q, %q) = %v, want %v", tc.handle, tc.password, got, tc.refused)
			}
		})
	}
}

// The padded-word list is short, and every entry could be the base of a padded password of 12 bytes.
func TestCommonWordListIsShort(t *testing.T) {
	list := commonWords()
	if n := len(list); n < 300 || n > 500 {
		t.Fatalf("%d entries: keep the list short", n)
	}
	for word := range list {
		if len(word) < 4 || len(word) > 11 || strings.Trim(word, "abcdefghijklmnopqrstuvwxyz") != "" {
			t.Errorf("entry %q: want 4 to 11 lowercase letters", word)
		}
	}
	for _, must := range []string{"letmein", "monkey", "dragon", "football", "admin", "password"} {
		if !list[must] {
			t.Errorf("%q is not listed", must)
		}
	}
}

// The list is small enough to review and in the form the check compares with.
func TestCommonPasswordListIsReviewable(t *testing.T) {
	list := commonPasswords()
	if n := len(list); n < 400 || n > 600 {
		t.Fatalf("%d entries: keep the list small and reviewable", n)
	}
	for word := range list {
		if len(word) < 12 || len(word) > 72 || word != strings.ToLower(word) {
			t.Errorf("entry %q cannot match a lowercased password of 12 to 72 bytes", word)
		}
	}
	for _, must := range []string{"password1234", "123456789012", "aaaaaaaaaaaa", "qwerty123456"} {
		if !list[must] {
			t.Errorf("%q is not listed", must)
		}
	}
	for _, comment := range []string{"#", "# added by this project"} {
		if list[comment] {
			t.Errorf("comment %q read as an entry", comment)
		}
	}
}

// /setup refuses a weak first administrator password and installs nothing.
func TestSetupRefusesCommonPasswords(t *testing.T) {
	e := newTestApp(t)
	before := agInt(e, "SELECT count(*) FROM users")
	for _, password := range refusedRegistrationPasswords {
		w := e.do("POST", "/setup", randomToken(), url.Values{"handle": {"first_admin"}, "password": {password}, "token": {e.A.setupToken}})
		if w.Code != 400 || !strings.Contains(w.Body.String(), passwordAdvice) {
			t.Fatalf("setup with %q: %d", password, w.Code)
		}
	}
	if n := agInt(e, "SELECT count(*) FROM users"); n != before {
		t.Fatalf("users %d, was %d", n, before)
	}
	e.check(e.do("POST", "/setup", randomToken(), url.Values{"handle": {"first_admin"}, "password": {"lantern orbit velvet quarry"}, "token": {e.A.setupToken}}), 303)
}

// An account that already has a weak password still signs in: the rule is for new passwords only.
func TestExistingWeakPasswordStillSignsIn(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("old_weak", "buyer")
	for _, old := range []string{"password1234", "P@ssw0rd1234"} { // a listed password, and a padded word
		hash, err := hashPassword(old)
		if err != nil {
			t.Fatal(err)
		}
		agExec(e, "UPDATE users SET password_hash=$2 WHERE id=$1", id, hash)
		w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"old_weak"}, "password": {old}})
		e.check(w, 303)
		e.session(w)
	}
}

// SEC725-2: registrations hold at most registerWork's 1 of passwordWork's 4 slots, so with it held a real sign-in still
// gets a slot. A refused registration has its own answer and is no part of the attack signal.
func TestRegisterFloodCannotTakeEverySignInSlot(t *testing.T) {
	e := newTestApp(t)
	e.user("flood_real", "buyer")
	setCaptchaMode(e, captchaModeUnderAttack)
	if cap(registerWork) != 1 || cap(registerWork) >= cap(passwordWork) {
		t.Fatalf("registerWork has %d slots of passwordWork's %d", cap(registerWork), cap(passwordWork))
	}
	// What a flood of answered registrations holds at most: the registration slot, and with it one password slot.
	for range cap(registerWork) {
		registerWork <- struct{}{}
		passwordWork <- struct{}{}
	}
	release := func() {
		for range cap(registerWork) {
			<-registerWork
			<-passwordWork
		}
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	session := randomToken()
	register := func(handle string) (int, string) {
		id := e.captcha("/register", session)
		w := e.do("POST", "/register", session, url.Values{"handle": {handle}, "password": {"lantern orbit velvet quarry"},
			"captcha_id": {id}, "captcha": {e.A.captchaTextChallenge(id).Answers[0]}})
		return w.Code, w.Body.String()
	}
	for i := range signInAttackBusy * 2 {
		code, body := register("flood_new_" + strings.Repeat("x", i+1))
		if code != 503 || !strings.Contains(body, "Registration is busy") {
			t.Fatalf("registration %d with no registration slot: %d", i, code)
		}
	}
	if e.A.signInAttackOn(context.Background()) {
		t.Fatal("refused registrations tripped the signal")
	}
	w := e.do("POST", "/login", randomToken(), signInForm("flood_real", testPassword))
	e.check(w, 303)
	e.session(w)
	if len(passwordWork) != cap(registerWork) {
		t.Fatalf("password slots held after the sign-in: %d", len(passwordWork))
	}
	release()
	released = true
	if code, body := register("flood_new_ok"); code != 303 {
		t.Fatalf("registration once the slots are free: %d\n%s", code, body)
	}
	if len(registerWork) != 0 || len(passwordWork) != 0 {
		t.Fatalf("slots leaked: register %d password %d", len(registerWork), len(passwordWork))
	}
	// With every password slot taken by other work and a registration slot free, it is still registration's own answer
	// and not the signal, and the registration slot goes back.
	releaseAll := holdSlots(t, passwordWork)
	defer releaseAll()
	for range signInAttackBusy {
		if code, body := register("flood_new_again"); code != 503 || !strings.Contains(body, "Registration is busy") {
			t.Fatalf("registration with no password slot: %d", code)
		}
	}
	if e.A.signInAttackOn(context.Background()) || len(registerWork) != 0 {
		t.Fatalf("signal on=%v, registration slots held %d", e.A.signInAttackOn(context.Background()), len(registerWork))
	}
	// A sign-in refused while a registration holds a slot does not reach the ordinary threshold: it counts apart, at a
	// higher one (TestRegistrationHeldSlotSignInFloodIsVisible).
	registerWork <- struct{}{}
	defer func() { <-registerWork }()
	for i := range signInAttackBusy {
		if w := e.do("POST", "/login", randomToken(), signInForm("flood_real", testPassword)); w.Code != 503 {
			t.Fatalf("sign-in %d with no slot: %d", i, w.Code)
		}
	}
	if e.A.signInAttackOn(context.Background()) {
		t.Fatal("sign-ins refused during a registration flood tripped the signal at the ordinary threshold")
	}
}

// SEC725-6: one notification to each active administrator when the signal turns on, naming no handle or client; none
// for an extension; another only after a hold ended and the signal tripped again.
func TestSignalTripNotifiesAdministratorsOnce(t *testing.T) {
	e := newTestApp(t)
	_, first := e.user("note_admin_one", "admin")
	_, second := e.user("note_admin_two", "admin")
	suspended, _ := e.user("note_admin_held", "admin")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", suspended)
	e.user("note_buyer", "buyer")
	setCaptchaMode(e, captchaModeUnderAttack)
	count := func() int {
		return agInt(e, "SELECT count(*) FROM notifications WHERE body LIKE 'Sign-in attack signal on%'")
	}
	for i := range signInAttackFailures {
		if w := e.do("POST", "/login", randomToken(), signInForm("flood_"+strings.Repeat("y", i%20+3), "wrong-password-123")); w.Code != 401 {
			t.Fatalf("failure %d: %d", i, w.Code)
		}
	}
	if n := count(); n != 2 {
		t.Fatalf("%d notifications for one trip, want one per active administrator", n)
	}
	for _, token := range []string{first, second} {
		mustContain(t, e.body("GET", "/notifications", token, nil, 200), "Sign-in attack signal on")
	}
	body := agStr(e, "SELECT body FROM notifications WHERE body LIKE 'Sign-in attack signal on%' LIMIT 1")
	mustNotContain(t, body, "flood_", "note_", "127.0.0.1", "wrong-password")
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", suspended); n != 0 {
		t.Fatalf("a suspended administrator got %d notifications", n)
	}
	// Further trips during the hold extend it and notify no one, even concurrently.
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range signInAttackFailures {
				e.A.noteSignInFailure(context.Background())
			}
		}()
	}
	for range 8 {
		<-done
	}
	if n := count(); n != 2 {
		t.Fatalf("%d notifications after extensions", n)
	}
	endSignInAttack(e)
	if e.A.signInAttackOn(context.Background()) {
		t.Fatal("hold did not end")
	}
	tripSignInAttack(context.Background(), e.A)
	if n := count(); n != 4 {
		t.Fatalf("%d notifications after a second trip", n)
	}
	// The private ingress's trip is its own and says which it is.
	tripSignInAttack(context.WithValue(context.Background(), adminIngressKey{}, true), e.A)
	if agInt(e, "SELECT count(*) FROM notifications WHERE body LIKE 'Sign-in attack signal on the private administrator onion%'") != 2 {
		t.Fatal("private trip not notified once per administrator")
	}
}
