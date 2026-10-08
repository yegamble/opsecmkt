package market

import (
	"context"
	"errors"
	"strings"
	"sync"
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

func TestProviderEpisodePooledWatcherThresholdRecoveryAndIntakePause(t *testing.T) {
	p := newPayEnv(t)
	ctx := context.Background()
	admin, _ := p.user("episode_admin_"+randomToken()[:6], "admin")
	suspended, _ := p.user("episode_suspended_"+randomToken()[:6], "admin")
	agExec(p.testEnv, `UPDATE users SET suspended_at=now() WHERE id=$1`, suspended)
	// Pausing new intake is operator policy, not a provider outage. Existing deposits are still polled.
	agExec(p.testEnv, `UPDATE settings SET value='false' WHERE key='payment_intake_BTC'`)
	p.poll()
	if n, bad, _ := providerEpisodeState(t, p.testEnv, "pooled_BTC"); n != 0 || bad {
		t.Fatalf("paused intake produced an outage: episode=%d bad=%v", n, bad)
	}
	p.fake.SetWallet(nil, errors.New("wallet secret address should not appear"), false)
	if err := p.A.pollOnce(ctx); err == nil {
		t.Fatal("failed wallet check unexpectedly passed")
	}
	if n, bad, alert := providerEpisodeState(t, p.testEnv, "pooled_BTC"); n != 1 || !bad || alert {
		t.Fatalf("first bad pass: episode=%d bad=%v alerted=%v", n, bad, alert)
	}
	if got := providerNoticeCount(t, p.testEnv, admin, "pooled payment provider"); got != 0 {
		t.Fatalf("early outage notice: %d", got)
	}
	agExec(p.testEnv, `UPDATE provider_episodes SET bad_since=now()-interval '10 minutes 1 second' WHERE provider='pooled_BTC'`)
	for range 3 {
		_ = p.A.pollOnce(ctx)
	}
	if got := providerNoticeCount(t, p.testEnv, admin, "has been unavailable"); got != 1 {
		t.Fatalf("repeated passes delivered %d outage notices", got)
	}
	if got := providerNoticeCount(t, p.testEnv, suspended, "has been unavailable"); got != 0 {
		t.Fatalf("suspended admin received %d notices", got)
	}
	newAdmin, _ := p.user("episode_new_"+randomToken()[:6], "admin")
	_ = p.A.pollOnce(ctx)
	if got := providerNoticeCount(t, p.testEnv, newAdmin, "has been unavailable"); got != 1 {
		t.Fatalf("new current admin missed ongoing episode: %d", got)
	}
	p.fake.SetWallet(nil, nil, false)
	p.poll()
	p.poll()
	for _, uid := range []string{admin, newAdmin} {
		if got := providerNoticeCount(t, p.testEnv, uid, "is available again"); got != 1 {
			t.Fatalf("recovery notice for %s: %d", uid, got)
		}
	}
	if got := providerNoticeCount(t, p.testEnv, suspended, "is available again"); got != 0 {
		t.Fatalf("suspended admin recovery notice: %d", got)
	}
	if n, bad, alert := providerEpisodeState(t, p.testEnv, "pooled_BTC"); n != 1 || bad || alert {
		t.Fatalf("recovery state: episode=%d bad=%v alerted=%v", n, bad, alert)
	}
	for _, body := range p.notes(admin) {
		if strings.Contains(body, "secret address") {
			t.Fatalf("wallet error leaked to staff notice: %q", body)
		}
	}
	// A tip high-water skip is computed after refreshProviders; it must also start an outage.
	p.fake.SetHeight(10)
	agExec(p.testEnv, `UPDATE payment_status SET tip_height=20 WHERE currency='BTC'`)
	p.poll()
	if n, bad, _ := providerEpisodeState(t, p.testEnv, "pooled_BTC"); n != 2 || !bad {
		t.Fatalf("behind-tip skip: episode=%d bad=%v", n, bad)
	}
}

func TestProviderEpisodeConfiguredRefusalAlertsButUnconfiguredDoesNot(t *testing.T) {
	p := newPayEnv(t)
	admin, _ := p.user("episode_refused_"+randomToken()[:6], "admin")
	p.fake.SetWallet(nil, refusef("wrong network with a private value"), false)
	_ = p.A.pollOnce(context.Background())
	if _, bad, _ := providerEpisodeState(t, p.testEnv, "pooled_BTC"); !bad {
		t.Fatal("configured provider's network refusal was not an outage")
	}
	agExec(p.testEnv, `UPDATE provider_episodes SET bad_since=now()-interval '11 minutes' WHERE provider='pooled_BTC'`)
	_ = p.A.pollOnce(context.Background())
	if got := providerNoticeCount(t, p.testEnv, admin, "has been unavailable"); got != 1 {
		t.Fatalf("configured refusal notice = %d", got)
	}
	if got := agInt(p.testEnv, `SELECT count(*) FROM provider_episodes WHERE provider='pooled_XMR'`); got != 0 {
		t.Fatalf("unconfigured Monero provider produced %d episode rows", got)
	}
	for _, body := range p.notes(admin) {
		if strings.Contains(body, "private value") {
			t.Fatalf("refusal detail leaked into notice: %q", body)
		}
	}
}

func TestProviderEpisodeShortBlipAndConcurrentRestartDedup(t *testing.T) {
	p := newPayEnv(t)
	ctx := context.Background()
	admin, _ := p.user("episode_concurrent_"+randomToken()[:6], "admin")
	for _, bad := range []bool{true, false} {
		if err := p.A.observeProviderEpisode(ctx, "escrow_XMR", bad); err != nil {
			t.Fatal(err)
		}
	}
	if got := providerNoticeCount(t, p.testEnv, admin, "escrow payment provider"); got != 0 {
		t.Fatalf("short blip delivered %d notices", got)
	}
	if err := p.A.observeProviderEpisode(ctx, "escrow_XMR", true); err != nil {
		t.Fatal(err)
	}
	agExec(p.testEnv, `UPDATE provider_episodes SET bad_since=now()-interval '11 minutes' WHERE provider='escrow_XMR'`)
	// Two app instances on the same database simulate a restart/competing watcher. The row lock and
	// per-recipient receipt must make their simultaneous attempts one durable notification.
	other := &App{db: p.DB}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := other.observeProviderEpisode(ctx, "escrow_XMR", true); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := providerNoticeCount(t, p.testEnv, admin, "has been unavailable"); got != 1 {
		t.Fatalf("concurrent outage delivered %d notices", got)
	}
	if err := other.observeProviderEpisode(ctx, "escrow_XMR", false); err != nil {
		t.Fatal(err)
	}
	if err := p.A.observeProviderEpisode(ctx, "escrow_XMR", false); err != nil {
		t.Fatal(err)
	}
	if got := providerNoticeCount(t, p.testEnv, admin, "is available again"); got != 1 {
		t.Fatalf("recovery delivered %d notices", got)
	}
	if err := p.A.observeProviderEpisode(ctx, "escrow_XMR", true); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := providerEpisodeState(t, p.testEnv, "escrow_XMR"); n != 3 {
		t.Fatalf("new episode number = %d, want 3", n)
	}
	if got := agInt(p.testEnv, `SELECT count(*) FROM provider_episode_deliveries WHERE provider='escrow_XMR'`); got != 0 {
		t.Fatalf("stale receipts retained: %d", got)
	}
}

// These are actual watcher paths against the repository's simulated RPCs, not direct helper calls.
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
