package market

import (
	"strings"
	"testing"
	"time"
)

func TestModerationRetentionNoticeSurvivesOriginalExpiryAndDisabledCapture(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	vendor, session := e.user("moderation_retention_notice_vendor", "vendor")
	product := e.product(vendor, "digital")
	const marker = "MOD69-retained-plaintext-marker-6a7d"
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	agExec(e, `UPDATE products SET description=$2 WHERE id=$1`, product, marker)
	if n, err := moderationTestWorker(e).Pass(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("capture proposal: processed=%d error=%v", n, err)
	}
	assertCopies := func() {
		t.Helper()
		if agInt(e, `SELECT count(*) FROM moderation_jobs WHERE target_id=$1 AND strpos(snapshot::text,$2)>0`, product, marker) != 1 || agInt(e, `SELECT count(*) FROM moderation_suggestions WHERE target_id=$1 AND strpos(input::text,$2)>0`, product, marker) != 1 {
			t.Fatal("fixture no longer demonstrates retained plaintext job and suggestion copies")
		}
	}
	assertNotice := func(stage string) {
		t.Helper()
		for _, path := range []string{"/canary", "/account"} {
			page := e.body("GET", path, session, nil, 200)
			for _, text := range []string{"Routine expiry and account deletion do not erase copies retained for abuse review", "These copies currently have no automatic expiry"} {
				if !strings.Contains(page, text) {
					t.Errorf("%s %s omitted retained-copy notice %q", stage, path, text)
				}
			}
			if strings.Contains(page, marker) {
				t.Errorf("%s %s disclosed private review contents", stage, path)
			}
		}
		page := e.body("GET", "/canary", "", nil, 200)
		if !strings.Contains(page, "<strong>Abuse-review records.</strong>") || !strings.Contains(page, "after collection stops") {
			t.Errorf("%s public records inventory omitted the review-copy lifecycle", stage)
		}
		account := e.body("GET", "/account", session, nil, 200)
		if !strings.Contains(account, "Account deletion does not erase retained abuse-review copies") {
			t.Errorf("%s deletion confirmation omitted retained abuse-review copies", stage)
		}
	}
	assertCopies()
	assertNotice("captured")
	// Advance only the existing listing/enrollment clocks. The real retention
	// sweep removes the original; this change does not introduce evidence expiry.
	agExec(e, `UPDATE products SET archived=true,archived_at=now()-interval '31 days',updated=now()-interval '31 days' WHERE id=$1`, product)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM products WHERE id=$1`, product) != 0 {
		t.Fatal("original listing did not expire through ordinary retention")
	}
	assertCopies()
	assertNotice("original expired")
	if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
		t.Fatal(err)
	}
	assertCopies()
	assertNotice("capture disabled with historical copies")
}
