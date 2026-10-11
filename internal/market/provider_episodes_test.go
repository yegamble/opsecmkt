package market

import (
	"context"
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

func TestEscrowProviderEpisodeTracksNodeOutageAndRecovery(t *testing.T) {
	x := newEscrowEnv(t)
	admin, _ := x.user("episode_btc_"+randomToken()[:6], "admin")
	x.poll()
	srvDown(x, true)
	if err := x.A.escrowPollOnce(context.Background()); err == nil {
		t.Fatal("down Bitcoin escrow node passed")
	}
	if _, bad, _ := providerEpisodeState(t, x.testEnv, "escrow_BTC"); !bad {
		t.Fatal("down node did not start escrow outage")
	}
	agExec(x.testEnv, `UPDATE provider_episodes SET bad_since=now()-interval '11 minutes' WHERE provider='escrow_BTC'`)
	_ = x.A.escrowPollOnce(context.Background())
	if got := providerNoticeCount(t, x.testEnv, admin, "has been unavailable"); got != 1 {
		t.Fatalf("Bitcoin escrow outage notices = %d", got)
	}
	srvDown(x, false)
	x.poll()
	if got := providerNoticeCount(t, x.testEnv, admin, "is available again"); got != 1 {
		t.Fatalf("Bitcoin escrow recovery notices = %d", got)
	}
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

// A-143: an outage alerts only after providerOutageDelay, once per administrator per episode (a repeated pass, a direct
// repeat and a restarted process add nothing), never to a suspended administrator or a non-administrator, and a new
// episode after recovery alerts again and discards the old receipts.
func TestEscrowProviderEpisodeDelayDedupeAndRecipients(t *testing.T) {
	x := newEscrowEnv(t)
	admin, _ := x.user("episode_dly_"+randomToken()[:6], "admin")
	suspended, _ := x.user("episode_sus_"+randomToken()[:6], "admin")
	agExec(x.testEnv, "UPDATE users SET suspended_at=now() WHERE id=$1", suspended)
	mod, _ := x.user("episode_mod_"+randomToken()[:6], "moderator")
	const outage, recovery = "has been unavailable", "is available again"
	notices := func(phrase string) (int, int, int) {
		return providerNoticeCount(t, x.testEnv, admin, phrase), providerNoticeCount(t, x.testEnv, suspended, phrase), providerNoticeCount(t, x.testEnv, mod, phrase)
	}
	receipts := func() int {
		return agInt(x.testEnv, `SELECT count(*) FROM provider_episode_deliveries WHERE provider='escrow_BTC'`)
	}
	x.poll()
	srvDown(x, true)
	for range 3 {
		_ = x.A.escrowPollOnce(context.Background())
	}
	if a, s, m := notices(outage); a+s+m != 0 || receipts() != 0 {
		t.Fatalf("outage alerted before the delay: admin=%d suspended=%d moderator=%d receipts=%d", a, s, m, receipts())
	}
	// Just under the delay still waits; only an episode at least providerOutageDelay old alerts.
	agExec(x.testEnv, `UPDATE provider_episodes SET bad_since=now()-interval '9 minutes 30 seconds' WHERE provider='escrow_BTC'`)
	for range 2 {
		_ = x.A.escrowPollOnce(context.Background())
	}
	if a, s, m := notices(outage); a+s+m != 0 {
		t.Fatalf("an episode younger than providerOutageDelay alerted: admin=%d suspended=%d moderator=%d", a, s, m)
	}
	if _, bad, alerted := providerEpisodeState(t, x.testEnv, "escrow_BTC"); !bad || alerted {
		t.Fatalf("young episode state: bad=%v alerted=%v", bad, alerted)
	}

	agExec(x.testEnv, `UPDATE provider_episodes SET bad_since=now()-interval '11 minutes' WHERE provider='escrow_BTC'`)
	for range 3 {
		_ = x.A.escrowPollOnce(context.Background())
	}
	// A direct repeat of the same observation is a no-op for an administrator who already holds the receipt.
	for range 2 {
		if err := x.A.observeProviderEpisode(context.Background(), "escrow_BTC", true); err != nil {
			t.Fatalf("repeated observation of an alerted episode: %v", err)
		}
	}
	if a, s, m := notices(outage); a != 1 || s != 0 || m != 0 || receipts() != 1 {
		t.Fatalf("outage notices admin=%d suspended=%d moderator=%d receipts=%d, want 1 0 0 and one receipt", a, s, m, receipts())
	}
	// A restarted process, with no memory of the first, does not deliver again.
	x.restart() // the node is down, so escrow starts disabled and its watcher retries
	if err := x.A.observeProviderEpisode(context.Background(), "escrow_BTC", true); err != nil {
		t.Fatalf("observation after a restart: %v", err)
	}
	if a, _, _ := notices(outage); a != 1 || receipts() != 1 {
		t.Fatalf("a restarted watcher repeated the outage notice: admin=%d receipts=%d", a, receipts())
	}

	srvDown(x, false)
	x.A.retryEscrow(context.Background(), &escrowRetry{last: escrowLogUnchecked})
	if x.A.escrow.Load() == nil {
		t.Fatal("escrow did not come back once the node did")
	}
	x.poll()
	if a, s, m := notices(recovery); a != 1 || s != 0 || m != 0 {
		t.Fatalf("recovery notices admin=%d suspended=%d moderator=%d, want 1 0 0", a, s, m)
	}
	// A second episode alerts again and drops the finished episode's receipts.
	srvDown(x, true)
	_ = x.A.escrowPollOnce(context.Background())
	if n, bad, _ := providerEpisodeState(t, x.testEnv, "escrow_BTC"); n != 2 || !bad {
		t.Fatalf("second outage episode = %d bad=%v, want episode 2", n, bad)
	}
	if n := agInt(x.testEnv, `SELECT count(*) FROM provider_episode_deliveries WHERE provider='escrow_BTC' AND episode<2`); n != 0 {
		t.Fatalf("%d receipts of the finished episode survive", n)
	}
	agExec(x.testEnv, `UPDATE provider_episodes SET bad_since=now()-interval '11 minutes' WHERE provider='escrow_BTC'`)
	_ = x.A.escrowPollOnce(context.Background())
	if a, s, _ := notices(outage); a != 2 || s != 0 {
		t.Fatalf("second episode outage notices admin=%d suspended=%d, want 2 0", a, s)
	}
}
