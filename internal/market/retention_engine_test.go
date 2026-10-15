package market

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func retentionPrivateTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func ageRetentionEnrollment(t *testing.T, a *App, at time.Time) {
	t.Helper()
	s := a.retention
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = s.read(); err != nil {
		t.Fatal(err)
	}
	s.state.EnabledAt = at
	s.state.EnrollmentDeadline = at.Add(30 * 24 * time.Hour)
	if err = s.write(); err != nil {
		t.Fatal(err)
	}
}

func runRetentionAt(t *testing.T, a *App, now time.Time) {
	t.Helper()
	for i := 0; i < 256; i++ {
		more, err := a.runRetentionBatch(t.Context(), now)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("retention did not drain bounded batches")
}

func commitRetentionHold(t *testing.T, e *testEnv, h retentionHold) string {
	t.Helper()
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id, err := e.A.setRetentionHoldTx(t.Context(), tx, h)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = e.A.finalizeRetentionOperation(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return agStr(e, `SELECT id FROM retention_holds WHERE scope=$1 AND record_id=$2`, h.Scope, h.RecordID)
}

func TestRetentionEnrollmentDoesNotPurgeLegacyRows(t *testing.T) {
	e := newTestApp(t)
	sender, _ := e.user("retention_enroll_sender", "buyer")
	recipient, _ := e.user("retention_enroll_recipient", "buyer")
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,created) VALUES('old_enrollment_message',$1,$2,'old ciphertext',now()-interval '365 days')`, sender, recipient)
	cfg := retentionConfig{Enabled: true, StateDir: retentionPrivateTestDir(t)}
	if err := e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM messages WHERE id='old_enrollment_message'`) != 1 {
		t.Fatal("initial opt-in silently purged historical messages")
	}
	if agInt(e, `SELECT count(*) FROM notifications WHERE body LIKE 'Retention is now enabled.%'`) != 2 {
		t.Fatal("existing users did not receive enrollment notice")
	}
	if agInt(e, `SELECT count(*) FROM users WHERE last_authenticated_at IS NOT NULL`) != 0 {
		t.Fatal("migration fabricated historical activity")
	}
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM messages WHERE id='old_enrollment_message'`) != 0 {
		t.Fatal("enrollment grace never ended")
	}
}

func TestRetentionExpiryDrainsBatchesAndKeepsRecentRows(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	sender, _ := e.user("retention_batch_sender", "buyer")
	recipient, _ := e.user("retention_batch_recipient", "buyer")
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,created) SELECT 'expired_'||n,$1,$2,'old ciphertext',now()-interval '91 days' FROM generate_series(1,450) n`, sender, recipient)
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,created) VALUES('recent_message',$1,$2,'recent ciphertext',now()-interval '89 days')`, sender, recipient)
	agExec(e, `INSERT INTO notifications(id,user_id,body,created) VALUES('expired_notification',$1,'fixed notice',now()-interval '31 days'),('recent_notification',$1,'fixed notice',now()-interval '29 days')`, sender)
	agExec(e, `INSERT INTO audit_events(user_id,action,created) VALUES($1,'old fixed action',now()-interval '181 days'),($1,'recent fixed action',now()-interval '179 days')`, sender)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM messages`) != 1 || agInt(e, `SELECT count(*) FROM messages WHERE id='recent_message'`) != 1 {
		t.Fatal("individual message clocks or batch drain failed")
	}
	if agInt(e, `SELECT count(*) FROM notifications WHERE id IN('expired_notification','recent_notification')`) != 1 {
		t.Fatal("notification expiry failed")
	}
	if agInt(e, `SELECT count(*) FROM audit_events WHERE action IN('old fixed action','recent fixed action')`) != 1 {
		t.Fatal("audit expiry failed")
	}
	if agInt(e, `SELECT count(*) FROM retention_runs`) < 3 {
		t.Fatal("batch purges were not recorded")
	}
	if _, err := e.DB.Exec(`DELETE FROM audit_events`); err == nil {
		t.Fatal("ordinary audit deletion bypassed restricted maintenance")
	}
}

func TestRetentionOverdueHoldSurvivesAndReleaseUsesOriginalDeadline(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	sender, _ := e.user("retention_hold_sender", "buyer")
	recipient, _ := e.user("retention_hold_recipient", "buyer")
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,created) VALUES('preserved_message',$1,$2,'case ciphertext',now()-interval '120 days')`, sender, recipient)
	now := time.Now().UTC()
	id := commitRetentionHold(t, e, retentionHold{Scope: "message", RecordID: "preserved_message", Authority: "Case-specific obligation", Reason: "Relevant evidence", Owner: "Named case owner", Readers: "Assigned handlers", ReleaseCondition: "Written release", BeganAt: now.Add(-60 * 24 * time.Hour), ReviewAt: now.Add(-31 * 24 * time.Hour)})
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM messages WHERE id='preserved_message'`) != 1 {
		t.Fatal("overdue review silently released legal preservation")
	}
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.A.releaseRetentionHoldTx(t.Context(), tx, id, "Named case owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = e.A.finalizeRetentionOperation(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM messages WHERE id='preserved_message'`) != 0 {
		t.Fatal("release restarted an expired message clock")
	}
}

func TestRetentionRestoreReappliesExternalDeletionAndRefusesMissingState(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	sender, _ := e.user("retention_restore_sender", "buyer")
	recipient, _ := e.user("retention_restore_recipient", "buyer")
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,created) VALUES('resurrection_message',$1,$2,'old ciphertext',now()-interval '91 days')`, sender, recipient)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	// A restored old copy may contain a body even when its timestamp was moved by
	// unrelated historical bugs. The external ID remains authoritative.
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body) VALUES('resurrection_message',$1,$2,'resurrected ciphertext')`, sender, recipient)
	agExec(e, `INSERT INTO settings(key,value) VALUES('retention_restore_required','true') ON CONFLICT(key) DO UPDATE SET value='true'`)
	if err := e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM messages WHERE id='resurrection_message'`) != 0 {
		t.Fatal("backup restore resurrected deleted message")
	}
	if agStr(e, `SELECT value FROM settings WHERE key='retention_restore_required'`) != "false" {
		t.Fatal("replay did not clear retention restore gate")
	}
	if err := e.A.initRetention(t.Context(), retentionConfig{}); err == nil {
		t.Fatal("previously enrolled database started with retention disabled")
	}
	if err := e.A.initRetention(t.Context(), retentionConfig{Enabled: true, StateDir: t.TempDir()}); err == nil {
		t.Fatal("missing current ledger was replaced after enrollment")
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "retention-state.json"), []byte(`{"checksum":"bad","state":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.A.initRetention(t.Context(), cfg); err == nil {
		t.Fatal("corrupt ledger accepted")
	}
}

func TestRetentionPreparedRequestNeedsCommitProofAndRollbackAbort(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	uid, _ := e.user("retention_prepare_user", "buyer")
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.A.requestAccountDeletionTx(t.Context(), tx, uid)
	if err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if err = e.A.initRetention(t.Context(), cfg); err == nil {
		t.Fatal("ambiguous prepared request replayed without commit proof")
	}
	if err = e.A.abortRetentionOperation(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err = e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND deletion_requested_at IS NOT NULL`, uid) != 0 {
		t.Fatal("rolled-back request later became effective")
	}
	// Simulate crash between DB commit and manifest finalization.
	tx, err = e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.A.requestAccountDeletionTx(t.Context(), tx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(e.A.retention.state.Pending) != 0 || len(e.A.retention.state.Requests) != 1 {
		t.Fatal("committed crash gap was not finalized from DB proof")
	}
}

func TestRetentionAccountCredentialsExpireWhileFinancialIdentityRemains(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	uid, session := e.user("retention_financial_owner", "buyer")
	agExec(e, `INSERT INTO escrow_index_floors(user_id,next_index) VALUES($1,51)`, uid)
	agExec(e, `UPDATE users SET pgp='public key',totp_secret='sealed factor',totp_pending='sealed pending',recovery_reveal='sealed reveal',payout_btc='address',deletion_requested_at=now()-interval '31 days' WHERE id=$1`, uid)
	now := time.Now().UTC()
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND closed_at IS NOT NULL AND password_hash='' AND pgp='' AND totp_secret='' AND totp_pending='' AND recovery_reveal='' AND payout_btc=''`, uid) != 1 {
		t.Fatal("financial dependency prevented independent profile/credential erasure")
	}
	if agInt(e, `SELECT count(*) FROM escrow_index_floors WHERE user_id=$1 AND next_index=51`, uid) != 1 {
		t.Fatal("closure reset the non-reuse index floor")
	}
	u, err := e.A.sessionUser(t.Context(), session)
	if err != nil || u != nil {
		t.Fatalf("closed credentials still authenticated: %v %v", u, err)
	}
}

func TestRetentionAuthenticationAndDraftExpiry(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	vendor, _ := e.user("retention_auth_vendor", "vendor")
	buyer, _ := e.user("retention_auth_buyer", "buyer")
	product := e.product(vendor, "physical")
	draft := e.order(buyer, product, "BTC", stateDraft)
	agExec(e, `UPDATE orders SET created=now()-interval '31 days' WHERE id=$1`, draft)
	agExec(e, `INSERT INTO pending_logins(token_hash,user_id,expires) VALUES('retention_expired_pending',$1,now()-interval '1 minute')`, buyer)
	agExec(e, `INSERT INTO captcha_spent(id,expires) VALUES('retention_expired_captcha',now()-interval '1 minute')`)
	agExec(e, `INSERT INTO pgp_challenges(user_id,kind,fingerprint,challenge,expires) VALUES($1,'sign','test','temporary challenge',now()-interval '1 minute')`, buyer)
	// Authentication cleanup does not wait for the initial content-purge grace.
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM pending_logins WHERE token_hash='retention_expired_pending'`)+agInt(e, `SELECT count(*) FROM captcha_spent WHERE id='retention_expired_captcha'`)+agInt(e, `SELECT count(*) FROM pgp_challenges WHERE user_id=$1`, buyer) != 0 {
		t.Fatal("expired authentication records survived maintenance")
	}
	if agInt(e, `SELECT count(*) FROM orders WHERE id=$1`, draft) != 1 {
		t.Fatal("draft purged before enrollment notice grace")
	}
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM orders WHERE id=$1`, draft) != 0 {
		t.Fatal("unfunded draft did not expire")
	}
}

func TestRetentionLockIsNonblockingAndSharedAcrossServices(t *testing.T) {
	dir := t.TempDir()
	first := &retentionService{dir: dir}
	second := &retentionService{dir: dir}
	unlock, err := first.lock()
	if err != nil {
		t.Fatal(err)
	}
	if release, err := second.lock(); !errors.Is(err, errRetentionBusy) {
		if release != nil {
			release()
		}
		t.Fatalf("second process-equivalent lock did not refuse: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			if release, err := first.lock(); err == nil {
				release()
				t.Error("shared state mutex was not held")
			}
		}()
	}
	wg.Wait()
	unlock()
	unlock, err = second.lock()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestRetentionBackupHoldExtendsOnlyRelevantManifestEntries(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	active := retentionBackupHold{Kind: "postgresql_dump", CreatedAt: old}
	if !retentionBackupNeedsManifest([]retentionBackupHold{active}, now.Add(-40*24*time.Hour), now) {
		t.Fatal("active old preserved dump lost deletion history")
	}
	active.CreatedAt = now
	if retentionBackupNeedsManifest([]retentionBackupHold{active}, old, now) {
		t.Fatal("newer unrelated backup retained old tombstones")
	}
	active.Kind = "audit_export"
	active.CreatedAt = old
	if retentionBackupNeedsManifest([]retentionBackupHold{active}, old, now) {
		t.Fatal("held audit export extended database restore history")
	}
	released := now.Add(-9 * 24 * time.Hour)
	active.Kind = "postgresql_dump"
	active.ReleasedAt = &released
	if retentionBackupNeedsManifest([]retentionBackupHold{active}, old, now) {
		t.Fatal("released destroyed backup retained tombstones indefinitely")
	}
}

func TestRetentionStaleLiveManifestCannotEraseNewerHistory(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	uid, _ := e.user("retention_checkpoint_user", "buyer")
	p := filepath.Join(cfg.StateDir, "retention-state.json")
	old, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.A.requestAccountDeletion(t.Context(), uid); err != nil {
		t.Fatal(err)
	}
	checkpoint := agStr(e, `SELECT value FROM settings WHERE key='retention_manifest_sequence'`)
	if err = os.WriteFile(p, old, 0600); err != nil {
		t.Fatal(err)
	}
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.A.requestAccountDeletionTx(t.Context(), tx, uid)
	tx.Rollback()
	if !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("stale ledger accepted a live mutation: %v", err)
	}
	if agStr(e, `SELECT value FROM settings WHERE key='retention_manifest_sequence'`) != checkpoint {
		t.Fatal("live mutation rolled the database checkpoint backwards")
	}
	if err = e.A.finalizeRetentionOperation(t.Context(), "unknown"); !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("finalization accepted stale state: %v", err)
	}
	if err = e.A.abortRetentionOperation(t.Context(), "unknown"); !errors.Is(err, errRetentionStateUnavailable) {
		t.Fatalf("abort accepted stale state: %v", err)
	}
}

func TestRetentionClosedAccountSweepCannotStarveBehindFinancialReferences(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	agExec(e, `INSERT INTO users(id,handle,password_hash,role,created,closed_at) SELECT 'retention_blocked_'||n,'retention_blocked_'||n,'','buyer',now()-interval '200 days',now()-interval '10 days' FROM generate_series(1,200) n`)
	agExec(e, `INSERT INTO escrow_index_floors(user_id,next_index) SELECT id,51 FROM users WHERE id LIKE 'retention_blocked_%'`)
	agExec(e, `INSERT INTO users(id,handle,password_hash,role,closed_at) VALUES('retention_free_account','retention_free_account','','buyer',now()-interval '1 day')`)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM users WHERE id='retention_free_account'`) != 0 {
		t.Fatal("blocked first batch starved a deletable later account")
	}
	if agInt(e, `SELECT count(*) FROM escrow_index_floors WHERE user_id LIKE 'retention_blocked_%'`) != 200 {
		t.Fatal("fairness cleanup erased financial non-reuse state")
	}
}

func TestRetentionReconfirmedActivitySurvivesRestrictionReplay(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	uid, _ := e.user("retention_reconfirmed_vendor", "vendor")
	product := e.product(uid, "physical")
	oldAvatar := e.image(uid, "", "avatar")
	at := time.Now().UTC().Add(-time.Hour)
	agExec(e, `UPDATE users SET last_authenticated_at=$2,retention_restricted_at=$3,profile_scrubbed_at=$3 WHERE id=$1`, uid, at.Add(-time.Hour), at)
	agExec(e, `UPDATE products SET archived=true,archived_at=$2,updated=$2 WHERE id=$1`, product, at)
	agExec(e, `UPDATE images SET created=$2 WHERE id=$1`, oldAvatar.id, at.Add(-time.Hour))
	agExec(e, `INSERT INTO retention_account_exceptions(user_id,reason,first_seen,last_checked,review_due) VALUES($1,'open_escrow',$2::timestamptz,$2::timestamptz,$2::timestamptz+interval '30 days')`, uid, at)
	s := e.A.retention
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.read(); err == nil {
		committed := time.Now().UTC()
		s.state.Tombstones = append(s.state.Tombstones, retentionTombstone{Kind: "account_restrict", ID: uid, At: at, CommittedAt: &committed})
		err = s.write()
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM images WHERE id=$1`, oldAvatar.id) != 0 {
		t.Fatal("original restriction failed to erase its avatar")
	}
	if err = e.A.touchRetentionActivity(t.Context(), uid); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM retention_account_exceptions WHERE user_id=$1 AND resolved_at IS NOT NULL`, uid) != 1 {
		t.Fatal("reconfirmed inactivity left a current account exception")
	}
	agExec(e, `UPDATE users SET totp_pending='new security enrollment' WHERE id=$1`, uid)
	agExec(e, `UPDATE products SET archived=false,archived_at=NULL,updated=now() WHERE id=$1`, product)
	newAvatar := e.image(uid, "", "avatar")
	if err = e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND retention_restricted_at IS NULL AND totp_pending='new security enrollment'`, uid) != 1 {
		t.Fatal("restriction replay overrode later reconfirmation or security enrollment")
	}
	if agInt(e, `SELECT count(*) FROM products WHERE id=$1 AND NOT archived`, product) != 1 || agInt(e, `SELECT count(*) FROM images WHERE id=$1`, newAvatar.id) != 1 {
		t.Fatal("restriction replay erased later reactivated catalog or avatar")
	}
	if agInt(e, `SELECT count(*) FROM images WHERE id=$1`, oldAvatar.id) != 0 {
		t.Fatal("restriction replay resurrected the originally erased avatar")
	}
	// Reauthentication cannot rescind an already-accepted deletion request.
	agExec(e, `UPDATE users SET deletion_requested_at=$2 WHERE id=$1`, uid, at.Add(-31*24*time.Hour))
	if err = e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND retention_restricted_at IS NOT NULL`, uid) != 1 {
		t.Fatal("later authentication silently rescinded a verified deletion request")
	}
}

func TestRetentionClosedStaffCannotReceiveDisputeEvidence(t *testing.T) {
	w := newDisputeOrderWorld(t)
	admin, _ := w.e.user("retention_active_dispute_admin", "admin")
	tx, err := w.e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "account_close", ID: w.modID, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The parcel arrived empty and the seal was broken.")
	if w.resolverNotices(w.modID) != 0 || w.resolverNotices(admin) != 1 {
		t.Fatal("closed staff remained a dispute evidence recipient")
	}
	if agInt(w.e, `SELECT count(*) FROM users WHERE id=$1 AND suspended_at IS NOT NULL AND closed_at IS NOT NULL`, w.modID) != 1 {
		t.Fatal("closed staff remained eligible for existing active-staff predicates")
	}
}

func TestRetentionRejectsDuplicateJSONAndMalformedBackupRegisters(t *testing.T) {
	for _, b := range []string{`{"version":1,"holds":[],"holds":[]}`, `{"outer":{"scope":"account","scope":"message"}}`, `{"outer":[{"id":1,"id":2}]}`} {
		var value any
		if err := strictRetentionJSON([]byte(b), &value); err == nil {
			t.Fatal("ambiguous duplicate-key retention JSON accepted")
		}
	}
	s := &retentionService{dir: retentionPrivateTestDir(t)}
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-100 * 24 * time.Hour)
	h := retentionBackupHold{CaseID: "case-1", Kind: "postgresql_dump", ArchivePath: "/private/tmp/preserved.dump.age", SHA256: digest("copy"), CreatedAt: created, ExpiresAt: created.Add(30 * 24 * time.Hour), Reason: "Specific relevant copy", Owner: "Assigned owner", AuthorizedReaders: []string{"Case handler"}, PermittedUse: "Isolated evidence review", StartedAt: now.Add(-60 * 24 * time.Hour), ReviewedAt: now.Add(-31 * 24 * time.Hour), NextReviewAt: now.Add(-2 * 24 * time.Hour), ReleaseCondition: "Written release"}
	write := func() {
		b, err := json.Marshal(retentionBackupRegister{Version: 1, Holds: []retentionBackupHold{h}})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(s.dir, "backup-preservations.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := s.backupRegister(); err != nil {
		t.Fatal("valid overdue legal hold rejected:", err)
	}
	h.AuthorizedReaders = []string{" "}
	write()
	if _, err := s.backupRegister(); err == nil {
		t.Fatal("empty authorized reader accepted")
	}
	h.AuthorizedReaders = []string{"Case handler"}
	h.NextReviewAt = h.ReviewedAt
	write()
	if _, err := s.backupRegister(); err == nil {
		t.Fatal("invalid backup review clock accepted")
	}
	h.NextReviewAt = h.ReviewedAt.Add(24 * time.Hour)
	h.CreatedAt = now.Add(24 * time.Hour)
	h.ExpiresAt = h.CreatedAt.Add(30 * 24 * time.Hour)
	write()
	if _, err := s.backupRegister(); err == nil {
		t.Fatal("future original backup creation accepted")
	}
}

func TestRetentionObservedCommitProtectsRacingPreservedBackup(t *testing.T) {
	start := time.Now().UTC().Add(-40 * 24 * time.Hour)
	observed := start.Add(time.Hour)
	s := &retentionService{state: retentionState{Pending: []retentionOperation{{ID: "purge", At: start, Tombstones: []retentionTombstone{{Kind: "message", ID: "opaque", At: start}}}}}}
	s.commitOperation(s.state.Pending[0])
	tombstone := s.state.Tombstones[0]
	if tombstone.CommittedAt == nil || tombstone.CommittedAt.Before(observed) {
		t.Fatal("commit evidence was incorrectly stamped at pass start")
	}
	backup := retentionBackupHold{Kind: "postgresql_dump", CreatedAt: start.Add(30 * time.Minute)}
	if !retentionBackupNeedsManifest([]retentionBackupHold{backup}, *tombstone.CommittedAt, time.Now().UTC()) {
		t.Fatal("backup snapshot racing the deletion lost its restore history")
	}
}

func TestRetentionAuditReplayProtectsNewRowsAndSequenceFloor(t *testing.T) {
	e, cfg := uiRetentionEnv(t)
	uid, _ := e.user("retention_audit_reuse_user", "buyer")
	agExec(e, `INSERT INTO audit_events(id,user_id,action,created) VALUES(9000,$1,'old action',now()-interval '181 days')`, uid)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	agExec(e, `INSERT INTO audit_events(id,user_id,action) VALUES(9000,$1,'new unexpired action')`, uid)
	agExec(e, `SELECT setval('audit_events_id_seq',1,false)`)
	if err := e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM audit_events WHERE id=9000 AND action='new unexpired action'`) != 1 {
		t.Fatal("old numeric tombstone erased a newer audit row")
	}
	if agInt(e, `INSERT INTO audit_events(action) VALUES('after replay') RETURNING id`) <= 9000 {
		t.Fatal("restore reused an externally retired audit ID")
	}
}

func TestRetentionLegacyEnrollmentIsBoundedToNinetyDays(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	uid, _ := e.user("retention_legacy_inactive_user", "buyer")
	now := time.Now().UTC()
	enabled := now.Add(-91 * 24 * time.Hour)
	agExec(e, `UPDATE users SET created=$2,last_authenticated_at=NULL WHERE id=$1`, uid, enabled.Add(-24*time.Hour))
	ageRetentionEnrollment(t, e.A, enabled)
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND closed_at IS NULL`, uid) != 0 {
		t.Fatal("late enrollment sweep restarted a fresh30-day notice after the90-day limit")
	}
}

func TestRetentionUnactivatedNewAccountsReceiveTwelveMonthLifecycle(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	buyer, _ := e.user("retention_unactivated_buyer", "buyer")
	admin, _ := e.user("retention_unactivated_only_admin", "admin")
	now := time.Now().UTC()
	ageRetentionEnrollment(t, e.A, now.AddDate(0, -15, 0))
	agExec(e, `UPDATE users SET created=$1,last_authenticated_at=NULL WHERE id IN($2,$3)`, now.AddDate(0, -13, 0), buyer, admin)
	runRetentionAt(t, e.A, now)
	if agInt(e, `SELECT count(*) FROM users WHERE id IN($1,$2) AND last_authenticated_at IS NULL AND inactivity_notified_at=$3 AND closed_at IS NULL AND retention_restricted_at IS NULL`, buyer, admin, now) != 2 {
		t.Fatal("never-activated new accounts were skipped or given the historical enrollment deadline")
	}
	runRetentionAt(t, e.A, now.Add(31*24*time.Hour))
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND closed_at IS NULL`, buyer) != 0 {
		t.Fatal("never-activated buyer survived the full inactivity notice window")
	}
	if agInt(e, `SELECT count(*) FROM users WHERE id=$1 AND last_authenticated_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NOT NULL`, admin) != 1 || agInt(e, `SELECT count(*) FROM retention_account_exceptions WHERE user_id=$1 AND reason='administrator_continuity' AND resolved_at IS NULL AND review_due=first_seen+interval '30 days'`, admin) != 1 {
		t.Fatal("never-activated sole administrator lacked a bounded continuity exception")
	}
}

func TestRetentionPrivateReplayPreservesLaterResolutionAndResetsExpiryMarker(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	vendor, _ := e.user("retention_epoch_vendor", "vendor")
	buyer, _ := e.user("retention_epoch_buyer", "buyer")
	product := e.product(vendor, "digital")
	order := e.order(buyer, product, "BTC", stateResolved)
	agExec(e, `INSERT INTO disputes(id,order_id,reason,status,outcome,resolution,created) VALUES('retention_old_dispute',$1,'old private evidence','Resolved — release to vendor','release','old private decision',now()-interval '60 days')`, order)
	agExec(e, `UPDATE orders SET financial_closed_at=now()-interval '31 days' WHERE id=$1`, order)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	agExec(e, `UPDATE disputes SET reason='restored old private evidence',resolution='new legitimate decision' WHERE order_id=$1`, order)
	agExec(e, `UPDATE orders SET financial_closed_at=now(),private_expired_at=NULL WHERE id=$1`, order)
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.A.replayRetention(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if agStr(e, `SELECT reason FROM disputes WHERE order_id=$1`, order) != "Private evidence expired under retention policy" {
		t.Fatal("old private reason was resurrected")
	}
	if agStr(e, `SELECT resolution FROM disputes WHERE order_id=$1`, order) != "new legitimate decision" {
		t.Fatal("original purge erased a later legitimate resolution")
	}
	if agInt(e, `SELECT count(*) FROM orders WHERE id=$1 AND private_expired_at IS NULL`, order) != 1 {
		t.Fatal("old replay marked new private-body epoch permanently expired")
	}
}

func TestRetentionRestrictedClosureKeepsExistingEscrowPartySigning(t *testing.T) {
	x := newEscrowEnv(t)
	order, settlement := x.shippedPhysical("bcrt1qretentionbuyerrefund000000000001", "bcrt1qretentionvendorclaim000000000001", 100000)
	cfg := retentionConfig{Enabled: true, StateDir: retentionPrivateTestDir(t)}
	if err := x.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	agExec(x.testEnv, `UPDATE users SET deletion_requested_at=now()-interval '31 days',totp_pending='unused pending material',recovery_reveal='unused reveal',payout_btc='unrelated address' WHERE id=$1`, x.buyer)
	now := time.Now().UTC()
	ageRetentionEnrollment(t, x.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, x.A, now)
	if agInt(x.testEnv, `SELECT count(*) FROM users WHERE id=$1 AND closed_at IS NULL AND retention_restricted_at IS NOT NULL AND profile_scrubbed_at IS NOT NULL AND password_hash<>'' AND totp_pending='' AND recovery_reveal='' AND payout_btc=''`, x.buyer) != 1 {
		t.Fatal("closure either retained unrelated profile or stranded party authentication")
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_account_exceptions WHERE user_id=$1 AND reason='open_escrow' AND resolved_at IS NULL`, x.buyer) != 1 {
		t.Fatal("continued authentication has no reviewable scoped basis")
	}
	copy := x.partyCopy(order, x.buyerSess, settlement)
	k, err := parseEscrowKey(x.buyerKey, keyNetTest)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := x.sign(order, x.buyerSess, settlement, walletSign(t, copy, k.Fingerprint, 1)); code != 303 {
		t.Fatalf("restricted buyer could not sign existing settlement: %d %s", code, body)
	}
	x.check(x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {x.product}, "currency": {"BTC"}}), 403)
}

func TestRetentionUserForeignKeyInventoryIsExplicit(t *testing.T) {
	e := newTestApp(t)
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	unknown, err := retentionUnknownUserReferences(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if unknown {
		t.Fatal("a user foreign key lacks reviewed retention lifecycle handling")
	}
	if _, err = tx.ExecContext(t.Context(), `CREATE TABLE retention_future_dependency(user_id text REFERENCES users(id))`); err != nil {
		t.Fatal(err)
	}
	unknown, err = retentionUnknownUserReferences(t.Context(), tx)
	if err != nil || !unknown {
		t.Fatalf("new unreviewed dependency was not protected: %t %v", unknown, err)
	}
}
