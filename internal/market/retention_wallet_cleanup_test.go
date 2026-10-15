package market

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetentionWalletCleanupKeepsOriginalDeadlineAndExpiresConfirmation(t *testing.T) {
	e := newTestApp(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-24 * time.Hour)
	agExec(e, `SELECT retention_wallet_cleanup_register('XMR','protected-watch',$1,$2)`, due, now)
	agExec(e, `SELECT retention_wallet_cleanup_register('XMR','protected-watch',$1,$2)`, now.Add(24*time.Hour), now.Add(time.Hour))
	rows, err := LoadRetentionWalletCleanup(t.Context(), e.DB, now.Add(31*24*time.Hour), 100)
	if err != nil || len(rows) != 1 || !rows[0].DueAt.Equal(due) || !rows[0].Overdue || !rows[0].ReviewOverdue {
		t.Fatalf("original deadline/review changed: %+v %v", rows, err)
	}
	// A review does not assert files or descriptors were destroyed.
	agExec(e, `UPDATE retention_wallet_cleanup SET review_due=$1 WHERE coin='XMR' AND wallet_name='protected-watch'`, now.Add(60*24*time.Hour))
	agExec(e, `UPDATE retention_wallet_cleanup SET confirmed_at=$1,confirmed_by='test host operator' WHERE coin='XMR' AND wallet_name='protected-watch'`, now)
	runRetentionClosure(t, e, now.Add(179*24*time.Hour))
	if agInt(e, `SELECT count(*) FROM retention_wallet_cleanup`) != 1 {
		t.Fatal("confirmation expired before180days")
	}
	// A genuinely later certificate restores the unresolved obligation while its
	// original deadline stays fixed. An old certificate replay cannot undo a
	// newer verified confirmation still present in the database.
	agExec(e, `SELECT retention_wallet_cleanup_register('XMR','protected-watch',$1,$2)`, due, now.Add(-time.Hour))
	if agInt(e, `SELECT count(*) FROM retention_wallet_cleanup WHERE confirmed_at IS NOT NULL`) != 1 {
		t.Fatal("old replay undid later physical confirmation")
	}
	agExec(e, `SELECT retention_wallet_cleanup_register('XMR','protected-watch',$1,$2)`, now, now.Add(time.Hour))
	if agInt(e, `SELECT count(*) FROM retention_wallet_cleanup WHERE confirmed_at IS NULL AND due_at=$1`, due) != 1 {
		t.Fatal("later certificate failed to reopen original obligation")
	}
	agExec(e, `UPDATE retention_wallet_cleanup SET confirmed_at=$1,confirmed_by='test host operator'`, now)
	runRetentionClosure(t, e, now.Add(180*24*time.Hour))
	if agInt(e, `SELECT count(*) FROM retention_wallet_cleanup`) != 0 {
		t.Fatal("confirmed host inventory was retained indefinitely")
	}
}

func TestRetentionEscrowJournalReconstitutesPreOrderRestoreState(t *testing.T) {
	x, order, r, tx := certifiedXMRRetirement(t)
	tx.Rollback()
	if err := x.A.initRetention(t.Context(), retentionConfig{Enabled: true, StateDir: retentionPrivateTestDir(t)}); err != nil {
		t.Fatal(err)
	}
	s := x.A.retention
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.read(); err != nil {
		t.Fatal(err)
	}
	tx, err = x.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	previousTombstones := len(s.state.Tombstones)
	opens := x.opens(r.WalletName)
	op := retentionOperation{ID: randomToken(), At: r.CertifiedAt, Tombstones: []retentionTombstone{{Kind: "escrow_order", ID: order, At: r.CertifiedAt, Retirement: r}}}
	if err = x.A.prepareRetentionOperation(t.Context(), tx, op); err != nil {
		t.Fatal(err)
	}
	if x.opens(r.WalletName) != opens {
		t.Fatal("journal preparation accessed the destroyed wallet")
	}
	if err = x.A.applyRetentionEscrowRetirement(t.Context(), tx, r); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err = x.A.finalizeRetentionOperation(t.Context(), op.ID); err != nil {
		t.Fatal(err)
	}
	// A snapshot from before this order existed has none of its financial rows,
	// sparse guards or wallet-name allocation counters. The external committed certificate must
	// reconstruct guards and protected physical-cleanup inventory before traffic.
	agExec(x.testEnv, `DELETE FROM retention_escrow_retired_addresses`)
	agExec(x.testEnv, `DELETE FROM retention_xmr_retired_wallets`)
	agExec(x.testEnv, `DELETE FROM retention_xmr_wallet_names`)
	agExec(x.testEnv, `DELETE FROM retention_wallet_cleanup`)
	x.A.escrowXMR = nil
	unlock, err = s.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = s.read(); err != nil {
		t.Fatal(err)
	}
	var committed *retentionTombstone
	for i := range s.state.Tombstones {
		marker := &s.state.Tombstones[i]
		if marker.Kind == "escrow_order" && marker.ID == order {
			committed = marker
		}
	}
	if len(s.state.Pending) != 0 || len(s.state.Tombstones) != previousTombstones+1 || committed == nil || committed.CommittedAt == nil {
		t.Fatal("committed certificate missing from durable journal")
	}
	tx, err = x.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = x.A.applyRetentionTombstone(t.Context(), tx, *committed); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_wallet_cleanup WHERE coin='XMR' AND wallet_name=$1 AND due_at=$2`, r.WalletName, r.CleanupDue) != 1 {
		t.Fatal("old restore lost physical cleanup obligation")
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_escrow_retired_addresses WHERE coin='XMR'`) != len(r.AddressHashes) {
		t.Fatal("old restore lost retired address non-reuse hashes")
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_xmr_retired_wallets WHERE name_hash=$1`, retentionTechnicalHash(r.WalletName)) != 1 {
		t.Fatal("old restore lost destroyed wallet non-reuse guard")
	}
	var next int
	if err = x.DB.QueryRow(`SELECT next_n FROM retention_xmr_wallet_names WHERE month=$1::date`, r.WalletMonth).Scan(&next); err != nil || next < r.WalletNextN {
		t.Fatalf("restore reused old Monero wallet sequence %d: %v", next, err)
	}
}

func TestRetentionEscrowPreparedCrashDoesNotInventRetirement(t *testing.T) {
	x, order, r, tx := certifiedXMRRetirement(t)
	tx.Rollback()
	if err := x.A.initRetention(t.Context(), retentionConfig{Enabled: true, StateDir: retentionPrivateTestDir(t)}); err != nil {
		t.Fatal(err)
	}
	s := x.A.retention
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = s.read(); err != nil {
		t.Fatal(err)
	}
	tx, err = x.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeCleanup := agInt(x.testEnv, `SELECT count(*) FROM retention_wallet_cleanup`)
	opens := x.opens(r.WalletName)
	op := retentionOperation{ID: randomToken(), At: r.CertifiedAt, Tombstones: []retentionTombstone{{Kind: "escrow_order", ID: order, At: r.CertifiedAt, Retirement: r}}}
	if err = x.A.prepareRetentionOperation(t.Context(), tx, op); err != nil {
		t.Fatal(err)
	}
	tx.Rollback() // crash/older dump before database commit
	if err = s.read(); err != nil {
		t.Fatal(err)
	}
	if err = x.A.resolveRetentionPending(t.Context()); err == nil || !strings.Contains(err.Error(), "unresolved retention commit") {
		t.Fatalf("ambiguous prepared retirement guessed committed: %v", err)
	}
	if agInt(x.testEnv, `SELECT count(*) FROM orders WHERE id=$1`, order) != 1 || agInt(x.testEnv, `SELECT count(*) FROM retention_wallet_cleanup`) != beforeCleanup || x.opens(r.WalletName) != opens {
		t.Fatal("ambiguous pending journal erased rows or asserted wallet cleanup")
	}
}

func TestRetentionEscrowEmptyRecoveryDoesNotRestartClosure(t *testing.T) {
	x, order, r, tx := certifiedXMRRetirement(t)
	tx.Rollback()
	agExec(x.testEnv, `UPDATE escrows SET recover_requested=$2 WHERE order_id=$1`, order, r.ClosedAt.Add(time.Minute))
	if got := retentionClosureAt(t, x.testEnv, order); !got.Valid || !got.Time.Equal(r.ClosedAt) {
		t.Fatal("an unverified request restarted the closure clock")
	}
	runRetentionClosure(t, x.testEnv, r.CertifiedAt)
	if got := retentionClosureAt(t, x.testEnv, order); !got.Valid || !got.Time.Equal(r.ClosedAt) {
		t.Fatal("pending empty recovery was treated as new financial facts")
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_escrow_exceptions WHERE order_id=$1 AND reason='pending_recovery' AND resolved_at IS NULL`, order) != 1 {
		t.Fatal("pending recovery did not pause due retirement for review")
	}
}

func TestRetentionMoneroPreservationDutyKeepsRecoverySeed(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	name := x.walletOf(order)
	x.rotate()
	x.days(name, 91)
	party := agStr(x.testEnv, `SELECT buyer_id FROM orders WHERE id=$1`, order)
	agExec(x.testEnv, `INSERT INTO retention_holds(id,scope,record_id,authority,reason,owner,readers,release_condition,began_at,review_at) VALUES('wallet_duty','account',$1,'specific preservation duty','wallet recovery evidence','operator','operator','released',$2,$3)`, party, x.now, x.now.Add(retentionEscrowReview))
	before := x.fake.countOf("change_wallet_password")
	x.mustPoll()
	if x.walletState(name) != "retained" || x.fake.countOf("change_wallet_password") != before || agInt(x.testEnv, `SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND seed_sealed IS NOT NULL`, name) != 1 {
		t.Fatal("account preservation duty erased wallet recovery state")
	}
	if _, err := x.DB.Exec(`UPDATE xmr_escrow_wallets SET destroy_started_at=$2 WHERE name=$1`, name, x.now); err == nil {
		t.Fatal("database allowed beginning destruction under applicable duty")
	}
	agExec(x.testEnv, `UPDATE retention_holds SET released_at=$1 WHERE id='wallet_duty'`, x.now)
	x.mustPoll()
	if x.walletState(name) != "destroyed" || x.fake.countOf("change_wallet_password") != before+1 {
		t.Fatal("released duty restarted deadline or indefinitely retained empty wallet")
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_wallet_cleanup WHERE coin='XMR' AND wallet_name=$1 AND due_at<=$2`, name, x.now) != 1 {
		t.Fatal("guarded destruction failed to expose original physical cleanup deadline")
	}
}

func TestRetentionMoneroHoldRegistrationSerializesWithPasswordDestruction(t *testing.T) {
	fixture := func(t *testing.T) (*xmrEscrowEnv, string, string, retentionHold) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, _ := x.settled()
		name := x.walletOf(order)
		x.rotate()
		x.days(name, 91)
		if err := x.A.initRetention(t.Context(), retentionConfig{Enabled: true, StateDir: retentionPrivateTestDir(t)}); err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC()
		h := retentionHold{ID: randomToken(), Scope: "order", RecordID: order, Authority: "specific duty", Reason: "wallet recovery evidence", Owner: "operator", Readers: "operator", ReleaseCondition: "explicit release", BeganAt: at, ReviewAt: at.Add(retentionEscrowReview)}
		return x, order, name, h
	}
	t.Run("pending registration first", func(t *testing.T) {
		x, _, name, h := fixture(t)
		tx, err := x.DB.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		op, err := x.A.setRetentionHoldTx(t.Context(), tx, h)
		if err != nil {
			t.Fatal(err)
		}
		before := x.fake.countOf("change_wallet_password")
		x.mustPoll() // duty is still uncommitted, but owns exact wallet lock
		if x.fake.countOf("change_wallet_password") != before || x.walletState(name) != "retained" {
			t.Fatal("pending hold registration lost ordering to password destruction")
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err = x.A.finalizeRetentionOperation(t.Context(), op); err != nil {
			t.Fatal(err)
		}
		x.mustPoll()
		if x.fake.countOf("change_wallet_password") != before || agInt(x.testEnv, `SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND seed_sealed IS NOT NULL`, name) != 1 {
			t.Fatal("committed duty was bypassed by irreversible wallet RPC")
		}
	})
	t.Run("password destruction first", func(t *testing.T) {
		x, _, name, h := fixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once atomic.Bool
		x.fake.hook("change_wallet_password", func() {
			if once.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
		})
		free := func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		defer free()
		done := make(chan error, 1)
		go func() { done <- x.poll() }()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("destruction did not reach protected password RPC")
		}
		tx, err := x.DB.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		op, err := x.A.setRetentionHoldTx(t.Context(), tx, h)
		var retry *httpError
		if err == nil || op != "" || !errors.As(err, &retry) || retry.Code != 409 {
			t.Fatalf("pending registration was not explicitly retriable before journaling: %q %v", op, err)
		}
		tx.Rollback()
		// Password destruction now has its own pending external certificate at
		// this point. The refused preservation request must not add a hold intent
		// or remove that legitimate destruction intent.
		unlock, err := x.A.retention.lock()
		if err != nil {
			t.Fatal(err)
		}
		if err = x.A.retention.read(); err != nil {
			unlock()
			t.Fatal(err)
		}
		pendingHold := false
		pendingDestruction := false
		for _, operation := range x.A.retention.state.Pending {
			if operation.Hold != nil && operation.Hold.ID == h.ID {
				pendingHold = true
			}
			for _, marker := range operation.Tombstones {
				if marker.Kind == "xmr_wallet_destroy" && marker.ID == name && marker.CommittedAt == nil {
					pendingDestruction = true
				}
			}
		}
		unlock()
		if agInt(x.testEnv, `SELECT count(*) FROM retention_holds WHERE id=$1`, h.ID) != 0 || pendingHold || !pendingDestruction {
			t.Fatal("rejected registration must preserve destruction intent without inventing a committed/pending preservation duty")
		}
		free()
		select {
		case err = <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("protected destruction did not complete")
		}
		if x.walletState(name) != "destroyed" {
			t.Fatal("first destruction lost exact wallet ownership")
		}
		if len(x.A.retention.state.Pending) != 0 {
			t.Fatal("completed destruction left unresolved intent")
		}
	})
}

func TestRetentionFinancialProofOutagePreservesPartyAccess(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	name := x.walletOf(order)
	party := agStr(x.testEnv, `SELECT buyer_id FROM orders WHERE id=$1`, order)
	var terminal time.Time
	if err := x.DB.QueryRow(`SELECT updated FROM orders WHERE id=$1`, order).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	at := terminal.Add(90 * 24 * time.Hour)
	obligations := func(want bool, at time.Time) {
		t.Helper()
		runRetentionClosure(t, x.testEnv, at)
		tx, err := x.DB.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		needed, err := retentionAccountObligations(t.Context(), tx, party, at)
		if err != nil || needed != want {
			t.Fatalf("party access obligation = %v, want %v: %v", needed, want, err)
		}
	}
	// Confirmed settlement alone cannot retire an account while its wallet still
	// holds recoverable keys. Losing the RPC adapter must preserve that access.
	obligations(true, at)
	rpc := x.A.escrowXMR
	x.A.escrowXMR = nil
	obligations(true, at.Add(31*time.Minute))
	x.A.escrowXMR = rpc
	// Guarded password destruction supplies the Monero closure proof. Once it is
	// recorded, a historical account no longer depends on the unavailable wallet.
	x.rotate()
	x.days(name, 91)
	x.mustPoll()
	if x.walletState(name) != "destroyed" {
		t.Fatal("restored RPC did not complete guarded wallet destruction")
	}
	obligations(false, x.now)
}
