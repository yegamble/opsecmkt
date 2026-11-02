package market

import (
	"context"
	"html"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// PM109-5: a broken staff contact showed only when a panel was opened. A daily sweep notifies the administrators once per
// change of its health, and 30 days before its key expires; the last state notified is a settings row, so a restart or
// another pass repeats nothing. DB integration.

var staffContactHealthRow = regexp.MustCompile(`^(ok|((empty|not-staff|no-key|closed|key-stops:\d{4}-\d\d-\d\d)(,|$))+)$`)

const staffHealthLike = "The staff contact %"

func expiringPublicKey(t *testing.T, name string, lifetime time.Duration) string {
	t.Helper()
	key, err := openpgp.NewEntity(name, "", name+"@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, KeyLifetimeSecs: uint32(lifetime / time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return armoredPublicKey(t, key)
}

func TestStaffContactHealthSweepNotifiesOncePerChange(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	setting := func(key, v string) {
		t.Helper()
		agExec(e, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, v)
	}
	setting(settingRegistrations, "open")
	sweep := func() {
		t.Helper()
		if err := e.A.sweepStaffContactHealth(ctx); err != nil {
			t.Fatal(err)
		}
	}
	marker := func() string {
		t.Helper()
		return agStr(e, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'')", settingStaffContactHealth)
	}

	// No administrator yet, as before first-run setup: nothing is recorded, so the first administrator still hears of it.
	sweep()
	if m := marker(); m != "" {
		t.Fatalf("marker %q with no one to tell", m)
	}

	adminID, admin := e.user("pm5_admin", "admin")
	otherID, _ := e.user("pm5_other_admin", "admin")
	suspendedID, _ := e.user("pm5_held_admin", "admin")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", suspendedID)
	modID, modSess := e.user("pm5_mod", "moderator")
	_, buyerSess := e.user("pm5_buyer", "buyer")
	count := func(id string) int {
		t.Helper()
		return agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", id, staffHealthLike)
	}
	want := func(n int) {
		t.Helper()
		if a, o := count(adminID), count(otherID); a != n || o != n {
			t.Fatalf("notices: admin %d, other admin %d, want %d each", a, o, n)
		}
		if s, m := count(suspendedID), count(modID); s != 0 || m != 0 {
			t.Fatalf("notices to a suspended administrator %d, a moderator %d", s, m)
		}
		m := marker()
		if !staffContactHealthRow.MatchString(m) {
			t.Fatalf("stored state %q is not fixed codes", m)
		}
		mustNotContain(t, m, "pm5_", "xmpp", adminID)
	}
	latest := func() string {
		t.Helper()
		return agStr(e, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2 ORDER BY created DESC, id LIMIT 1", adminID, staffHealthLike)
	}

	// A market with no contact: one notice, however many passes, across a restart.
	sweep()
	want(1)
	mustContain(t, latest(), staffContactWarnEmpty)
	sweep()
	e.restart()
	sweep()
	want(1)
	for _, session := range []string{modSess, buyerSess} {
		mustNotContain(t, e.body("GET", "/notifications", session, nil, 200), "staff contact")
		e.check(e.do("GET", "/admin/site", session, nil), 403)
	}
	for range 2 { // through a reload
		mustContain(t, html.UnescapeString(e.body("GET", "/notifications", admin, nil, 200)), staffContactWarnEmpty)
	}

	// Concurrent passes over one change send one notice. A former staff handle is named nowhere in it.
	setting(settingStaffContact, "pm5_buyer")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := e.A.sweepStaffContactHealth(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	want(2)
	body := latest()
	mustContain(t, body, staffContactWarnNotStaff)
	mustNotContain(t, body, "pm5_", "contact the market staff")

	// Fixed: told once that it is, and told nothing more.
	setting(settingStaffContact, "pm5_mod")
	sweep()
	sweep()
	want(3)
	mustContain(t, latest(), "The staff contact no longer has a warning.")

	// A change of which warning applies is a change; the same warning for another handle is not.
	setting(settingRegistrations, "closed")
	sweep()
	want(4)
	mustContain(t, latest(), staffContactWarnClosed)
	setting(settingRegistrations, "open")
	sweep()
	want(5)
	e.keylessUser("pm5_keyless", "moderator")
	otherKeyless, _ := e.keylessUser("pm5_keyless2", "moderator")
	setting(settingStaffContact, "pm5_keyless")
	sweep()
	want(6)
	mustContain(t, latest(), staffContactWarnNoKey)
	setting(settingStaffContact, "pm5_keyless2")
	sweep()
	want(6)

	// Beyond 30 days nothing is said; within them, one notice with the date, on the panel too.
	e.setKey(otherKeyless, expiringPublicKey(t, "pm5_far", 40*24*time.Hour))
	sweep()
	want(7)
	mustContain(t, latest(), "no longer has a warning")
	mustNotContain(t, html.UnescapeString(e.body("GET", "/admin/site", admin, nil, 200)), "stops receiving encrypted messages on")
	e.setKey(otherKeyless, expiringPublicKey(t, "pm5_near", 10*24*time.Hour))
	sweep()
	sweep()
	e.restart()
	sweep()
	want(8)
	body = latest()
	stop := time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.DateOnly)
	mustContain(t, body, "The staff contact's PGP key stops receiving encrypted messages on ", stop)
	mustNotContain(t, body, "pm5_", "@example.test")
	for _, path := range []string{"/admin/site", "/admin"} {
		mustContain(t, html.UnescapeString(e.body("GET", path, admin, nil, 200)), "The staff contact's PGP key stops receiving encrypted messages on "+stop)
	}

	// The key then expires: a change. Renewing it: another.
	e.setKey(otherKeyless, testUnusableKeys(t, "pm5_gone")["expired key"])
	sweep()
	sweep()
	want(9)
	mustContain(t, latest(), staffContactWarnNoKey)
	e.setKey(otherKeyless, expiringPublicKey(t, "pm5_renewed", 400*24*time.Hour))
	sweep()
	want(10)
	mustContain(t, latest(), "no longer has a warning")

	// A pass that cannot read the contact's account fails: it tells no one and keeps the state, so the next pass decides.
	agExec(e, "UPDATE settings SET value='pm5_keyless2' WHERE key=$1", settingStaffContact)
	before := marker()
	agExec(e, "ALTER TABLE users RENAME COLUMN pgp_key_generation TO pgp_key_generation_broken")
	err := e.A.sweepStaffContactHealth(ctx)
	agExec(e, "ALTER TABLE users RENAME COLUMN pgp_key_generation_broken TO pgp_key_generation")
	if err == nil {
		t.Fatal("a pass with the check failing reported success")
	}
	want(10)
	if m := marker(); m != before {
		t.Fatalf("a failed pass moved the state from %q to %q", before, m)
	}

	// A market whose first sweep finds nothing wrong says nothing.
	agExec(e, "DELETE FROM settings WHERE key=$1", settingStaffContactHealth)
	setting(settingStaffContact, "xmpp:help@staff-example.onion")
	sweep()
	want(10)
	if m := marker(); m != "ok" {
		t.Fatalf("healthy marker %q", m)
	}
}

// The sweeper runs a pass when it starts, as registerBackground runs it, and stops with its context.
func TestStaffContactHealthSweeperRunsAtStart(t *testing.T) {
	e := newTestApp(t)
	adminID, _ := e.user("pm5_start_admin", "admin")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		staffContactHealthSweeper(ctx, e.A)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", adminID, staffHealthLike) != 1 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("no notice from the sweeper")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}
