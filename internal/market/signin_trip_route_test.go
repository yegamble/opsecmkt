package market

import (
	"context"
	"html"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
)

// PM109-3: which count turned the attack signal on was only in the server log. The last trip's route and time are kept
// per ingress in a settings row and shown on /admin/security, and its notification names the route. DB integration.

var signInTripRow = regexp.MustCompile(`^(failed_passwords|refused_sign_ins|registration_held_slot)\|\d{4}-\d\d-\d\d \d\d:\d\d UTC$`)

func TestAttackSignalLastTripRouteIsStoredAndShown(t *testing.T) {
	e := newTestApp(t)
	e.A.adminSocket = "/run/market-admin/http.sock"
	adminID, admin := e.user("trip_admin", "admin")
	_, moderator := e.user("trip_moderator", "moderator")
	_, buyer := e.user("trip_buyer", "buyer")
	setCaptchaMode(e, captchaModeUnderAttack)
	// With a private ingress configured, /admin is served only there.
	get := func(session string) *httptest.ResponseRecorder {
		return ingressDo(e, true, "GET", "/admin/security", session, nil)
	}
	page := func() string {
		t.Helper()
		w := get(admin)
		e.check(w, 200)
		return html.UnescapeString(w.Body.String())
	}
	mustContain(t, page(), "Attack signal: no record of it turning on.", "Attack signal on the private administrator onion: no record of it turning on.")
	row := func(key string) string {
		return agStr(e, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'')", key)
	}
	ctx := context.Background()

	// A concurrent burst over the threshold is one trip: one notice, one route.
	var wg sync.WaitGroup
	for range signInAttackFailures {
		wg.Go(func() { e.A.noteSignInFailure(ctx) })
	}
	wg.Wait()
	if n := signalNotices(e, adminID); n != 1 {
		t.Fatalf("%d notices for one concurrent trip", n)
	}
	stored := row(settingSignInAttackLastPublic)
	if !signInTripRow.MatchString(stored) {
		t.Fatalf("stored trip %q is not a route and a time alone", stored)
	}
	_, at, _ := cutTrip(stored)
	want := "Attack signal last turned on " + at + ": many failed passwords."
	body := page()
	mustContain(t, body, want, "Attack signal on the private administrator onion: no record of it turning on.")
	mustContain(t, page(), want) // after a reload
	notice := agStr(e, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'Sign-in attack signal on%'", adminID)
	mustContain(t, notice, "Sign-in attack signal on: many failed passwords.")
	mustNotContain(t, notice+stored, "trip_", "127.0.0.1", adminID)

	// Non-administrators see neither the panel nor the notice.
	for _, session := range []string{moderator, buyer} {
		mustNotContain(t, get(session).Body.String(), "last turned on", "no record of it turning on")
		mustNotContain(t, e.body("GET", "/notifications", session, nil, 200), "Sign-in attack signal on")
	}

	// A burst while it is on extends the hold: no notice, and the route of the trip that turned it on stays.
	for range signInAttackBusy {
		e.A.noteSignInBusy(ctx)
	}
	if n := signalNotices(e, adminID); n != 1 {
		t.Fatalf("%d notices after an extension", n)
	}
	if got := row(settingSignInAttackLastPublic); got != stored {
		t.Fatalf("an extension changed the stored trip: %q", got)
	}

	// The next hold replaces it, whichever count trips.
	endSignInAttack(e)
	for range signInAttackBusy {
		e.A.noteSignInBusy(ctx)
	}
	mustContain(t, page(), ": many sign-ins refused because every password slot was busy.")
	endSignInAttack(e)
	for range signInAttackHeldBusy {
		e.A.noteSignInHeldBusy(ctx)
	}
	mustContain(t, page(), ": many sign-ins refused while a registration held a password slot.")
	if n := signalNotices(e, adminID); n != 3 {
		t.Fatalf("%d notices after three holds", n)
	}
	if n := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Sign-in attack signal on: many sign-ins refused while a registration held a password slot.%'", adminID); n != 1 {
		t.Fatalf("%d notices name the held-slot route", n)
	}

	// The private ingress keeps its own record and says which it is.
	tripSignInAttack(context.WithValue(ctx, adminIngressKey{}, true), e.A)
	if !signInTripRow.MatchString(row(settingSignInAttackLastPrivate)) {
		t.Fatalf("private trip: %q", row(settingSignInAttackLastPrivate))
	}
	mustContain(t, page(), "Attack signal on the private administrator onion last turned on ")

	// A restart ends the signal but not the record of what turned it on.
	e.restart()
	e.A.adminSocket = "/run/market-admin/http.sock"
	mustContain(t, page(), "Attack signal: off.", ": many sign-ins refused while a registration held a password slot.", "Attack signal on the private administrator onion last turned on ")
	if n := signalNotices(e, adminID); n != 4 {
		t.Fatalf("%d notices after a restart", n)
	}

	// A row this binary cannot read shows as no record, never as its raw text.
	agExec(e, "UPDATE settings SET value='<b>x</b>|bad' WHERE key=$1", settingSignInAttackLastPublic)
	body = page()
	mustContain(t, body, "Attack signal: no record of it turning on.")
	mustNotContain(t, body, "<b>x</b>")
}
