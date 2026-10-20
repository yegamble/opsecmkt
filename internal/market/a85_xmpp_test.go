package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestA85RemovedXMPPProfileAndUpgradeClearsStoredValues(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.keylessUser("a85_buyer", "buyer")
	if _, err := e.DB.Exec("UPDATE users SET xmpp=$1 WHERE id=$2", "legacy@example.test", uid); err != nil {
		t.Fatal(err)
	}

	// Simulate an installation that has the legacy value but has not recorded migration 098.
	migrations, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("DELETE FROM schema_migrations WHERE version=98"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = applyMigrations(context.Background(), tx, migrations); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := agStr(e, "SELECT xmpp FROM users WHERE id=$1", uid); got != "" {
		t.Fatalf("upgrade retained legacy XMPP value %q", got)
	}

	// Old or malicious clients may still submit the removed parameter; it must be ignored.
	e.check(e.do("POST", "/account", session, url.Values{"xmpp": {strings.Repeat("x", 300)}}), 303)
	if got := agStr(e, "SELECT xmpp FROM users WHERE id=$1", uid); got != "" {
		t.Fatalf("removed XMPP field stored %q", got)
	}
	page := e.do("GET", "/account", session, nil)
	e.check(page, 200)
	if !strings.Contains(page.Body.String(), "XMPP notifications") || strings.Contains(page.Body.String(), "legacy@example.test") {
		t.Fatal("private opt-in form missing or legacy destination exposed")
	}
}
