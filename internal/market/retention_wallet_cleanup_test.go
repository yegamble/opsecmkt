package market

import (
	"context"
	"encoding/json"
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
	agExec(e, `SELECT retention_wallet_cleanup_register('BTC','protected-watch',$1,$2)`, due, now)
	agExec(e, `SELECT retention_wallet_cleanup_register('BTC','protected-watch',$1,$2)`, now.Add(24*time.Hour), now.Add(time.Hour))
	rows, err := LoadRetentionWalletCleanup(t.Context(), e.DB, now.Add(31*24*time.Hour), 100)
	if err != nil || len(rows) != 1 || !rows[0].DueAt.Equal(due) || !rows[0].Overdue || !rows[0].ReviewOverdue {
		t.Fatalf("original deadline/review changed: %+v %v", rows, err)
	}
	// A review does not assert files or descriptors were destroyed.
	agExec(e, `UPDATE retention_wallet_cleanup SET review_due=$1 WHERE coin='BTC' AND wallet_name='protected-watch'`, now.Add(60*24*time.Hour))
	agExec(e, `UPDATE retention_wallet_cleanup SET confirmed_at=$1,confirmed_by='test host operator' WHERE coin='BTC' AND wallet_name='protected-watch'`, now)
	runRetentionClosure(t, e, now.Add(179*24*time.Hour))
	if agInt(e, `SELECT count(*) FROM retention_wallet_cleanup`) != 1 {
		t.Fatal("confirmation expired before180days")
	}
	// A genuinely later certificate restores the unresolved obligation while its
	// original deadline stays fixed. An old certificate replay cannot undo a
	// newer verified confirmation still present in the database.
	agExec(e, `SELECT retention_wallet_cleanup_register('BTC','protected-watch',$1,$2)`, due, now.Add(-time.Hour))
	if agInt(e, `SELECT count(*) FROM retention_wallet_cleanup WHERE confirmed_at IS NOT NULL`) != 1 {
		t.Fatal("old replay undid later physical confirmation")
	}
	agExec(e, `SELECT retention_wallet_cleanup_register('BTC','protected-watch',$1,$2)`, now, now.Add(time.Hour))
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
	x, order, r, tx := certifiedBTCRetirement(t)
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
	op := retentionOperation{ID: randomToken(), At: r.CertifiedAt, Tombstones: []retentionTombstone{{Kind: "escrow_order", ID: order, At: r.CertifiedAt, Retirement: r}}}
	if err = x.A.prepareRetentionOperation(t.Context(), tx, op); err != nil {
		t.Fatal(err)
	}
	if x.core.count("setlabel") != 0 {
		t.Fatal("journal preparation changed the wallet")
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
	// labels, sparse guards or counters. The external committed certificate must
	// reconstruct guards and protected physical-cleanup inventory before traffic.
	agExec(x.testEnv, `DELETE FROM retention_escrow_retired_addresses`)
	agExec(x.testEnv, `DELETE FROM retention_escrow_retired_keys`)
	agExec(x.testEnv, `DELETE FROM retention_escrow_key_floors`)
	agExec(x.testEnv, `DELETE FROM retention_wallet_cleanup`)
	agExec(x.testEnv, `UPDATE retention_escrow_global_floors SET next_index=0,allocation_next_index=0`)
	agExec(x.testEnv, `SELECT setval('escrow_market_index',0,false)`)
	x.A.escrow.Store(nil)
	unlock, err = s.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = s.read(); err != nil {
		t.Fatal(err)
	}
	if len(s.state.Pending) != 0 || len(s.state.Tombstones) != 1 || s.state.Tombstones[0].CommittedAt == nil {
		t.Fatal("committed certificate missing from durable journal")
	}
	tx, err = x.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = x.A.applyRetentionTombstone(t.Context(), tx, s.state.Tombstones[0]); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_wallet_cleanup WHERE coin='BTC' AND wallet_name=$1 AND due_at=$2`, r.WatchWalletName, r.CleanupDue) != 1 {
		t.Fatal("old restore lost physical cleanup obligation")
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_escrow_retired_addresses WHERE coin='BTC' AND NOT label_checked`) == 0 {
		t.Fatal("restore traffic can bypass wallet label reconciliation")
	}
	var next int64
	if err = x.DB.QueryRow(`SELECT retention_next_escrow_index('market')`).Scan(&next); err != nil || next < r.MarketNextIndex {
		t.Fatalf("restore reused old market index%d: %v", next, err)
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_escrow_key_floors`) == 0 {
		t.Fatal("old restore lost party root allocation floors")
	}
}

func TestRetentionEscrowPreparedCrashDoesNotInventRetirement(t *testing.T) {
	x, order, r, tx := certifiedBTCRetirement(t)
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
	if agInt(x.testEnv, `SELECT count(*) FROM orders WHERE id=$1`, order) != 1 || agInt(x.testEnv, `SELECT count(*) FROM retention_wallet_cleanup`) != 0 || x.core.count("setlabel") != 0 {
		t.Fatal("ambiguous pending journal erased rows or asserted wallet cleanup")
	}
}

func TestRetentionEscrowEmptyRecoveryDoesNotRestartClosure(t *testing.T) {
	x, order, r, tx := certifiedBTCRetirement(t)
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

func TestRetentionWalletCleanupAttestationAllowsDestroyedOriginalWalletOff(t *testing.T) {
	x, _, r, tx := certifiedBTCRetirement(t)
	if err := x.A.applyRetentionEscrowRetirement(t.Context(), tx, r); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	x.A.escrow.Store(nil)
	agExec(x.testEnv, `UPDATE retention_escrow_retired_addresses SET label_checked=false WHERE coin='BTC'`)
	x.core.mu.Lock()
	x.core.watchInfo = nil
	x.core.mu.Unlock() // original wallet physically gone
	t.Setenv("ESCROW_BTC", "off")
	agExec(x.testEnv, `SELECT retention_wallet_cleanup_register('BTC','another-wallet',$1,$2)`, r.CleanupDue, r.CertifiedAt)
	agExec(x.testEnv, `UPDATE retention_wallet_cleanup SET confirmed_at=$1,confirmed_by='verified host operator' WHERE wallet_name='another-wallet'`, r.CertifiedAt.Add(time.Hour))
	if err := x.A.initEscrow(t.Context()); err == nil {
		t.Fatal("unrelated wallet destruction bypassed exact-wallet pending guard")
	}
	agExec(x.testEnv, `UPDATE retention_wallet_cleanup SET confirmed_at=$1,confirmed_by='verified host operator' WHERE coin='BTC' AND wallet_name=$2`, r.CertifiedAt.Add(time.Hour), r.WatchWalletName)
	before := x.core.count("getwalletinfo")
	if err := x.A.initEscrow(t.Context()); err != nil {
		t.Fatalf("exact verified physical destruction required resurrecting original wallet: %v", err)
	}
	if x.core.count("getwalletinfo") != before || agInt(x.testEnv, `SELECT count(*) FROM retention_escrow_retired_addresses WHERE coin='BTC' AND NOT label_checked`) != 0 {
		t.Fatal("BTC-off proof accessed destroyed original or left startup pending")
	}
}

func TestRetentionWalletCleanupReplacementDoesNotAccessDestroyedOriginal(t *testing.T) {
	x, _, r, tx := certifiedBTCRetirement(t)
	if err := x.A.applyRetentionEscrowRetirement(t.Context(), tx, r); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	x.A.escrow.Store(nil)
	agExec(x.testEnv, `UPDATE retention_escrow_retired_addresses SET label_checked=false WHERE coin='BTC'`)
	core, _ := newFakeEscrowCore(t)
	const replacement = "/wallet/replacement-watch"
	var oldAccess atomic.Bool
	srv := newPayRPCServer(t, "escrowapp", "escrow-rpc-pass", false, func(path, method string, p json.RawMessage) (any, int, string) {
		if path == fakeWatchWallet {
			oldAccess.Store(true)
			return nil, -18, "original wallet destroyed"
		}
		if path == replacement {
			path = fakeWatchWallet
		}
		return core.handle(path, method, p)
	})
	x.A.escrowCfg.rpcURL = srv.url("")
	x.A.escrowCfg.watchWallet = "replacement-watch"
	state, _, err := x.A.startEscrow(t.Context())
	if err != nil || state != escrowLogOn || oldAccess.Load() || core.count("setlabel") != 0 {
		t.Fatalf("replacement startup reopened original wallet: %q %v old=%v", state, err, oldAccess.Load())
	}
	if agInt(x.testEnv, `SELECT count(*) FROM retention_escrow_retired_addresses WHERE coin='BTC' AND NOT label_checked`) != 0 {
		t.Fatal("checked replacement failed to resolve replay label-pending")
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
	x, _, r, tx := certifiedBTCRetirement(t)
	needed, err := retentionAccountObligations(t.Context(), tx, x.buyer, r.CertifiedAt)
	if err != nil || needed {
		t.Fatalf("verified closed historical record required live account: %v %v", needed, err)
	}
	tx.Rollback()
	e := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	runRetentionClosure(t, x.testEnv, r.CertifiedAt)
	tx, err = x.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err = retentionAccountObligations(t.Context(), tx, x.buyer, r.CertifiedAt)
	tx.Rollback()
	if err != nil || !needed {
		t.Fatalf("proof outage erased access needed for unseen late funds: %v %v", needed, err)
	}
	x.A.escrow.Store(e)
	runRetentionClosure(t, x.testEnv, r.CertifiedAt.Add(31*time.Minute))
	tx, err = x.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	needed, err = retentionAccountObligations(t.Context(), tx, x.buyer, r.CertifiedAt.Add(31*time.Minute))
	if err != nil || needed {
		t.Fatalf("restored proof turned temporary outage into indefinite credential retention: %v %v", needed, err)
	}
}
