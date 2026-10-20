package market

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"testing"
	"time"
)

// forceDeadline writes set on order's escrow row past migration 079's freeze trigger, as a test moving the clock does
// (the trigger is disabled only inside this transaction).
func (x *escrowEnv) forceDeadline(order, set string) {
	x.t.Helper()
	tx, err := x.DB.Begin()
	if err != nil {
		x.t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{"ALTER TABLE escrows DISABLE TRIGGER escrows_deadlines_frozen", "UPDATE escrows SET " + set + " WHERE order_id=$1",
		"ALTER TABLE escrows ENABLE TRIGGER escrows_deadlines_frozen"} {
		var args []any
		if strings.Contains(q, "$1") {
			args = append(args, order)
		}
		if _, err = tx.Exec(q, args...); err != nil {
			x.t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		x.t.Fatal(err)
	}
}

// deadlineOf is order's escrow column (release_at or ship_by) as the pages and notices state it.
func (x *escrowEnv) deadlineOf(order, column string) string {
	x.t.Helper()
	var at time.Time
	if err := x.DB.QueryRow("SELECT "+column+" FROM escrows WHERE order_id=$1", order).Scan(&at); err != nil {
		x.t.Fatal(err)
	}
	if column == "ship_by" {
		return utcFrom(at)
	}
	return utcUntil(at)
}

// noticesOf lists uid's notifications that name order (its short ID), oldest first.
func (x *escrowEnv) noticesOf(uid, order string) []string {
	return agStrings(x.t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND position($2 in body)>0 ORDER BY created,id", uid, shortID(order))
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func anyContains(list []string, parts ...string) bool {
	for _, s := range list {
		if containsAll(s, parts...) {
			return true
		}
	}
	return false
}

// countContains counts the entries of list holding every part.
func countContains(list []string, parts ...string) int {
	n := 0
	for _, s := range list {
		if containsAll(s, parts...) {
			n++
		}
	}
	return n
}

// waitForLockWaits waits until n backends of this database wait for a lock.
func (x *escrowEnv) waitForLockWaits(n int) {
	x.t.Helper()
	if !waitUntil(x.t, 5*time.Second, func() bool {
		return agInt(x.testEnv, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'") >= n
	}) {
		x.t.Fatalf("fewer than %d requests waited for a lock", n)
	}
}

// A-381: only the release recheck (escrow_deadline.go) writes release_recheck, and release_at is written only there and
// by the shipment (escrow_release.go, from NULL); scripts/restore.sh only asks for the recheck.
func TestEscrowReleaseAtWriters(t *testing.T) {
	writes := regexp.MustCompile(`(?i)(release_at|release_recheck)\s*=[^=]`)
	// TestMain runs the tests from the repository root.
	files, err := filepath.Glob("internal/market/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files found: %v", err)
	}
	scripts, _ := filepath.Glob("scripts/*.sh")
	// The Monero escrow's shipment and recheck (slice X-e) follow the same rules for Monero escrow orders.
	allowed := map[string]string{"escrow_release.go": "release_at", "escrow_deadline.go": "both", "escrow_xmr_release.go": "release_at", "escrow_xmr_deadline.go": "both"}
	for _, f := range append(files, scripts...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range writes.FindAllStringSubmatch(string(b), -1) {
			col := strings.ToLower(m[1])
			switch allowed[filepath.Base(f)] {
			case "both":
			case col:
			default:
				t.Errorf("%s writes %s (%q): only the release recheck moves release_at and resets release_recheck", f, col, m[0])
			}
		}
	}
}
