package market

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A-449 (P4-R15): the real-chain legs of Monero escrow on stagenet that an offline stagenet fork can run
// (scripts/test-escrow-xmr-stagenet.sh; manual, not run in CI). The fork is built from stagenet's genesis block with the
// local monerod 0.18.5.1 image only: two daemons peered only with each other on a Docker network with no route out
// (--add-exclusive-node), fixed difficulty, local mining, plus one --offline daemon and a stagenet monero-wallet-rpc. It
// is "offline stagenet, not public": every leg refuses a daemon that is not stagenet or is at height 10,000 or more (the
// public stagenet is past 1,900,000). Such a chain is at hard fork version 1 (version 2 starts at height 32,000 and
// 16 at 1,151,720), so Monero escrow's network check refuses it, as it must: the fakechain legs
// (scripts/test-escrow-xmr-regtest.sh), which need escrow to start, cannot run on it. What can run here:
//
//   - OldForkRefused: an App with ESCROW_XMR=on against the stagenet daemon is refused for its hard fork (errXMRHardFork),
//     not for its network: the nettype check passes stagenet.
//   - Addresses: a real stagenet wallet's primary address, a create_account subaddress and an integrated address parse
//     under the stagenet tags (the integrated one refused as such) and are refused under the fakechain's; the adapter,
//     verified for stagenet, reads the real wallet's create_account and primary address.
//   - SyncGuard: the escrow adapter's sync read (get_info and get_last_block_header): the pair's unrestricted daemon
//     shows its peer; its restricted one (as the bundled monero service serves) hides the counts (0) and is synced on
//     offline false and a fresh tip; the --offline daemon has no peers; the same reading judged 21 minutes after its
//     tip, under MONERO_MAX_TIP_AGE's minimum of 20 minutes, is stale.

// xmrStagenet is one leg's view of the offline stagenet fork.
type xmrStagenet struct {
	// URLs: the peered pair's unrestricted daemon (it mines; the wallet-rpc uses it) and restricted one (the app's,
	// as the bundled monero service serves), the --offline daemon, the wallet-rpc
	miner, peered, offline, wallet string
}

func newXMRStagenet(t *testing.T) *xmrStagenet {
	t.Helper()
	s := &xmrStagenet{miner: os.Getenv("ESCROW_XMR_STAGENET_MINER_URL"), peered: os.Getenv("ESCROW_XMR_STAGENET_DAEMON_URL"),
		offline: os.Getenv("ESCROW_XMR_STAGENET_OFFLINE_URL"), wallet: os.Getenv("ESCROW_XMR_STAGENET_WALLET_URL")}
	if s.miner == "" || s.peered == "" || s.offline == "" || s.wallet == "" {
		t.Skip("set ESCROW_XMR_STAGENET_MINER_URL, _DAEMON_URL, _OFFLINE_URL and _WALLET_URL (scripts/test-escrow-xmr-stagenet.sh) to run against an offline stagenet fork")
	}
	for _, at := range []string{s.miner, s.peered, s.offline} {
		d, err := newRPCClient("stagenet daemon", at, "2.0", true)
		if err != nil {
			t.Fatal(err)
		}
		var info struct {
			NetType string `json:"nettype"`
			Height  int64  `json:"height"`
		}
		if err = d.call(context.Background(), "/json_rpc", "get_info", nil, &info); err != nil || info.NetType != "stagenet" || info.Height >= 10_000 {
			t.Fatalf("refusing: a daemon is not an offline stagenet fork (nettype %q, height %d, %v)", info.NetType, info.Height, err)
		}
	}
	return s
}

// rpc is the escrow adapter against the wallet-rpc and daemon, verified for nettype.
func (s *xmrStagenet) rpc(t *testing.T, daemon, nettype string) *xmrEscrowRPC {
	t.Helper()
	r, err := newXMREscrowRPC(s.wallet, daemon)
	if err != nil {
		t.Fatal(err)
	}
	r.net.Store(nettype)
	return r
}

func TestXMREscrowStagenetOldForkRefused(t *testing.T) {
	s := newXMRStagenet(t)
	fake := newFakeXMREscrow(t) // only for its environment; the URLs below are the real nodes'
	_, pub := testSeedKey(t, nil)
	fake.setEnv(t, writeSeedKey(t, pub))
	t.Setenv("ESCROW_XMR", "")
	newTestApp(t) // a migrated database for New, started with Monero escrow off
	t.Setenv("ESCROW_XMR", "on")
	t.Setenv("ESCROW_XMR_WALLET_RPC_URL", s.wallet)
	t.Setenv("ESCROW_XMR_DAEMON_URL", s.peered)
	a, err := New(context.Background(), false)
	if a != nil {
		a.Close()
	}
	if !errors.Is(err, errXMRStartDaemon) || !strings.Contains(err.Error(), errXMRHardFork.Error()) || strings.Contains(err.Error(), errXMRDaemonNetwork.Error()) {
		t.Fatalf("offline stagenet fork at hard fork 1: %v", err)
	}
	t.Logf("refused as it must be: %v", err)
}

func TestXMREscrowStagenetAddresses(t *testing.T) {
	s := newXMRStagenet(t)
	ctx := context.Background()
	r := s.rpc(t, s.peered, "stagenet")
	name := "escrow-" + time.Now().UTC().Format("200601") + "-" + time.Now().UTC().Format("05")
	if err := r.createWallet(ctx, name, "stagenet leg"); err != nil {
		var re *xmrRPCError
		if !errors.As(err, &re) || re.Code != -21 {
			t.Fatal(err)
		}
		if err = r.openWallet(ctx, name, "stagenet leg"); err != nil {
			t.Fatal(err)
		}
	}
	primary, err := r.primaryAddress(ctx)
	if err != nil {
		t.Fatalf("the adapter refused the real stagenet wallet's primary address: %v", err)
	}
	idx, sub, err := r.createAccount(ctx)
	if err != nil {
		t.Fatalf("the adapter refused the real stagenet wallet's create_account: %v", err)
	}
	var integ struct {
		Address string `json:"integrated_address"`
	}
	if err = r.walletCall(ctx, "make_integrated_address", nil, &integ); err != nil {
		t.Fatal(err)
	}
	t.Logf("real stagenet wallet: primary %s… (standard), account %d %s… (subaddress), integrated %s…", primary[:8], idx, sub[:8], integ.Address[:8])
	for _, c := range []struct {
		addr, kind string
		err        error
	}{{primary, xmrStandard, nil}, {sub, xmrSubaddress, nil}} {
		if a, err := parseXMRAddress(c.addr, "stagenet"); err != nil || a.Kind != c.kind || (c.addr[0] != '5' && c.addr[0] != '7') {
			t.Fatalf("%s… under stagenet: %q %v", c.addr[:8], a.Kind, err)
		}
		if _, err := parseXMRAddress(c.addr, "fakechain"); !errors.Is(err, errXMRAddressNetwork) {
			t.Fatalf("%s… under the fakechain: %v", c.addr[:8], err)
		}
	}
	if err := checkXMRFormAddress(integ.Address, "stagenet"); !errors.Is(err, errXMRAddressIntegrated) {
		t.Fatalf("real stagenet integrated address: %v", err)
	}
	// The same adapter set for the fakechain refuses the real stagenet wallet's replies.
	if _, _, err := s.rpc(t, s.peered, "fakechain").createAccount(ctx); err == nil {
		t.Fatal("a fakechain-verified adapter accepted a stagenet create_account reply")
	}
	if err = r.closeWallet(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestXMREscrowStagenetSyncGuard(t *testing.T) {
	s := newXMRStagenet(t)
	ctx := context.Background()
	// The unrestricted miner of the pair shows its peer; the restricted one hides the counts (0), as monerod does.
	m, err := s.rpc(t, s.miner, "stagenet").node(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("peered unrestricted daemon: restricted %v, offline %v, connections %d", m.Restricted, m.Offline, m.Connections)
	if m.Restricted || m.Connections < 1 || m.notSynced(time.Now(), 20*time.Minute) != "" {
		t.Fatalf("the unrestricted peered daemon: %+v", m)
	}
	n, err := s.rpc(t, s.peered, "stagenet").node(ctx)
	if err != nil {
		t.Fatalf("sync read on the restricted daemon: %v", err)
	}
	age := time.Since(time.Unix(n.TipTime, 0)).Round(time.Second)
	t.Logf("peered restricted daemon: restricted %v, offline %v, synchronized %v, height %d, target %d, connections %d (hidden), tip age %s",
		n.Restricted, n.Offline, n.Synchronized, n.Height, n.TargetHeight, n.Connections, age)
	if why := n.notSynced(time.Now(), 20*time.Minute); why != "" || !n.Restricted || n.Offline {
		t.Fatalf("the restricted peered daemon with a fresh tip counts as not synced: %q (%+v)", why, n)
	}
	// The same reading judged as if the tip were older than the bound: 20 minutes past it.
	if why := n.notSynced(time.Unix(n.TipTime, 0).Add(21*time.Minute), 20*time.Minute); why != xmrNotSyncedTip {
		t.Fatalf("a tip 21 minutes old under a 20-minute bound: %q", why)
	}
	off, err := s.rpc(t, s.offline, "stagenet").node(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("--offline daemon: synchronized %v, height %d, connections %d", off.Synchronized, off.Height, off.Connections)
	if why := off.notSynced(time.Now(), time.Hour); why != notSyncedPeers {
		t.Fatalf("the --offline daemon: %q, want %q", why, notSyncedPeers)
	}
}
