package market

import (
	"net/url"
	"testing"
)

func TestRetentionAdministratorCannotRestoreClosedOrRestrictedOwnersListing(t *testing.T) {
	for _, state := range []string{"closed", "restricted"} {
		for _, path := range []string{"/listings/restore", "/admin/listing"} {
			t.Run(state+path, func(t *testing.T) {
				e, _ := uiRetentionEnv(t)
				uid, _ := e.user("retention_restore_vendor", "vendor")
				_, admin := e.user("retention_restore_admin", "admin")
				product := e.product(uid, "physical")
				if state == "closed" {
					agExec(e, `UPDATE users SET closed_at=now(),suspended_at=now(),password_hash='' WHERE id=$1`, uid)
				} else {
					agExec(e, `UPDATE users SET deletion_requested_at=now()-interval '31 days',retention_restricted_at=now() WHERE id=$1`, uid)
				}
				agExec(e, `UPDATE products SET archived=true,archived_at=now()-interval '45 days',private_expired_at=now()-interval '15 days' WHERE id=$1`, product)
				form := url.Values{"id": {product}}
				if path == "/admin/listing" {
					agExec(e, `UPDATE products SET admin_archive_reason=$2 WHERE id=$1`, product, takedownReason)
					page := e.body("GET", "/listing-edit?id="+product, admin, nil, 200)
					form = takedownForm(t, page, "restore", product)
				}
				beforeAudit := agInt(e, `SELECT count(*) FROM audit_events`)
				beforeNotifications := agInt(e, `SELECT count(*) FROM notifications`)
				w := e.do("POST", path, admin, form)
				e.check(w, 409)
				mustContain(t, refusalText(w.Body.String()), "closing their account")
				if agInt(e, `SELECT count(*) FROM products WHERE id=$1 AND archived AND archived_at IS NOT NULL AND private_expired_at IS NOT NULL`, product) != 1 {
					t.Fatal("administrator restore bypassed the owner's retention lifecycle")
				}
				if path == "/admin/listing" && agStr(e, `SELECT admin_archive_reason FROM products WHERE id=$1`, product) != takedownReason {
					t.Fatal("rejected restoration cleared the existing takedown reason")
				}
				if agInt(e, `SELECT count(*) FROM audit_events`) != beforeAudit || agInt(e, `SELECT count(*) FROM notifications`) != beforeNotifications {
					t.Fatal("rejected restoration reported a successful change")
				}
			})
		}
	}
}
