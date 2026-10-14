package market

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func retentionXMRDestroyFixture(t *testing.T) (*xmrEscrowEnv, string, string) {
	t.Helper()
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	name := x.walletOf(order)
	x.rotate()
	x.days(name, 91)
	if err := x.A.initRetention(t.Context(), retentionConfig{Enabled: true, StateDir: retentionPrivateTestDir(t)}); err != nil {
		t.Fatal(err)
	}
	oldRow := agStr(x.testEnv, `SELECT row_to_json(w)::text FROM xmr_escrow_wallets w WHERE name=$1`, name)
	return x, name, oldRow
}

func TestRetentionXMRDestructionNinetyDayJournalAndOlderSeedReplay(t *testing.T) {
	x, name, oldRow := retentionXMRDestroyFixture(t)
	prepared := make(chan bool, 1)
	x.fake.hook("change_wallet_password", func() {
		s := x.A.retention
		unlock, err := s.lock()
		if err != nil {
			prepared <- false
			return
		}
		defer unlock()
		if err = s.read(); err != nil {
			prepared <- false
			return
		}
		valid := len(s.state.Pending) == 1 && len(s.state.Pending[0].Tombstones) == 1 && s.state.Pending[0].Tombstones[0].Kind == "xmr_wallet_destroy"
		if valid {
			valid = agInt(x.testEnv, `SELECT count(*) FROM retention_operations WHERE id=$1`, s.state.Pending[0].ID) == 0
		}
		prepared <- valid
	})
	x.mustPoll()
	if !<-prepared {
		t.Fatal("irreversible password RPC preceded durable intent or committed marker too early")
	}
	s := x.A.retention
	if len(s.state.Pending) != 0 || len(s.state.Tombstones) != 1 {
		t.Fatal("guarded90day destruction did not finish external certificate")
	}
	tombstone := s.state.Tombstones[0]
	if tombstone.Kind != "xmr_wallet_destroy" || tombstone.CommittedAt == nil || tombstone.XMRDestruction.WalletName != name {
		t.Fatal("exact destruction proof missing")
	}
	// Materialize a pre-destruction backup row in a separate disposable database.
	// No preservation/immutability trigger is disabled to fabricate old state.
	e := newTestApp(t)
	agExec(e, `INSERT INTO xmr_escrow_wallets SELECT (json_populate_record(NULL::xmr_escrow_wallets,$1::json)).*`, oldRow)
	agExec(e, `SELECT setval('xmr_escrow_wallets_id_seq',100,true)`)
	other := xmrWallet(t, e, 2, "active")
	buyer, _ := e.user("seed_restore_buyer", "buyer")
	vendor, _ := e.user("seed_restore_vendor", "vendor")
	order, addr, _ := xmrEscrow(t, e, other, 1, buyer, e.product(vendor, "digital"), stateAwaitingPayment)
	agExec(e, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'XMR',$2,$3,$4,100000,12)`, order, txidOf("seed restore unrelated funds"), xmrEscrowIdx(other, 1), addr)
	badCertificate := *tombstone.XMRDestruction
	badCertificate.WalletAddressHash = retentionTechnicalHash(xmrAddr("another wallet cryptographic identity"))
	wrong, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.A.applyRetentionXMRDestruction(t.Context(), wrong, &badCertificate); err == nil {
		t.Fatal("same wallet name with another cryptographic identity erased a restored seed")
	}
	wrong.Rollback()
	if agInt(e, `SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND seed_sealed IS NOT NULL`, name) != 1 {
		t.Fatal("rejected wallet identity proof changed seed")
	}
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = e.A.applyRetentionTombstone(t.Context(), tx, tombstone); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND state='destroyed' AND seed_sealed IS NULL AND destroy_started_at IS NOT NULL`, name) != 1 {
		t.Fatal("backup resurrected erased recovery seed or lost password-destruction evidence")
	}
	if agInt(e, `SELECT count(*) FROM payments WHERE order_id=$1`, order) != 1 || agInt(e, `SELECT count(*) FROM xmr_escrow_wallets WHERE id=$1 AND seed_sealed IS NOT NULL`, other) != 1 {
		t.Fatal("seed-only certificate erased unrelated unresolved financial state")
	}
	if agInt(e, `SELECT count(*) FROM retention_xmr_destroy_permits`) != 0 {
		t.Fatal("exact replay authority survived commit")
	}
	// A backup before wallet creation still receives permanent name non-reuse
	// state and physical cleanup obligations, even without any financial row.
	before := newTestApp(t)
	tx, err = before.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = before.A.applyRetentionTombstone(t.Context(), tx, tombstone); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cert := tombstone.XMRDestruction
	if agInt(before, `SELECT count(*) FROM retention_xmr_wallet_names WHERE month=$1::date AND next_n>=$2`, cert.WalletMonth, cert.WalletNextN) != 1 || agInt(before, `SELECT count(*) FROM retention_xmr_retired_wallets WHERE name_hash=$1`, retentionTechnicalHash(name)) != 1 {
		t.Fatal("pre-wallet backup can reallocate destroyed wallet name")
	}
	if agInt(before, `SELECT count(*) FROM retention_wallet_cleanup WHERE coin='XMR' AND wallet_name=$1 AND due_at=$2`, name, cert.DestroyAfter) != 1 {
		t.Fatal("pre-wallet backup lost original physical cleanup obligation")
	}
	if _, err = before.DB.Exec(`INSERT INTO xmr_escrow_wallets(name,month) VALUES($1,$2::date)`, name, cert.WalletMonth); err == nil {
		t.Fatal("database allowed destroyed wallet name reuse")
	}
}

func TestRetentionXMRDestructionAmbiguousRPCPreservesPendingProof(t *testing.T) {
	x, name, _ := retentionXMRDestroyFixture(t)
	x.fake.fault("change_wallet_password", fakeXMRDrop)
	if err := x.poll(); err == nil {
		t.Fatal("lost irreversible RPC response was treated as complete")
	}
	s := x.A.retention
	if len(s.state.Pending) != 1 || len(s.state.Tombstones) != 0 {
		t.Fatal("ambiguous password change lost or invented committed proof")
	}
	if agInt(x.testEnv, `SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND seed_sealed IS NOT NULL`, name) != 1 {
		t.Fatal("ambiguous operation erased uncommitted database recovery seed")
	}
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = s.read(); err != nil {
		t.Fatal(err)
	}
	if err = x.A.resolveRetentionPending(context.Background()); err == nil {
		t.Fatal("older database with absent commit marker bypassed pending destruction gate")
	}
}

func TestRetentionXMRDestructionSnapshotCapturesExistingWalletFacts(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _ := x.settled()
	name := x.walletOf(order)
	x.rotate()
	x.days(name, 91)
	x.mustPoll() // legacy/off enrollment destruction
	if err := x.A.initRetention(t.Context(), retentionConfig{Enabled: true, StateDir: retentionPrivateTestDir(t)}); err != nil {
		t.Fatal(err)
	}
	if len(x.A.retention.state.Tombstones) != 1 || x.A.retention.state.Tombstones[0].Kind != "xmr_wallet_destroy" || x.A.retention.state.Tombstones[0].XMRDestruction.WalletName != name {
		t.Fatal("initial policy enrollment omitted existing90day seed-erasure facts")
	}
	if x.A.retention.state.Tombstones[0].CommittedAt.Before(time.Now().UTC().Add(-time.Minute)) {
		t.Fatal("initial observation gave historical erasure no restore coverage")
	}
}

func TestRetentionXMRDestructionConfirmedMissingWalletIsJournaled(t *testing.T) {
	x, name, _ := retentionXMRDestroyFixture(t)
	x.fake.edit(func(f *fakeXMREscrow) { delete(f.files, name) })
	x.mustPoll()
	if x.walletState(name) != "missing" {
		t.Fatal("fixture wallet not missing")
	}
	_, adminSession := x.user("seed_confirm_admin_"+randomToken()[:6], "admin")
	id := strconv.Itoa(agInt(x.testEnv, `SELECT id FROM xmr_escrow_wallets WHERE name=$1`, name))
	form := url.Values{"wallet_id": {id}, "gone": {"confirmed"}, "password": {testPassword}}
	if response := x.do("POST", "/admin/escrow-xmr-destroyed", adminSession, form); response.Code != 303 {
		t.Fatalf("confirmed missing wallet destruction: %d", response.Code)
	}
	if len(x.A.retention.state.Pending) != 0 || len(x.A.retention.state.Tombstones) != 1 || x.A.retention.state.Tombstones[0].Kind != "xmr_wallet_destroy" || x.A.retention.state.Tombstones[0].CommittedAt == nil {
		t.Fatal("confirmed HTTP seed erasure lacked committed exact-wallet proof")
	}
	if agInt(x.testEnv, `SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND state='destroyed' AND seed_sealed IS NULL`, name) != 1 {
		t.Fatal("successful confirmation retained recoverable seed")
	}
}
