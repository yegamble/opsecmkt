package market

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func enrollmentTestEnv(dir string) func(string) string {
	return func(name string) string {
		switch name {
		case "RETENTION_ENABLED":
			return "on"
		case "RETENTION_STATE_DIR":
			return dir
		}
		return ""
	}
}

func TestHostRetentionEnrollmentMigratesOriginalWithoutMaintenance(t *testing.T) {
	e := newPooledHistoricalFixture(t, 111)
	agExec(e, `INSERT INTO users(id,handle,password_hash,role) VALUES('enroll_sender','enroll_sender','x','buyer'),('enroll_recipient','enroll_recipient','x','buyer')`)
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,created) VALUES('enroll_old','enroll_sender','enroll_recipient','old private bytes',now()-interval '365 days')`)
	agExec(e, `INSERT INTO sessions(token_hash,user_id,expires) VALUES('enroll_expired','enroll_sender',now()-interval '1 day')`)
	dir := retentionPrivateTestDir(t)
	if err := EnrollRetention(t.Context(), e.DB, enrollmentTestEnv(dir)); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM schema_migrations WHERE version>=112`) == 0 || agInt(e, `SELECT count(*) FROM messages WHERE id='enroll_old'`) != 1 || agInt(e, `SELECT count(*) FROM sessions WHERE token_hash='enroll_expired'`) != 1 {
		t.Fatal("host enrollment did not migrate or purged original data")
	}
	if agInt(e, `SELECT count(*) FROM retention_runs`) != 0 || agInt(e, `SELECT count(*) FROM notifications`) != 0 || agInt(e, `SELECT count(*) FROM settings WHERE key='retention_restore_required' OR key='retention_enrollment_notified'`) != 0 {
		t.Fatal("host enrollment performed serving startup or maintenance")
	}
	before, err := os.ReadFile(filepath.Join(dir, "retention-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = EnrollRetention(t.Context(), e.DB, enrollmentTestEnv(dir)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "retention-state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("repeated host enrollment changed the original grace clocks")
	}
	// Simulate a crash after the initial file was published but before its DB
	// pin committed. An empty ledger can be adopted without inventing new clocks.
	agExec(e, `DELETE FROM settings WHERE key IN('retention_instance_id','retention_manifest_sequence')`)
	if err = EnrollRetention(t.Context(), e.DB, enrollmentTestEnv(dir)); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(filepath.Join(dir, "retention-state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("crash retry reset the original grace clocks")
	}
}

func TestHostRetentionEnrollmentRefusesRestoredOriginalBeforeMigration(t *testing.T) {
	e := newPooledHistoricalFixture(t, 111)
	agExec(e, `INSERT INTO settings(key,value) VALUES('retention_restore_required','true')`)
	dir := filepath.Join(retentionPrivateTestDir(t), "must-not-create")
	err := EnrollRetention(t.Context(), e.DB, enrollmentTestEnv(dir))
	if err == nil || !strings.Contains(err.Error(), "pending or invalid restore gate") {
		t.Fatalf("restored source enrollment: %v", err)
	}
	if agInt(e, `SELECT max(version) FROM schema_migrations`) != 111 || agInt(e, `SELECT count(*) FROM settings WHERE key='retention_instance_id'`) != 0 {
		t.Fatal("refused restored source was migrated or pinned")
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("refused restored source created external enrollment state")
	}
}

func TestHostRetentionEnrollmentRefusesConflictingOrUsedState(t *testing.T) {
	for _, kind := range []string{"missing", "different_instance", "used", "stale", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			e := newPooledHistoricalFixture(t, 111)
			dir := retentionPrivateTestDir(t)
			if err := EnrollRetention(t.Context(), e.DB, enrollmentTestEnv(dir)); err != nil {
				t.Fatal(err)
			}
			s := &retentionService{dir: dir}
			if err := s.read(); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				os.Remove(filepath.Join(dir, "retention-state.json"))
			case "corrupt":
				os.WriteFile(filepath.Join(dir, "retention-state.json"), []byte("broken"), 0600)
			case "different_instance":
				s.state.InstanceID = randomToken()
				s.write()
			case "used":
				now := time.Now().UTC()
				s.state.Tombstones = []retentionTombstone{{Kind: "message", ID: "erased", At: now, CommittedAt: &now}}
				s.write()
			case "stale":
				agExec(e, `UPDATE settings SET value='999' WHERE key='retention_manifest_sequence'`)
			}
			if err := EnrollRetention(t.Context(), e.DB, enrollmentTestEnv(dir)); err == nil {
				t.Fatal("host enrollment accepted conflicting or previously used state")
			}
		})
	}
}

func TestHostRetentionEnrollmentCapturesExistingDestructionBeforePublishing(t *testing.T) {
	closed := time.Now().UTC().Add(-180 * 24 * time.Hour).Truncate(time.Second)
	due, destroyed := closed.Add(90*24*time.Hour), closed.Add(91*24*time.Hour)
	seedOriginal := func(e *testEnv) {
		agExec(e, `INSERT INTO xmr_escrow_wallets(name,month,state,last_closed_at,destroy_after,destroyed_at) VALUES('escrow-202601-01','2026-01-01','destroyed',$1,$2,$3)`, closed, due, destroyed)
	}
	e := newPooledHistoricalFixture(t, 111)
	seedOriginal(e)
	dir := retentionPrivateTestDir(t)
	if err := EnrollRetention(t.Context(), e.DB, enrollmentTestEnv(dir)); err != nil {
		t.Fatal(err)
	}
	s := &retentionService{dir: dir}
	if err := s.read(); err != nil {
		t.Fatal(err)
	}
	if s.state.Sequence != 1 || len(s.state.Tombstones) != 1 || s.state.Tombstones[0].Kind != "xmr_wallet_destroy" || s.state.Tombstones[0].XMRDestruction == nil || s.state.Tombstones[0].CommittedAt == nil {
		t.Fatal("first published original enrollment omitted an already erased wallet seed")
	}
	before, err := os.ReadFile(filepath.Join(dir, "retention-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the same pre-enrollment original to model migration/pin rollback
	// after the durable initial file was published. Its trusted wallet facts are
	// identical; the host command must neither reset clocks nor drop certificates.
	rolledBack := newPooledHistoricalFixture(t, 111)
	seedOriginal(rolledBack)
	if err = EnrollRetention(t.Context(), rolledBack.DB, enrollmentTestEnv(dir)); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "retention-state.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("interrupted original enrollment changed destruction evidence or clocks: %v", err)
	}
	// A historical snapshot from before that destruction cannot adopt the file
	// through the enrollment-only path, even when it lacks a first-enrollment pin.
	older := newPooledHistoricalFixture(t, 111)
	if err = EnrollRetention(t.Context(), older.DB, enrollmentTestEnv(dir)); err == nil {
		t.Fatal("source lacking the recorded destruction adopted original enrollment")
	}
}
