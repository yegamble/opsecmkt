package market

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestXMREscrowPAYEE2 is PAYE-E2 (WR-escrow Phase 3, P3-R12): how one monthly Monero escrow wallet scales, measured on
// a real monerod and monero-wallet-rpc 0.18.5.1 fakechain (scripts/test-escrow-xmr-payee2.sh; manual, not run in CI).
// One wallet gets ESCROW_XMR_PAYEE2_ACCOUNTS accounts (default 10,000) by create_account, then
// ESCROW_XMR_PAYEE2_FUNDED of them (default 200, chosen at random) are funded by the funder wallet, two per block over
// half as many blocks, while the escrow wallet is closed. It measures, through the app's own adapter (so its timeouts
// apply: rpcTimeout for create_account, get_address, get_transfers, get_accounts and incoming_transfers;
// xmrWalletSlowTimeout for create_wallet, open_wallet and store): create_account in total and per call; open_wallet;
// refresh over the funding blocks, over 1,000 empty blocks and over one block with 2 deposits (each measured to its end
// and recorded as a FINDING when it takes longer than xmrWalletSlowTimeout, the bound the watcher gives it);
// get_transfers all_accounts (time and payload size); get_accounts; incoming_transfers of one funded account; the wallet
// file's size and the wallet-rpc's resident memory (when ESCROW_XMR_PAYEE2_WALLET_CONTAINER names its container); and a
// seed restore on the restore wallet-rpc (restore_deterministic_wallet, set_subaddress_lookahead past the highest
// account, rescan_blockchain, also a FINDING beyond xmrWalletSlowTimeout), which must find every funded account. Then a
// full watcher pass (xmrWalletPass's calls: open_wallet, the network check with its
// refresh, get_address of account 0, get_transfers, get_accounts, store) over ESCROW_XMR_PAYEE2_WALLETS such wallets
// (default 5: the container's copies of the measured wallet file, so each is a real open of a full wallet). It passes
// when that pass fits the Monero watcher's pass timeout (xmrEscrowPassTimeout) and every call its own timeout; how it
// compares with the poll interval (PAYMENT_POLL_INTERVAL's default) is reported. The report is written to
// artifacts/real-chain/paye-e2-<ESCROW_XMR_PAYEE2_SHA>.txt and logged. FAKECHAIN ONLY: it refuses any other daemon
// before a wallet call. It needs the same nodes as the X-c legs (ESCROW_XMR_REGTEST_DAEMON_URL, _WALLET_URL,
// _FUNDER_URL and _RESTORE_URL); no database.
func TestXMREscrowPAYEE2(t *testing.T) {
	if os.Getenv("ESCROW_XMR_PAYEE2") != "1" {
		t.Skip("set ESCROW_XMR_PAYEE2=1 and the fakechain node URLs (scripts/test-escrow-xmr-payee2.sh) to measure PAYE-E2")
	}
	daemonURL, walletURL := os.Getenv("ESCROW_XMR_REGTEST_DAEMON_URL"), os.Getenv("ESCROW_XMR_REGTEST_WALLET_URL")
	funderURL, restoreURL := os.Getenv("ESCROW_XMR_REGTEST_FUNDER_URL"), os.Getenv("ESCROW_XMR_REGTEST_RESTORE_URL")
	if daemonURL == "" || walletURL == "" || funderURL == "" || restoreURL == "" {
		t.Fatal("set ESCROW_XMR_REGTEST_DAEMON_URL, _WALLET_URL, _FUNDER_URL and _RESTORE_URL")
	}
	num := func(key string, def int) int {
		if v := os.Getenv(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				t.Fatalf("%s must be a positive whole number", key)
			}
			return n
		}
		return def
	}
	nAccounts, nFunded, nWallets := num("ESCROW_XMR_PAYEE2_ACCOUNTS", 10_000), num("ESCROW_XMR_PAYEE2_FUNDED", 200), num("ESCROW_XMR_PAYEE2_WALLETS", 5)
	if nFunded > nAccounts || nFunded%2 != 0 {
		t.Fatal("ESCROW_XMR_PAYEE2_FUNDED must be even and at most ESCROW_XMR_PAYEE2_ACCOUNTS")
	}
	container := os.Getenv("ESCROW_XMR_PAYEE2_WALLET_CONTAINER")
	seed := uint64(time.Now().UnixNano())
	rng := rand.New(rand.NewPCG(seed, seed>>1))
	p := &payee2{t: t, container: container}
	defer p.write() // also when a step fails: the report then ends with VERDICT: FAIL
	p.logf("PAYE-E2 (P3-R12): one Monero escrow wallet with %d accounts, %d funded over %d blocks; a watcher pass over %d such wallets", nAccounts, nFunded, nFunded/2, nWallets)
	p.logf("monerod and monero-wallet-rpc 0.18.5.1 on a local fakechain; started %s UTC; random seed %d", time.Now().UTC().Format("2006-01-02 15:04"), seed)

	r, err := newXMREscrowRPC(walletURL, daemonURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = r.checkDaemon(ctx); err != nil {
		t.Fatalf("refusing: the daemon is not a local fakechain with hard fork 16 or later: %v", err)
	}
	reg := &xmrRegtest{xmrEscrowEnv: &xmrEscrowEnv{testEnv: &testEnv{t: t}}}
	for _, c := range []struct {
		dst      **rpcClient
		name, at string
	}{{&reg.daemon, "fakechain daemon", daemonURL}, {&reg.funder, "funder wallet-rpc", funderURL}, {&reg.restore, "restore wallet-rpc", restoreURL}} {
		if *c.dst, err = newRPCClient(c.name, c.at, "2.0", true); err != nil {
			t.Fatal(err)
		}
	}
	funder := "payee2-funder-" + randomToken()[:10]
	reg.call(reg.funder, "create_wallet", map[string]any{"filename": funder, "password": "", "language": "English"}, nil)
	var addr struct {
		Address string `json:"address"`
	}
	reg.call(reg.funder, "get_address", map[string]any{"account_index": 0}, &addr)
	reg.funderAddr = addr.Address
	reg.mine(nFunded/2 + 70) // a spendable coinbase output per funding transaction (coinbase unlocks after 60 blocks)

	// The escrow wallet and its accounts.
	month := time.Date(2200+rng.IntN(7800), time.Month(1+rng.IntN(12)), 1, 0, 0, 0, 0, time.UTC).Format("200601")
	names := make([]string, nWallets)
	for i := range names {
		names[i] = fmt.Sprintf("escrow-%s-%02d", month, i+1)
	}
	const password = "paye-e2 wallet password"
	start := time.Now()
	if err = r.createWallet(ctx, names[0], password); err != nil {
		t.Fatalf("create_wallet: %s", xmrErrorClass(err))
	}
	p.logf("create_wallet %s: %s", names[0], p.since(start))
	// The restore height, as xmrSealWallet records it: the wallet's height after its first refresh.
	if err = r.refresh(ctx); err != nil {
		t.Fatalf("refresh: %s", xmrErrorClass(err))
	}
	restoreHeight, err := r.height(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.memory("after create_wallet")
	calls := make([]time.Duration, 0, nAccounts)
	addresses := make([]string, nAccounts+1)
	start = time.Now()
	for i := 1; i <= nAccounts; i++ {
		t0 := time.Now()
		idx, a, err := r.createAccount(ctx)
		calls = append(calls, time.Since(t0))
		if err != nil || idx != int64(i) {
			t.Fatalf("create_account %d of %d (after %s): %d %s", i, nAccounts, p.since(start), idx, xmrErrorClass(err))
		}
		addresses[i] = a
		if i%2000 == 0 {
			p.logf("  %d accounts after %s", i, p.since(start))
		}
	}
	total := time.Since(start)
	p.logf("create_account x%d: total %s; per call %s (first 100: median %s; last 100: median %s; rpcTimeout %s)", nAccounts, p.dur(total), p.stats(calls),
		p.dur(median(calls[:min(100, len(calls))])), p.dur(median(calls[max(0, len(calls)-100):])), p.dur(rpcTimeout))
	var gets []time.Duration
	for _, i := range []int{1, nAccounts / 2, nAccounts} { // xmrTopUpSlots checks each new account with get_address
		t0 := time.Now()
		got, err := r.address(ctx, int64(i), 0)
		gets = append(gets, time.Since(t0))
		if err != nil || got != addresses[i] {
			t.Fatalf("get_address of account %d: %s", i, xmrErrorClass(err))
		}
	}
	p.logf("get_address of one account (3 samples): %s", p.stats(gets))
	start = time.Now()
	if err = r.store(ctx); err != nil {
		t.Fatalf("store: %s", xmrErrorClass(err))
	}
	p.logf("store after creating the accounts: %s", p.since(start))
	p.files(names[0], "after creating the accounts")
	p.memory("after creating the accounts")

	// Funding, with the escrow wallet closed: two random accounts per block.
	if err = r.closeWallet(ctx); err != nil {
		t.Fatalf("close_wallet: %s", xmrErrorClass(err))
	}
	funded := rng.Perm(nAccounts)[:nFunded]
	amounts := map[int]int64{}
	var fundedTotal int64
	start = time.Now()
	for i := 0; i < nFunded; i += 2 {
		pay := map[string]int64{}
		for _, a := range funded[i : i+2] {
			amounts[a+1] = 10_000_000_000 + rng.Int64N(990_000_000_000) // 0.01 to 1 XMR
			pay[addresses[a+1]] = amounts[a+1]
			fundedTotal += amounts[a+1]
		}
		reg.pay(pay)
		reg.mine(1)
	}
	reg.mine(10) // the last deposits unlock
	p.logf("funded %d accounts (%s XMR in all) over %d blocks, two per block, in %s", nFunded, amount(fundedTotal, 12), nFunded/2+10, p.since(start))

	open := func(name string) time.Duration {
		t0 := time.Now()
		if err := r.openWallet(ctx, name, password); err != nil {
			t.Fatalf("open_wallet %s: %s", name, xmrErrorClass(err))
		}
		return time.Since(t0)
	}
	// A refresh is measured to its end (the adapter would cut it off at xmrWalletSlowTimeout); one slower than that bound
	// is a finding: the watcher's call times out and the wallet-rpc goes on refreshing, busy, into the next pass.
	refresh := func(what string) time.Duration {
		d := p.slow(r.wallet, "refresh", nil, nil)
		p.over(what, d, xmrWalletSlowTimeout, "xmrWalletSlowTimeout")
		return d
	}
	d := open(names[0])
	p.logf("open_wallet with %d accounts: %s (xmrWalletSlowTimeout %s)", nAccounts, p.dur(d), p.dur(xmrWalletSlowTimeout))
	p.memory("after open_wallet")
	what := fmt.Sprintf("refresh over the %d funding blocks (%d deposits)", nFunded/2+10, nFunded)
	d = refresh(what)
	p.logf("%s: %s, %s per deposit received", what, p.dur(d), p.dur(d/time.Duration(nFunded)))
	start = time.Now()
	list, err := r.transfers(ctx)
	d = time.Since(start)
	if err != nil {
		t.Fatalf("get_transfers: %s", xmrErrorClass(err))
	}
	if len(list) != nFunded {
		t.Fatalf("get_transfers lists %d deposits, want %d", len(list), nFunded)
	}
	p.logf("get_transfers in, pool, all_accounts: %s, %d entries, %s payload", p.dur(d), len(list), p.size(r, "get_transfers", map[string]any{"in": true, "pool": true, "all_accounts": true}))
	start = time.Now()
	accounts, err := r.accounts(ctx)
	d = time.Since(start)
	if err != nil || len(accounts) != nAccounts+1 {
		t.Fatalf("get_accounts: %d accounts, %s", len(accounts), xmrErrorClass(err))
	}
	p.logf("get_accounts: %s, %d accounts, %s payload", p.dur(d), len(accounts), p.size(r, "get_accounts", nil))
	one := int64(funded[0] + 1)
	start = time.Now()
	outs, err := r.incomingTransfers(ctx, one)
	d = time.Since(start)
	if err != nil || len(outs) != 1 || outs[0].Amount != amounts[int(one)] {
		t.Fatalf("incoming_transfers of account %d: %+v %s", one, outs, xmrErrorClass(err))
	}
	p.logf("incoming_transfers of one funded account: %s", p.dur(d))
	mnemonic, err := r.mnemonic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err = r.closeWallet(ctx); err != nil {
		t.Fatalf("close_wallet: %s", xmrErrorClass(err))
	}
	p.logf("close_wallet (stores): %s", p.since(start))
	p.files(names[0], "after funding")

	// Refresh over 1,000 blocks.
	reg.mine(1000)
	d = open(names[0])
	p.logf("open_wallet again: %s", p.dur(d))
	p.logf("refresh over 1000 empty blocks: %s", p.dur(refresh("refresh over 1000 empty blocks")))
	start = time.Now()
	if err = r.closeWallet(ctx); err != nil {
		t.Fatalf("close_wallet: %s", xmrErrorClass(err))
	}
	p.logf("close_wallet (stores): %s", p.since(start))
	p.files(names[0], "after 1000 more blocks")
	p.memory("after 1000 more blocks")

	// What one pass reads on a busy day: two new deposits in one new block.
	pay := map[string]int64{}
	for _, a := range rng.Perm(nAccounts) { // two accounts not funded yet, so the restore below counts nFunded+2
		if _, ok := amounts[a+1]; !ok && len(pay) < 2 {
			pay[addresses[a+1]] = 50_000_000_000
		}
	}
	reg.pay(pay)
	reg.mine(1)
	open(names[0])
	p.logf("refresh over one block with 2 deposits: %s", p.dur(refresh("refresh over one block with 2 deposits")))
	if err = r.closeWallet(ctx); err != nil {
		t.Fatalf("close_wallet: %s", xmrErrorClass(err))
	}

	// A seed restore on the restore wallet-rpc: the lookahead past the highest account, then a rescan.
	restored := "payee2-restored-" + randomToken()[:10]
	d = p.slow(reg.restore, "restore_deterministic_wallet", map[string]any{"filename": restored, "password": password, "seed": mnemonic,
		"restore_height": restoreHeight, "language": "English", "autosave_current": false}, nil)
	p.logf("restore_deterministic_wallet from height %d: %s", restoreHeight, p.dur(d))
	// P3-R10 (slice X-f): the lookahead covers every account the database knows, one address each.
	d = p.slow(reg.restore, "set_subaddress_lookahead", map[string]any{"password": password, "major_idx": nAccounts + 1, "minor_idx": 1}, nil)
	p.logf("set_subaddress_lookahead major %d minor 1: %s", nAccounts+1, p.dur(d))
	d = p.slow(reg.restore, "rescan_blockchain", map[string]any{"hard": false}, nil)
	p.over("rescan_blockchain of the restored wallet", d, xmrWalletSlowTimeout, "xmrWalletSlowTimeout, the bound slice X-f gives it")
	p.logf("rescan_blockchain: %s (slice X-f bounds it by xmrWalletSlowTimeout, %s)", p.dur(d), p.dur(xmrWalletSlowTimeout))
	p.slow(reg.restore, "refresh", nil, nil)
	var bal struct {
		Balance int64 `json:"balance"`
		Per     []struct {
			Account int64 `json:"account_index"`
		} `json:"per_subaddress"`
	}
	reg.call(reg.restore, "get_balance", map[string]any{"all_accounts": true}, &bal)
	p.logf("the restored wallet finds %d funded accounts holding %s XMR (want %d and %s)", len(bal.Per), amount(bal.Balance, 12), nFunded+2, amount(fundedTotal+100_000_000_000, 12))
	if len(bal.Per) != nFunded+2 || bal.Balance != fundedTotal+100_000_000_000 {
		t.Errorf("the seed restore did not find every funded account")
	}
	reg.call(reg.restore, "close_wallet", nil, nil)

	// A full watcher pass over nWallets such wallets: copies of the measured wallet file.
	if nWallets > 1 {
		if container == "" {
			t.Fatal("ESCROW_XMR_PAYEE2_WALLET_CONTAINER must name the escrow wallet-rpc container to copy the wallet file for a pass over several wallets")
		}
		for _, n := range names[1:] {
			p.docker("cp -p /tmp/w/" + names[0] + " /tmp/w/" + n + " && cp -p /tmp/w/" + names[0] + ".keys /tmp/w/" + n + ".keys") // -p: the wallet-rpc's own user must own them
		}
		p.logf("copied the wallet file to %s (each opens with its own name and the same keys)", strings.Join(names[1:], ", "))
	}
	reg.mine(1) // as in production, each pass has a new block to read
	var pass time.Duration
	var steps []string
	worst := map[string]time.Duration{}
	for _, name := range names {
		step := func(label string, limit time.Duration, fn func() error) {
			t0 := time.Now()
			if err := fn(); err != nil {
				t.Fatalf("watcher pass, wallet %s, %s: %s", name, label, xmrErrorClass(err))
			}
			d := time.Since(t0)
			pass += d
			worst[label] = max(worst[label], d)
			if d > limit {
				t.Errorf("watcher pass, wallet %s: %s took %s, over its timeout %s", name, label, p.dur(d), p.dur(limit))
			}
		}
		t0 := pass
		step("open_wallet", xmrWalletSlowTimeout, func() error { return r.openWallet(ctx, name, password) })
		var h int64
		step("get_info and hard_fork_info", rpcTimeout, func() (err error) { h, err = r.checkDaemon(ctx); return })
		step("refresh", xmrWalletSlowTimeout, func() error { return r.refresh(ctx) })
		step("get_height", rpcTimeout, func() error {
			got, err := r.height(ctx)
			if err == nil && (got-h > xmrMaxWalletLag || h-got > xmrMaxWalletLag) {
				return errXMRWalletLag
			}
			return err
		})
		step("get_address of account 0", rpcTimeout, func() error { _, err := r.primaryAddress(ctx); return err })
		step("get_transfers", rpcTimeout, func() error { _, err := r.transfers(ctx); return err })
		step("get_accounts", rpcTimeout, func() error { _, err := r.accounts(ctx); return err })
		step("store", xmrWalletSlowTimeout, func() error { return r.store(ctx) })
		steps = append(steps, fmt.Sprintf("%s %s", name, p.dur(pass-t0)))
	}
	p.memory("after the watcher pass")
	poll, _ := pollInterval()
	p.logf("watcher pass over %d wallets: %s (%s)", nWallets, p.dur(pass), strings.Join(steps, "; "))
	var worstLines []string
	for _, k := range slices.Sorted(maps.Keys(worst)) {
		worstLines = append(worstLines, k+" "+p.dur(worst[k]))
	}
	p.logf("slowest call of each kind in the pass: %s", strings.Join(worstLines, ", "))
	p.logf("Monero watcher pass timeout (xmrEscrowPassTimeout): %s; poll interval (PAYMENT_POLL_INTERVAL default): %s", p.dur(xmrEscrowPassTimeout), p.dur(poll))
	for _, f := range p.findings {
		p.logf("FINDING: %s", f)
	}
	switch {
	case t.Failed():
		p.logf("VERDICT: FAIL (see the errors above)")
	case pass > xmrEscrowPassTimeout:
		t.Errorf("the pass over %d wallets took %s, over the pass timeout %s", nWallets, p.dur(pass), p.dur(xmrEscrowPassTimeout))
		p.logf("VERDICT: FAIL: the pass does not fit the pass timeout")
	case pass > poll:
		p.logf("VERDICT: PASS: the pass fits the pass timeout with %s to spare; it is longer than the poll interval, so passes run back to back (the next starts one interval after the last ends)", p.dur(xmrEscrowPassTimeout-pass))
	default:
		p.logf("VERDICT: PASS: the pass fits the pass timeout and the poll interval")
	}
	p.verdict = true
}

// payee2 collects PAYE-E2's report lines.
type payee2 struct {
	t         *testing.T
	container string
	lines     []string
	findings  []string // calls that took longer than the bound the app gives them, outside the watcher pass
	verdict   bool     // the verdict line was written
}

// over records a finding when what took d, longer than limit (named limitName).
func (p *payee2) over(what string, d, limit time.Duration, limitName string) {
	if d > limit {
		p.findings = append(p.findings, fmt.Sprintf("%s took %s, longer than %s (%s)", what, p.dur(d), limitName, p.dur(limit)))
	}
}

// write writes the report to artifacts/real-chain/paye-e2-<ESCROW_XMR_PAYEE2_SHA>.txt.
func (p *payee2) write() {
	if !p.verdict {
		p.logf("VERDICT: FAIL: the measurement stopped at the step above")
	}
	sha := os.Getenv("ESCROW_XMR_PAYEE2_SHA")
	if sha == "" {
		sha = "unknown"
	}
	out := filepath.Join("artifacts", "real-chain", "paye-e2-"+sha+".txt")
	err := os.MkdirAll(filepath.Dir(out), 0o755)
	if err == nil {
		err = os.WriteFile(out, []byte(strings.Join(p.lines, "\n")+"\n"), 0o644)
	}
	if err != nil {
		p.t.Errorf("writing the report: %v", err)
		return
	}
	p.t.Logf("report written to %s", out)
}

// slow calls a wallet-rpc method with a long bound (30 minutes) rather than the adapter's, so a call slower than the
// bound the app gives it is measured to its end; the caller records it with over.
func (p *payee2) slow(c *rpcClient, method string, params, out any) time.Duration {
	p.t.Helper()
	t0 := time.Now()
	if err := c.callWithin(context.Background(), 30*time.Minute, "/json_rpc", method, params, out); err != nil {
		p.t.Fatalf("%s: %v", method, err)
	}
	return time.Since(t0)
}

func (p *payee2) logf(format string, args ...any) {
	p.t.Helper()
	line := fmt.Sprintf(format, args...)
	p.lines = append(p.lines, line)
	p.t.Log(line)
}

func (p *payee2) dur(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%.1f min", d.Minutes())
	case d >= time.Second:
		return fmt.Sprintf("%.2f s", d.Seconds())
	}
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

func (p *payee2) since(t time.Time) string { return p.dur(time.Since(t)) }

func median(ds []time.Duration) time.Duration {
	s := slices.Sorted(slices.Values(ds))
	return s[len(s)/2]
}

func (p *payee2) stats(ds []time.Duration) string {
	s := slices.Sorted(slices.Values(ds))
	return fmt.Sprintf("min %s, median %s, p99 %s, max %s", p.dur(s[0]), p.dur(s[len(s)/2]), p.dur(s[len(s)*99/100]), p.dur(s[len(s)-1]))
}

// size is the byte size of method's answer from the open wallet.
func (p *payee2) size(r *xmrEscrowRPC, method string, params any) string {
	var raw json.RawMessage
	if err := r.wallet.callWithin(context.Background(), xmrWalletSlowTimeout, "/json_rpc", method, params, &raw); err != nil {
		p.t.Fatalf("%s: %v", method, err)
	}
	return fmt.Sprintf("%.1f KiB", float64(len(raw))/1024)
}

func (p *payee2) docker(script string) string {
	out, err := exec.Command("docker", "exec", p.container, "sh", "-c", script).CombinedOutput()
	if err != nil {
		p.t.Fatalf("docker exec in %s: %v: %s", p.container, err, out)
	}
	return strings.TrimSpace(string(out))
}

// files reports the wallet's cache and keys file sizes in the container.
func (p *payee2) files(name, when string) {
	if p.container == "" {
		return
	}
	sizes := strings.Fields(p.docker("stat -c %s /tmp/w/" + name + " /tmp/w/" + name + ".keys"))
	if len(sizes) != 2 {
		p.t.Fatalf("stat: %v", sizes)
	}
	mib := func(s string) string { n, _ := strconv.ParseFloat(s, 64); return fmt.Sprintf("%.1f MiB", n/(1<<20)) }
	p.logf("wallet files %s: cache %s, keys %s", when, mib(sizes[0]), mib(sizes[1]))
}

// memory reports the wallet-rpc's resident memory and its peak (it is the container's PID 1).
func (p *payee2) memory(when string) {
	if p.container == "" {
		return
	}
	p.logf("wallet-rpc memory %s: %s", when, strings.Join(strings.Fields(p.docker("grep -E '^(VmRSS|VmHWM)' /proc/1/status")), " "))
}
