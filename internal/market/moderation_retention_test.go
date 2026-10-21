package market

import (
	"testing"
	"time"
)

func TestModerationRetentionUserReferenceInventory(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	account, _ := e.user("moderation_retention_unknown_owner", "buyer")
	agExec(e, `DELETE FROM sessions WHERE user_id=$1`, account)
	agExec(e, `UPDATE users SET closed_at=now(),profile_scrubbed_at=now() WHERE id=$1`, account)
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	unknown, err := retentionUnknownUserReferences(t.Context(), tx)
	if err != nil || unknown {
		t.Fatalf("fresh moderation schema blocks account erasure: unknown=%t err=%v", unknown, err)
	}
	// Nullable references still need an explicit lifecycle review. Do not make
	// the inventory accept every future ON DELETE SET NULL relationship.
	if _, err = tx.ExecContext(t.Context(), `CREATE TABLE retention_unreviewed_moderation_owner(user_id text REFERENCES users(id) ON DELETE SET NULL)`); err != nil {
		t.Fatal(err)
	}
	unknown, err = retentionUnknownUserReferences(t.Context(), tx)
	if err != nil || !unknown {
		t.Fatalf("unreviewed nullable user reference bypassed inventory: unknown=%t err=%v", unknown, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1`, account) != 1 {
		t.Fatal("an unreviewed user relationship no longer blocked account erasure")
	}
}

func TestModerationRetentionDisabledDoesNotBlockAccountErasure(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	account, _ := e.user("moderation_retention_disabled", "buyer")
	if agStr(e, `SELECT value FROM settings WHERE key='moderation_enabled'`) != "false" {
		t.Fatal("fixture must keep moderation disabled")
	}
	agExec(e, `DELETE FROM sessions WHERE user_id=$1`, account)
	agExec(e, `UPDATE users SET closed_at=now(),profile_scrubbed_at=now() WHERE id=$1`, account)
	now := time.Now().UTC()
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1`, account) != 0 {
		t.Fatal("moderation migration blocked eligible account erasure while moderation was disabled")
	}
}

func TestModerationRetentionReviewerErasureUnlinksRetainedReview(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	reviewer, _ := e.user("moderation_retention_reviewer", "admin")
	e.user("moderation_retention_other_admin", "admin")
	vendor, _ := e.user("moderation_retention_vendor", "vendor")
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	e.product(vendor, "digital")
	if processed, err := moderationTestWorker(e).Pass(t.Context(), 1); err != nil || processed != 1 {
		t.Fatalf("record proposal: processed=%d err=%v", processed, err)
	}
	suggestion := agStr(e, `SELECT id FROM moderation_suggestions`)
	agExec(e, `INSERT INTO moderation_reviews(suggestion_id,reviewer_user_id,action) VALUES($1,$2,'hold')`, suggestion, reviewer)
	var created time.Time
	if err := e.DB.QueryRow(`SELECT created_at FROM moderation_reviews WHERE suggestion_id=$1`, suggestion).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
		t.Fatal(err)
	}
	agExec(e, `DELETE FROM sessions WHERE user_id=$1`, reviewer)
	agExec(e, `UPDATE users SET closed_at=now(),profile_scrubbed_at=now() WHERE id=$1`, reviewer)
	now := time.Now().UTC()
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1`, reviewer) != 0 {
		t.Fatal("retained moderation review blocked eligible reviewer erasure")
	}
	var retained bool
	if err := e.DB.QueryRow(`SELECT reviewer_user_id IS NULL AND action='hold' AND created_at=$2 FROM moderation_reviews WHERE suggestion_id=$1`, suggestion, created).Scan(&retained); err != nil || !retained {
		t.Fatalf("erasure lost review chronology or retained reviewer link: retained=%t err=%v", retained, err)
	}
	if agInt(e, `SELECT count(*) FROM moderation_suggestions WHERE id=$1`, suggestion) != 1 {
		t.Fatal("reviewer erasure removed the moderation proposal")
	}
}

func TestModerationRetentionActorCounterErasedWithAccount(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	account, _ := e.user("moderation_retention_counted_actor", "buyer")
	agExec(e, `INSERT INTO moderation_actor_allocations(actor_id,window_start,allocations) VALUES($1,now(),3)`, account)
	agExec(e, `DELETE FROM sessions WHERE user_id=$1`, account)
	agExec(e, `UPDATE users SET closed_at=now(),profile_scrubbed_at=now() WHERE id=$1`, account)
	now := time.Now().UTC()
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1`, account) != 0 {
		t.Fatal("a moderation actor counter blocked eligible account erasure")
	}
	if agInt(e, `SELECT count(*) FROM moderation_actor_allocations WHERE actor_id=$1`, account) != 0 {
		t.Fatal("account erasure left the moderation actor counter")
	}
}

func TestModerationRetentionMarkerReleasesErasedWithAccount(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	account, _ := e.user("moderation_retention_released_actor", "buyer")
	agExec(e, `INSERT INTO moderation_marker_releases(actor_id,parked) VALUES($1,false),($1,true)`, account)
	agExec(e, `DELETE FROM sessions WHERE user_id=$1`, account)
	agExec(e, `UPDATE users SET closed_at=now(),profile_scrubbed_at=now() WHERE id=$1`, account)
	now := time.Now().UTC()
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1`, account) != 0 {
		t.Fatal("a pending moderation release blocked eligible account erasure")
	}
	if agInt(e, `SELECT count(*) FROM moderation_marker_releases WHERE actor_id=$1`, account) != 0 {
		t.Fatal("account erasure left pending moderation releases")
	}
}
