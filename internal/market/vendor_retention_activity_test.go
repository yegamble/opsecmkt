package market

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestVendorLastSeenIsScrubbedWhenRetentionClosesOrRestrictsAccount(t *testing.T) {
	for _, kind := range []string{"account_close", "account_restrict"} {
		t.Run(kind, func(t *testing.T) {
			e := newTestApp(t)
			vendorID, _ := e.user("retained_activity_vendor", "vendor")
			agExec(e, `UPDATE users SET vendor_show_last_seen=true,vendor_last_seen=now()-interval '2 hours',
				deletion_requested_at=now()-interval '1 second' WHERE id=$1`, vendorID)
			tx, err := e.DB.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err = applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: kind, ID: vendorID, At: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var erased bool
			if err = e.DB.QueryRow("SELECT NOT vendor_show_last_seen AND vendor_last_seen IS NULL FROM users WHERE id=$1", vendorID).Scan(&erased); err != nil {
				t.Fatal(err)
			}
			if !erased {
				t.Fatal("retention left vendor activity publication enabled or retained its timestamp")
			}
		})
	}
}

func TestVendorActivityDoesNotRecordRestrictedSuspendedOrClosedAccounts(t *testing.T) {
	for _, column := range []string{"retention_restricted_at", "suspended_at", "closed_at"} {
		t.Run(column, func(t *testing.T) {
			e := newTestApp(t)
			vendorID, _ := e.user("inactive_activity_vendor", "vendor")
			agExec(e, "UPDATE users SET vendor_show_last_seen=true,vendor_last_seen='2001-02-03 04:00:00+00',"+column+"=now() WHERE id=$1", vendorID)
			// Use a stale page actor deliberately: the database predicate must
			// recheck account status rather than trust an earlier session read.
			d := PageData{User: &User{ID: vendorID, Role: "vendor"}}
			r := httptest.NewRequest("GET", "/account", nil)
			if err := vendorActivityLoader(t.Context(), e.A, r, &d); err != nil {
				t.Fatal(err)
			}
			var unchanged bool
			if err := e.DB.QueryRow("SELECT vendor_last_seen='2001-02-03 04:00:00+00' FROM users WHERE id=$1", vendorID).Scan(&unchanged); err != nil {
				t.Fatal(err)
			}
			if !unchanged {
				t.Fatal("activity was recorded after the account became unavailable")
			}
		})
	}
}
