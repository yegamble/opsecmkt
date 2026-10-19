package market

import (
	"testing"
)

func providerEpisodeState(t *testing.T, e *testEnv, provider string) (int64, bool, bool) {
	t.Helper()
	var n int64
	var bad, alerted bool
	if err := e.DB.QueryRow(`SELECT episode,bad_since IS NOT NULL,alerted FROM provider_episodes WHERE provider=$1`, provider).Scan(&n, &bad, &alerted); err != nil {
		t.Fatal(err)
	}
	return n, bad, alerted
}

func providerNoticeCount(t *testing.T, e *testEnv, uid, phrase string) int {
	t.Helper()
	return agInt(e, `SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%'||$2||'%'`, uid, phrase)
}

func TestXMREscrowProviderEpisodeTracksSyncSkipAndRecovery(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.fake.edit(func(f *fakeXMREscrow) { f.unsynced = true })
	x.mustPoll() // a syncing node skips coin reads without returning an error
	if _, bad, _ := providerEpisodeState(t, x.testEnv, "escrow_XMR"); !bad {
		t.Fatal("sync skip did not start Monero escrow outage")
	}
	agExec(x.testEnv, `UPDATE provider_episodes SET bad_since=now()-interval '11 minutes' WHERE provider='escrow_XMR'`)
	x.mustPoll()
	if got := providerNoticeCount(t, x.testEnv, x.admin, "has been unavailable"); got != 1 {
		t.Fatalf("Monero escrow outage notices = %d", got)
	}
	// A-553 (STAFF66-3): the notice points to the persisted watcher health on Admin → Payments, not to server logs.
	if providerNoticeCount(t, x.testEnv, x.admin, "Admin → Payments (/admin/payments#xmr-escrow-health)") != 1 || providerNoticeCount(t, x.testEnv, x.admin, "watcher logs") != 0 {
		t.Fatal("the outage notice does not point to the Payments health panel")
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.unsynced = false })
	x.mustPoll()
	if got := providerNoticeCount(t, x.testEnv, x.admin, "is available again"); got != 1 {
		t.Fatalf("Monero escrow recovery notices = %d", got)
	}
}

func TestXMREscrowLockedWalletIsUnreadEpisode(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll() // creates the current wallet
	const name = "escrow-202610-01"
	x.fake.edit(func(f *fakeXMREscrow) { f.files[name].locked = true })
	x.mustPoll() // the existing watcher intentionally returns nil while this wallet is unread
	if _, bad, _ := providerEpisodeState(t, x.testEnv, "escrow_XMR"); !bad {
		t.Fatal("unread locked wallet was mistaken for a healthy provider")
	}
	agExec(x.testEnv, `UPDATE provider_episodes SET bad_since=now()-interval '11 minutes' WHERE provider='escrow_XMR'`)
	x.mustPoll()
	if got := providerNoticeCount(t, x.testEnv, x.admin, "has been unavailable"); got != 1 {
		t.Fatalf("locked-wallet outage notices = %d", got)
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.files[name].locked = false })
	x.mustPoll()
	if got := providerNoticeCount(t, x.testEnv, x.admin, "is available again"); got != 1 {
		t.Fatalf("locked-wallet recovery notices = %d", got)
	}
}
