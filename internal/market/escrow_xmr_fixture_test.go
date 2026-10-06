package market

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestXMREscrowFixtureAdapter: the simulated Monero escrow nodes of the browser journey (tests/e2e/fixtures/
// xmr_escrow_rpc.py, served by wallet-rpc.py --xmr-wallet-port --xmr-daemon-port; SIMULATED: not chain evidence) answer
// the app's own adapter in shapes it parses, with keys its offline parser accepts (checksum and prime-order keys,
// A-417), and with the faults it classifies: the -21, -13, -8, -4 codes and a transfer that drops the connection
// (xmrRPCError.Dropped). A transaction the simulator builds passes the settlement builder's checkXMRBuild. The reply
// shapes themselves are held to monero-wallet-rpc 0.18.5.1 by tests/test_xmr_escrow_rpc_fixture.py; this keeps the
// simulator and the Go adapter from drifting apart. No database is needed (TestMain runs the tests from the repository
// root).
func TestXMREscrowFixtureAdapter(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to run the simulated Monero escrow nodes")
	}
	ports := make([]string, 3)
	for i := range ports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
		l.Close()
	}
	cmd := exec.Command(py, "tests/e2e/fixtures/wallet-rpc.py", "--port", ports[0], "--xmr-wallet-port", ports[1], "--xmr-daemon-port", ports[2])
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	walletURL, daemonURL := "http://127.0.0.1:"+ports[1], "http://127.0.0.1:"+ports[2]
	sim := func(path string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		res, err := http.Post(walletURL+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer res.Body.Close()
		var out map[string]any
		if err = json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: status %d %v %v", path, res.StatusCode, out, err)
		}
		return out
	}
	for i := 0; ; i++ {
		res, err := http.Get(walletURL + "/health")
		if err == nil {
			res.Body.Close()
			break
		}
		if i == 100 {
			t.Fatalf("the simulated Monero escrow nodes did not start: %v %s", err, stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	r, err := newXMREscrowRPC(walletURL, daemonURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	code := func(err error) int64 {
		var re *xmrRPCError
		if !errors.As(err, &re) {
			t.Fatalf("not an RPC error: %v", err)
		}
		return re.Code
	}

	if net, open, err := testXMRCheckNetwork(ctx, r); err != nil || open || net != "fakechain" {
		t.Fatalf("network check with no wallet open: %s %v %v", net, open, err)
	}
	r.net.Store("fakechain") // as startEscrowXMR leaves it once the daemon is verified (A-449)
	const name = "escrow-209901-01"
	if err = r.createWallet(ctx, name, "first password"); err != nil {
		t.Fatal(err)
	}
	if c := code(r.createWallet(ctx, name, "first password")); c != -21 {
		t.Fatalf("second create_wallet: code %d", c)
	}
	if _, open, err := testXMRCheckNetwork(ctx, r); err != nil || !open {
		t.Fatalf("network check: %v %v", open, err)
	}
	seed, err := r.mnemonic(ctx)
	if err != nil || len(strings.Fields(seed)) != 25 {
		t.Fatalf("mnemonic: %d words, %v", len(strings.Fields(seed)), err)
	}
	primary, err := r.primaryAddress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	account, addr, err := r.createAccount(ctx)
	if err != nil || account != 1 {
		t.Fatalf("create_account: %d %v", account, err)
	}
	if got, err := r.address(ctx, account, 0); err != nil || got != addr {
		t.Fatalf("get_address: %q %v, want %s", got, err, addr)
	}
	if accounts, err := r.accounts(ctx); err != nil || len(accounts) != 2 || accounts[0].BaseAddress != primary || accounts[1].BaseAddress != addr {
		t.Fatalf("get_accounts: %+v %v", accounts, err)
	}
	if list, err := r.transfers(ctx); err != nil || len(list) != 0 {
		t.Fatalf("get_transfers of a new wallet: %+v %v", list, err)
	}
	if outs, err := r.incomingTransfers(ctx, account); err != nil || len(outs) != 0 {
		t.Fatalf("incoming_transfers of a new account: %+v %v", outs, err)
	}

	const price = 500_000_000_000
	deposit := sim("/test/xmr/deposit", map[string]any{"address": addr, "amount": price})["txid"].(string)
	if err = r.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if list, err := r.transfers(ctx); err != nil || len(list) != 1 || list[0].Type != "pool" || list[0].TxID != deposit || list[0].Account != 1 || list[0].Amount != price {
		t.Fatalf("a deposit in the pool: %+v %v", list, err)
	}
	sim("/test/xmr/mine", map[string]any{"blocks": 10})
	if err = r.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if list, err := r.transfers(ctx); err != nil || len(list) != 1 || list[0].Type != "in" || list[0].Confirmations != 10 || list[0].Locked || list[0].DoubleSpendSeen {
		t.Fatalf("a deposit at 10 confirmations: %+v %v", list, err)
	}
	if b, err := r.balance(ctx); err != nil || b.Balance != price || b.Unlocked != price || len(b.PerSubaddress) != 1 || b.PerSubaddress[0].Account != 1 {
		t.Fatalf("get_balance: %+v %v", b, err)
	}
	outs, err := r.incomingTransfers(ctx, account)
	if err != nil || len(outs) != 1 || !outs[0].Unlocked || outs[0].Frozen || outs[0].TxHash != deposit {
		t.Fatalf("incoming_transfers: %+v %v", outs, err)
	}
	if err = r.freeze(ctx, outs[0].KeyImage); err != nil {
		t.Fatal(err)
	}
	if again, _ := r.incomingTransfers(ctx, account); len(again) != 1 || !again[0].Frozen {
		t.Fatalf("frozen: %+v", again)
	}
	if err = r.thaw(ctx, outs[0].KeyImage); err != nil {
		t.Fatal(err)
	}

	// A release as the settlement builder plans it: the claimant (the fee from their output) and the market fee.
	claimant := sim("/test/xmr/address", map[string]any{})["address"].(string)
	feeTo := sim("/test/xmr/address", map[string]any{"label": "market fee"})["address"].(string)
	for _, a := range []string{claimant, feeTo} {
		if err := checkXMRFormAddress(a, "fakechain"); err != nil {
			t.Fatalf("the simulator's address %s: %v", a, err)
		}
	}
	plan := []settleOutput{{"claimant", claimant, price - 12_500_000_000}, {"market_fee", feeTo, 12_500_000_000}}
	tx, err := r.buildTransfer(ctx, account, []xmrDestination{{claimant, plan[0].Sats}, {feeTo, plan[1].Sats}})
	if err != nil {
		t.Fatal(err)
	}
	if class := checkXMRBuild(tx, plan, []string{outs[0].KeyImage}, xmrDefaultMaxFee, minPayoutXMR); class != "" {
		t.Fatalf("the settlement builder discards the simulator's transaction: %s (%+v)", class, tx)
	}
	if valid, err := r.validAddress(ctx, claimant); err != nil || !valid {
		t.Fatalf("validate_address: %v %v", valid, err)
	}
	if txs, missed, err := r.transactions(ctx, []string{tx.TxHash}); err != nil || len(txs) != 0 || !slices.Equal(missed, []string{tx.TxHash}) {
		t.Fatalf("an unrelayed transaction: %+v %v %v", txs, missed, err)
	}
	for range 2 { // relay_tx is idempotent
		if got, err := r.relayTx(ctx, tx.Metadata); err != nil || got != tx.TxHash {
			t.Fatalf("relay_tx: %q %v", got, err)
		}
	}
	if txs, _, err := r.transactions(ctx, []string{tx.TxHash}); err != nil || len(txs) != 1 || !txs[0].InPool || txs[0].RCTType != 6 ||
		!slices.Equal(txs[0].RingSizes, []int{16}) || !slices.Equal(txs[0].KeyImages, tx.SpentKeyImages) {
		t.Fatalf("in the pool: %+v %v", txs, err)
	}
	if spent, err := r.keyImagesSpent(ctx, tx.SpentKeyImages); err != nil || !slices.Equal(spent, []int{2}) {
		t.Fatalf("is_key_image_spent in the pool: %v %v", spent, err)
	}
	if list, found, err := r.transferByTxid(ctx, account, tx.TxHash); err != nil || !found || list[0].Type != "pending" || list[0].Fee != tx.Fee {
		t.Fatalf("get_transfer_by_txid: %+v %v %v", list, found, err)
	}
	sim("/test/xmr/mine", map[string]any{"blocks": 10})
	if txs, _, err := r.transactions(ctx, []string{tx.TxHash}); err != nil || len(txs) != 1 || txs[0].InPool || txs[0].Confirmations != 10 {
		t.Fatalf("mined: %+v %v", txs, err)
	}
	if spent, err := r.keyImagesSpent(ctx, tx.SpentKeyImages); err != nil || !slices.Equal(spent, []int{1}) {
		t.Fatalf("is_key_image_spent on chain: %v %v", spent, err)
	}
	if _, found, err := r.transferByTxid(ctx, account, hex64("unknown")); err != nil || found {
		t.Fatalf("an unknown transaction: %v %v", found, err)
	}

	// A refused relay (-4), as relayXMRStored reads it, and a transfer that drops the wallet-rpc (xmrBuildRefused).
	second := sim("/test/xmr/deposit", map[string]any{"address": addr, "amount": price})["txid"].(string)
	sim("/test/xmr/mine", map[string]any{"blocks": 10})
	if err = r.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	refund := []xmrDestination{{sim("/test/xmr/address", map[string]any{})["address"].(string), price}}
	built, err := r.buildTransfer(ctx, account, refund)
	if err != nil {
		t.Fatal(err)
	}
	sim("/test/xmr/refuse-relay", map[string]any{})
	if _, err = r.relayTx(ctx, built.Metadata); code(err) != -4 {
		t.Fatalf("a refused relay: %v", err)
	}
	sim("/test/xmr/crash-transfer", map[string]any{"count": 1})
	_, err = r.buildTransfer(ctx, account, refund)
	var re *xmrRPCError
	if !errors.As(err, &re) || !re.Dropped {
		t.Fatalf("a transfer that crashed the wallet-rpc: %v", err)
	}
	if _, err = r.height(ctx); !xmrNotOpen(err) {
		t.Fatalf("after the crash: %v", err)
	}
	if err = r.openWallet(ctx, name, "first password"); err != nil {
		t.Fatal(err)
	}
	if list, _, err := r.transferByTxid(ctx, account, second); err != nil || len(list) != 1 || list[0].Type != "in" {
		t.Fatalf("the second deposit: %+v %v", list, err)
	}

	// Passwords and the wallet lifecycle.
	if err = r.changeWalletPassword(ctx, "first password", "second password"); err != nil {
		t.Fatal(err)
	}
	if err = r.store(ctx); err != nil {
		t.Fatal(err)
	}
	if err = r.closeWallet(ctx); err != nil {
		t.Fatal(err)
	}
	if c := code(r.openWallet(ctx, name, "first password")); c != -1 {
		t.Fatalf("open_wallet with the old password: code %d", c)
	}
	if err = r.openWallet(ctx, name, "second password"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.primaryAddress(ctx); err != nil || got != primary {
		t.Fatalf("after reopening: %q %v", got, err)
	}
}
