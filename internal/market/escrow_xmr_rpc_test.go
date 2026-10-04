package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeXMRNode is a monero-wallet-rpc or monerod for the escrow adapter's tests: /json_rpc answers JSON-RPC 2.0 by
// method, any other path (monerod's /get_transactions, /is_key_image_spent) answers plain JSON by path. Every request
// is recorded with its exact params (the raw JSON the client sent). It asks for no authentication, so the
// credentials in its URL never leave the client.
type fakeXMRNode struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []fakeXMRCall
	replies map[string]func(params json.RawMessage) (any, int, string)
	delay   map[string]time.Duration
}

type fakeXMRCall struct{ Path, Method, Params string }

func newFakeXMRNode(t *testing.T) *fakeXMRNode {
	t.Helper()
	f := &fakeXMRNode{replies: map[string]func(json.RawMessage) (any, int, string){}, delay: map[string]time.Duration{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// url is the node's URL with credentials, as an operator configures it.
func (f *fakeXMRNode) url() string {
	return strings.Replace(f.URL, "http://", "http://escrowuser:s3cret-escrow-pass@", 1)
}

func (f *fakeXMRNode) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	name, params := r.URL.Path, json.RawMessage(body)
	if r.URL.Path == "/json_rpc" {
		if json.Unmarshal(body, &req) != nil {
			w.WriteHeader(400)
			return
		}
		name, params = req.Method, req.Params
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeXMRCall{r.URL.Path, req.Method, string(params)})
	reply, delay := f.replies[name], f.delay[name]
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	result, code, msg := any(map[string]any{}), 0, ""
	if reply != nil {
		result, code, msg = reply(params)
	}
	if r.URL.Path != "/json_rpc" {
		json.NewEncoder(w).Encode(result)
		return
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if code != 0 {
		resp["error"] = map[string]any{"code": code, "message": msg}
	} else {
		resp["result"] = result
	}
	json.NewEncoder(w).Encode(resp)
}

func (f *fakeXMRNode) set(name string, fn func(json.RawMessage) (any, int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[name] = fn
}

func (f *fakeXMRNode) result(name string, v any) {
	f.set(name, func(json.RawMessage) (any, int, string) { return v, 0, "" })
}

func (f *fakeXMRNode) setDelay(name string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay[name] = d
}

func (f *fakeXMRNode) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

func (f *fakeXMRNode) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeXMRNode) last(t *testing.T) fakeXMRCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no request was made")
	}
	return f.calls[len(f.calls)-1]
}

func hex64(label string) string { s := sha256.Sum256([]byte(label)); return hex.EncodeToString(s[:]) }

// testXMRTxJSON is a decoded fakechain transaction as monerod's /get_transactions returns it in as_json: CLSAG /
// Bulletproofs+ (rct type 6) and ring size 16, as probed on monerod 0.18.5.1 fakechain.
func testXMRTxJSON(keyImages ...string) string {
	offsets := make([]int, 16)
	for i := range offsets {
		offsets[i] = i + 1
	}
	var vin []any
	for _, ki := range keyImages {
		vin = append(vin, map[string]any{"key": map[string]any{"amount": 0, "key_offsets": offsets, "k_image": ki}})
	}
	b, _ := json.Marshal(map[string]any{"version": 2, "unlock_time": 0, "vin": vin, "vout": []any{}, "extra": []int{1},
		"rct_signatures": map[string]any{"type": 6, "txnFee": 1874400000}})
	return string(b)
}

// happyXMRNodes is a wallet-rpc and a monerod on fakechain at height 120 with a wallet open.
func happyXMRNodes(t *testing.T) (wallet, daemon *fakeXMRNode, r *xmrEscrowRPC) {
	t.Helper()
	wallet, daemon = newFakeXMRNode(t), newFakeXMRNode(t)
	wallet.result("refresh", map[string]any{"blocks_fetched": 0, "received_money": false})
	wallet.result("query_key", map[string]any{"key": testMnemonic})
	wallet.result("get_height", map[string]any{"height": 120})
	wallet.result("create_account", map[string]any{"account_index": 3, "address": testXMRAddr("account 3", true)})
	wallet.set("get_address", func(p json.RawMessage) (any, int, string) {
		var q struct {
			Account int64   `json:"account_index"`
			Minor   []int64 `json:"address_index"`
		}
		json.Unmarshal(p, &q)
		base := testXMRAddr("primary", false)
		if q.Account != 0 {
			base = testXMRAddr("account", true)
		}
		return map[string]any{"address": base, "addresses": []any{map[string]any{"address": base, "address_index": q.Minor[0], "label": "", "used": false}}}, 0, ""
	})
	wallet.result("get_accounts", map[string]any{"subaddress_accounts": []any{
		map[string]any{"account_index": 0, "balance": 0, "unlocked_balance": 0, "base_address": testXMRAddr("primary", false), "label": "Primary account"},
		map[string]any{"account_index": 1, "balance": 7000, "unlocked_balance": 5000, "base_address": testXMRAddr("account", true), "label": ""},
	}, "total_balance": 7000, "total_unlocked_balance": 5000})
	wallet.result("get_balance", map[string]any{"balance": 7000, "unlocked_balance": 5000, "per_subaddress": []any{
		map[string]any{"account_index": 1, "address_index": 0, "balance": 7000, "unlocked_balance": 5000, "address": testXMRAddr("account", true)},
	}})
	wallet.result("get_transfers", map[string]any{
		"in":   []any{map[string]any{"txid": hex64("dep"), "type": "in", "amount": 5000, "fee": 30, "confirmations": 12, "height": 108, "double_spend_seen": false, "unlock_time": 0, "subaddr_index": map[string]any{"major": 2, "minor": 0}}},
		"pool": []any{map[string]any{"txid": hex64("pool"), "type": "pool", "amount": 2000, "fee": 30, "confirmations": 0, "height": 0, "double_spend_seen": false, "unlock_time": 500, "subaddr_index": map[string]any{"major": 4, "minor": 0}}},
	})
	wallet.set("incoming_transfers", func(p json.RawMessage) (any, int, string) {
		var q struct {
			Account int64 `json:"account_index"`
		}
		json.Unmarshal(p, &q)
		return map[string]any{"transfers": []any{map[string]any{"amount": 5000, "block_height": 108, "frozen": false, "global_index": 77,
			"key_image": hex64("ki1"), "pubkey": hex64("pk1"), "spent": false, "subaddr_index": map[string]any{"major": q.Account, "minor": 0},
			"tx_hash": hex64("dep"), "unlocked": true}}}, 0, ""
	})
	wallet.set("transfer", func(p json.RawMessage) (any, int, string) {
		var q struct {
			Destinations []map[string]any `json:"destinations"`
		}
		json.Unmarshal(p, &q)
		amounts := make([]int64, len(q.Destinations))
		for i := range amounts {
			amounts[i] = 50
		}
		amounts[0] = 900 - 30
		return map[string]any{"tx_hash": hex64("settle"), "fee": 30, "amount": 970, "tx_metadata": "0a0b0c", "tx_blob": "0102",
			"tx_key": "", "weight": 1500, "spent_key_images": map[string]any{"key_images": []string{hex64("ki1")}},
			"amounts_by_dest": map[string]any{"amounts": amounts}, "multisig_txset": "", "unsigned_txset": ""}, 0, ""
	})
	wallet.result("relay_tx", map[string]any{"tx_hash": hex64("settle")})
	wallet.result("get_transfer_by_txid", map[string]any{"transfer": map[string]any{"txid": hex64("settle"), "type": "out"},
		"transfers": []any{map[string]any{"txid": hex64("settle"), "type": "out", "amount": 970, "fee": 30, "confirmations": 2, "height": 119,
			"double_spend_seen": false, "unlock_time": 0, "subaddr_index": map[string]any{"major": 3, "minor": 0}}}})
	daemon.result("get_info", map[string]any{"nettype": "fakechain", "mainnet": false, "stagenet": false, "testnet": false, "height": 120, "target_height": 0, "status": "OK"})
	daemon.result("hard_fork_info", map[string]any{"version": 16, "enabled": true, "status": "OK"})
	daemon.result("/get_transactions", map[string]any{"status": "OK", "missed_tx": []string{hex64("gone")}, "txs": []any{map[string]any{
		"tx_hash": hex64("settle"), "in_pool": false, "block_height": 110, "confirmations": 11, "double_spend_seen": false,
		"as_json": testXMRTxJSON(hex64("ki1"), hex64("ki2"))}}})
	daemon.set("/is_key_image_spent", func(p json.RawMessage) (any, int, string) { // 0, 1, 2, 0, ... one per key image
		var q struct {
			KeyImages []string `json:"key_images"`
		}
		json.Unmarshal(p, &q)
		status := make([]int, len(q.KeyImages))
		for i := range status {
			status[i] = i % 3
		}
		return map[string]any{"status": "OK", "spent_status": status}, 0, ""
	})
	var err error
	if r, err = newXMREscrowRPC(wallet.url(), daemon.url()); err != nil {
		t.Fatal(err)
	}
	return wallet, daemon, r
}

// TestXMREscrowRPCPinsParams pins the path, method and exact JSON params of every adapter call (P3-R3, R6, R9, R10):
// account_index is explicit and non-zero wherever one account is spent or read, transfer never relays and names
// only the claimant (index 0) to pay the fee, and the all-account reads say all_accounts.
func TestXMREscrowRPCPinsParams(t *testing.T) {
	ctx := context.Background()
	wallet, daemon, r := happyXMRNodes(t)
	claimant, fee, buyer := testXMRAddr("claimant", true), testXMRAddr("fee", false), testXMRAddr("buyer", true)
	ki, txid := hex64("ki1"), hex64("settle")
	steps := []struct {
		node                 *fakeXMRNode
		path, method, params string
		call                 func() error
	}{
		{wallet, "/json_rpc", "create_wallet", `{"filename":"escrow-202610-01","language":"English","password":"pw"}`, func() error { return r.createWallet(ctx, "escrow-202610-01", "pw") }},
		{wallet, "/json_rpc", "open_wallet", `{"filename":"escrow-202610-01","password":"pw"}`, func() error { return r.openWallet(ctx, "escrow-202610-01", "pw") }},
		{wallet, "/json_rpc", "close_wallet", ``, func() error { return r.closeWallet(ctx) }},
		{wallet, "/json_rpc", "store", ``, func() error { return r.store(ctx) }},
		{wallet, "/json_rpc", "refresh", ``, func() error { return r.refresh(ctx) }},
		{wallet, "/json_rpc", "query_key", `{"key_type":"mnemonic"}`, func() error { _, err := r.mnemonic(ctx); return err }},
		{wallet, "/json_rpc", "get_height", ``, func() error { _, err := r.height(ctx); return err }},
		{wallet, "/json_rpc", "create_account", ``, func() error { _, _, err := r.createAccount(ctx); return err }},
		{wallet, "/json_rpc", "get_address", `{"account_index":0,"address_index":[0]}`, func() error { _, err := r.primaryAddress(ctx); return err }},
		{wallet, "/json_rpc", "get_address", `{"account_index":3,"address_index":[0]}`, func() error { _, err := r.address(ctx, 3, 0); return err }},
		{wallet, "/json_rpc", "get_accounts", ``, func() error { _, err := r.accounts(ctx); return err }},
		{wallet, "/json_rpc", "get_balance", `{"all_accounts":true}`, func() error { _, err := r.balance(ctx); return err }},
		{wallet, "/json_rpc", "get_transfers", `{"all_accounts":true,"in":true,"pool":true}`, func() error { _, err := r.transfers(ctx); return err }},
		{wallet, "/json_rpc", "incoming_transfers", `{"account_index":3,"transfer_type":"available","verbose":true}`, func() error { _, err := r.incomingTransfers(ctx, 3); return err }},
		{wallet, "/json_rpc", "freeze", `{"key_image":"` + ki + `"}`, func() error { return r.freeze(ctx, ki) }},
		{wallet, "/json_rpc", "thaw", `{"key_image":"` + ki + `"}`, func() error { return r.thaw(ctx, ki) }},
		{wallet, "/json_rpc", "transfer", `{"account_index":3,"destinations":[{"address":"` + claimant + `","amount":900},{"address":"` + fee + `","amount":50},{"address":"` + buyer + `","amount":50}],"do_not_relay":true,"get_tx_hex":true,"get_tx_metadata":true,"priority":0,"subtract_fee_from_outputs":[0]}`, func() error {
			_, err := r.buildTransfer(ctx, 3, []xmrDestination{{claimant, 900}, {fee, 50}, {buyer, 50}})
			return err
		}},
		{wallet, "/json_rpc", "relay_tx", `{"hex":"0a0b0c"}`, func() error { _, err := r.relayTx(ctx, "0a0b0c"); return err }},
		{wallet, "/json_rpc", "get_transfer_by_txid", `{"account_index":3,"txid":"` + txid + `"}`, func() error { _, _, err := r.transferByTxid(ctx, 3, txid); return err }},
		{wallet, "/json_rpc", "change_wallet_password", `{"new_password":"new","old_password":"old"}`, func() error { return r.changeWalletPassword(ctx, "old", "new") }},
		{wallet, "/json_rpc", "set_subaddress_lookahead", `{"major_idx":60,"minor_idx":5,"password":"pw"}`, func() error { return r.setSubaddressLookahead(ctx, "pw", 60, 5) }},
		{wallet, "/json_rpc", "rescan_blockchain", `{"hard":false}`, func() error { return r.rescan(ctx) }},
		{wallet, "/json_rpc", "open_wallet", `{"autosave_current":false,"filename":"escrow-202610-02","password":"pw"}`, func() error { _, err := r.probeWallet(ctx, "escrow-202610-02", "pw"); return err }},
		{wallet, "/json_rpc", "get_transfers", `{"all_accounts":true,"pending":true,"pool":true}`, func() error { _, err := r.inPool(ctx); return err }},
		{daemon, "/json_rpc", "get_info", ``, func() error { _, err := r.daemonInfo(ctx); return err }},
		{daemon, "/json_rpc", "hard_fork_info", ``, func() error { _, err := r.hardForkVersion(ctx); return err }},
		{daemon, "/get_transactions", "", `{"decode_as_json":true,"txs_hashes":["` + txid + `"]}`, func() error { _, _, err := r.transactions(ctx, []string{txid}); return err }},
		{daemon, "/is_key_image_spent", "", `{"key_images":["` + ki + `"]}`, func() error { _, err := r.keyImagesSpent(ctx, []string{ki}); return err }},
	}
	for _, s := range steps {
		before := s.node.count()
		if err := s.call(); err != nil {
			t.Fatalf("%s%s: %v", s.method, s.path, err)
		}
		got := s.node.last(t)
		if s.node.count() != before+1 || got != (fakeXMRCall{s.path, s.method, s.params}) {
			t.Errorf("%s%s: %d requests, last %+v\nwant %s", s.method, s.path, s.node.count()-before, got, s.params)
		}
	}
}

// TestXMREscrowRPCResults: each call's typed result, and the reply shapes the adapter refuses as bad-reply.
func TestXMREscrowRPCResults(t *testing.T) {
	ctx := context.Background()
	wallet, daemon, r := happyXMRNodes(t)
	if m, err := r.mnemonic(ctx); err != nil || m != testMnemonic {
		t.Fatalf("mnemonic: %v", err)
	}
	if idx, addr, err := r.createAccount(ctx); err != nil || idx != 3 || addr != testXMRAddr("account 3", true) {
		t.Fatalf("create_account: %d %s %v", idx, addr, err)
	}
	if a, err := r.primaryAddress(ctx); err != nil || a != testXMRAddr("primary", false) {
		t.Fatalf("primary: %s %v", a, err)
	}
	if acc, err := r.accounts(ctx); err != nil || len(acc) != 2 || acc[1] != (xmrAccount{Index: 1, BaseAddress: testXMRAddr("account", true), Balance: 7000, Unlocked: 5000}) {
		t.Fatalf("accounts: %+v %v", acc, err)
	}
	if b, err := r.balance(ctx); err != nil || b.Balance != 7000 || b.Unlocked != 5000 || len(b.PerSubaddress) != 1 || b.PerSubaddress[0] != (xmrSubaddressBalance{Account: 1, Minor: 0, Balance: 7000, Unlocked: 5000}) {
		t.Fatalf("balance: %+v %v", b, err)
	}
	tr, err := r.transfers(ctx)
	if err != nil || len(tr) != 2 || tr[0] != (xmrTransfer{TxID: hex64("dep"), Type: "in", Account: 2, Amount: 5000, Fee: 30, Confirmations: 12, Height: 108}) ||
		tr[1] != (xmrTransfer{TxID: hex64("pool"), Type: "pool", Account: 4, Amount: 2000, Fee: 30, Locked: true}) {
		t.Fatalf("transfers: %+v %v", tr, err)
	}
	outs, err := r.incomingTransfers(ctx, 3)
	if err != nil || len(outs) != 1 || outs[0] != (xmrOutput{KeyImage: hex64("ki1"), TxHash: hex64("dep"), Amount: 5000, BlockHeight: 108, Minor: 0, Unlocked: true}) {
		t.Fatalf("incoming: %+v %v", outs, err)
	}
	claimant, fee := testXMRAddr("claimant", true), testXMRAddr("fee", false)
	tx, err := r.buildTransfer(ctx, 3, []xmrDestination{{claimant, 900}, {fee, 50}})
	if err != nil || tx.TxHash != hex64("settle") || tx.Fee != 30 || tx.Amount != 970 || tx.Metadata != "0a0b0c" || tx.Blob != "0102" ||
		len(tx.SpentKeyImages) != 1 || tx.SpentKeyImages[0] != hex64("ki1") || len(tx.AmountsByDest) != 2 || tx.AmountsByDest[0] != 870 || tx.AmountsByDest[1] != 50 {
		t.Fatalf("transfer: %+v %v", tx, err)
	}
	if h, err := r.relayTx(ctx, "0a0b0c"); err != nil || h != hex64("settle") {
		t.Fatalf("relay: %s %v", h, err)
	}
	if got, found, err := r.transferByTxid(ctx, 3, hex64("settle")); err != nil || !found || len(got) != 1 || got[0].Type != "out" || got[0].Confirmations != 2 || got[0].Account != 3 {
		t.Fatalf("by txid: %+v %v %v", got, found, err)
	}
	wallet.set("get_transfer_by_txid", func(json.RawMessage) (any, int, string) { return nil, -8, "Transaction not found." })
	if _, found, err := r.transferByTxid(ctx, 3, hex64("settle")); err != nil || found {
		t.Fatalf("unknown txid: %v %v", found, err)
	}
	// QA-XB-3: only -8 means "the wallet does not know it". Any other failure is an error, never (nil, false, nil):
	// X-d's ambiguous-relay decision must not read a failed lookup as an absent transaction.
	for _, code := range []int{-13, -1, -4} {
		wallet.set("get_transfer_by_txid", func(json.RawMessage) (any, int, string) { return nil, code, "failure" })
		var re *xmrRPCError
		if got, found, err := r.transferByTxid(ctx, 3, hex64("settle")); !errors.As(err, &re) || re.Code != int64(code) || found || got != nil {
			t.Fatalf("code %d: %v %v %v", code, got, found, err)
		}
	}
	wallet.set("get_transfer_by_txid", func(json.RawMessage) (any, int, string) { return "not an object", 0, "" })
	if _, found, err := r.transferByTxid(ctx, 3, hex64("settle")); err == nil || found {
		t.Fatalf("bad reply: %v %v", found, err)
	}
	func() {
		old := rpcTimeout
		rpcTimeout = 100 * time.Millisecond
		defer func() { rpcTimeout = old }()
		wallet.setDelay("get_transfer_by_txid", 400*time.Millisecond)
		defer wallet.setDelay("get_transfer_by_txid", 0)
		var re *xmrRPCError
		if _, found, err := r.transferByTxid(ctx, 3, hex64("settle")); !errors.As(err, &re) || re.Class != "timeout" || found {
			t.Fatalf("timeout: %v %v", found, err)
		}
		down := newFakeXMRNode(t)
		gone, err := newXMREscrowRPC(down.url(), daemon.url())
		if err != nil {
			t.Fatal(err)
		}
		down.Close()
		if _, found, err := gone.transferByTxid(ctx, 3, hex64("settle")); !errors.As(err, &re) || re.Class != "unreachable" || found {
			t.Fatalf("unreachable: %v %v", found, err)
		}
	}()
	txs, missed, err := r.transactions(ctx, []string{hex64("settle"), hex64("gone")})
	if err != nil || len(txs) != 1 || len(missed) != 1 || missed[0] != hex64("gone") {
		t.Fatalf("get_transactions: %+v %v %v", txs, missed, err)
	}
	if d := txs[0]; d.Hash != hex64("settle") || d.InPool || d.BlockHeight != 110 || d.Confirmations != 11 || d.RCTType != 6 ||
		len(d.RingSizes) != 2 || d.RingSizes[0] != 16 || d.RingSizes[1] != 16 || len(d.KeyImages) != 2 || d.KeyImages[1] != hex64("ki2") {
		t.Fatalf("decoded tx: %+v", d)
	}
	if s, err := r.keyImagesSpent(ctx, []string{hex64("a"), hex64("b"), hex64("c")}); err != nil || len(s) != 3 || s[0] != 0 || s[1] != 1 || s[2] != 2 {
		t.Fatalf("is_key_image_spent: %v %v", s, err)
	}
	// The restore sweep's probe (P3-R10): open_wallet's -1 for a file that does not exist is no wallet there; any other
	// refusal (a wrong password, monero-wallet-rpc 0.18.5.1's words) is an error with its class, never the message.
	if found, err := r.probeWallet(ctx, "escrow-202610-02", "pw"); err != nil || !found {
		t.Fatalf("probe of a wallet that opens: %v %v", found, err)
	}
	wallet.set("open_wallet", func(json.RawMessage) (any, int, string) {
		return nil, -1, `Failed to open wallet : file not found "/home/monero/wallet/escrow-202610-02.keys"`
	})
	if found, err := r.probeWallet(ctx, "escrow-202610-02", "pw"); err != nil || found {
		t.Fatalf("probe of no file: %v %v", found, err)
	}
	wallet.set("open_wallet", func(json.RawMessage) (any, int, string) { return nil, -1, "Failed to open wallet : Invalid password." })
	var refused *xmrRPCError
	if found, err := r.probeWallet(ctx, "escrow-202610-02", "pw"); !errors.As(err, &refused) || refused.Class != "rpc:-1" || refused.Refusal != xmrOpenPassword || found {
		t.Fatalf("probe of another wallet's file: %v %v", found, err)
	}
	// open_wallet's refusals as monero-wallet-rpc 0.18.5.1 words them, each to its fixed class (A-419, A-421); the message
	// itself is never kept. Only wallet2's invalid_password is a wrong password.
	for msg, want := range map[string]string{
		`Failed to open wallet : file not found "/home/monero/wallet/escrow-202610-01.keys"`:                                      xmrOpenNoFile,
		`Failed to open wallet : internal error: "/home/monero/wallet/escrow-202610-01.keys" is opened by another wallet program`: xmrOpenLocked,
		"Failed to open wallet : Invalid password.":                                                                               xmrOpenPassword,
		"Failed to open wallet : invalid password":                                                                                "",
		"Failed to open wallet : invalid password.":                                                                               "",
		"Failed to open wallet : Invalid password. Retry with the background wallet":                                              "",
		"Failed to open wallet : std::bad_alloc":                                                                                  "",
	} {
		wallet.set("open_wallet", func(json.RawMessage) (any, int, string) { return nil, -1, msg })
		err := r.openWallet(ctx, "escrow-202610-01", "pw")
		if !errors.As(err, &refused) || refused.Refusal != want || xmrOpenRefusal(err) != want || strings.Contains(err.Error(), "Failed") || strings.Contains(err.Error(), "/home") {
			t.Errorf("open_wallet refused with %q: %v, want %q", msg, err, want)
		}
	}
	wallet.set("open_wallet", func(json.RawMessage) (any, int, string) { return nil, -13, "Failed to open wallet : Invalid password." })
	if err := r.openWallet(ctx, "escrow-202610-01", "pw"); xmrOpenRefusal(err) != "" {
		t.Errorf("a refusal other than -1 was classified: %v", err)
	}
	wallet.result("get_transfers", map[string]any{})
	if busy, err := r.inPool(ctx); err != nil || busy {
		t.Fatalf("an empty pool: %v %v", busy, err)
	}
	wallet.result("get_transfers", map[string]any{"pending": []any{map[string]any{"txid": hex64("out"), "type": "pending"}}})
	if busy, err := r.inPool(ctx); err != nil || !busy {
		t.Fatalf("a pending transfer: %v %v", busy, err)
	}

	// Replies the adapter refuses: each becomes a bad-reply class, never the reply's text.
	bad := []struct {
		name string
		node *fakeXMRNode
		key  string
		v    any
		call func() error
	}{
		{"mnemonic of 12 words", wallet, "query_key", map[string]any{"key": "abbey abbey abbey abbey abbey abbey abbey abbey abbey abbey abbey abbey"}, func() error { _, err := r.mnemonic(ctx); return err }},
		{"account 0 created", wallet, "create_account", map[string]any{"account_index": 0, "address": testXMRAddr("x", true)}, func() error { _, _, err := r.createAccount(ctx); return err }},
		{"account address not a subaddress", wallet, "create_account", map[string]any{"account_index": 4, "address": testXMRAddr("x", false)}, func() error { _, _, err := r.createAccount(ctx); return err }},
		{"stagenet primary address", wallet, "get_address", map[string]any{"address": testXMRAddressWith(24, testXMRKey("s"), testXMRKey("v"), nil, false), "addresses": []any{map[string]any{"address": testXMRAddressWith(24, testXMRKey("s"), testXMRKey("v"), nil, false), "address_index": 0}}}, func() error { _, err := r.primaryAddress(ctx); return err }},
		{"output of another account", wallet, "incoming_transfers", map[string]any{"transfers": []any{map[string]any{"amount": 1, "key_image": hex64("k"), "tx_hash": hex64("t"), "subaddr_index": map[string]any{"major": 9, "minor": 0}}}}, func() error { _, err := r.incomingTransfers(ctx, 3); return err }},
		{"output without key image", wallet, "incoming_transfers", map[string]any{"transfers": []any{map[string]any{"amount": 1, "key_image": "", "tx_hash": hex64("t"), "subaddr_index": map[string]any{"major": 3, "minor": 0}}}}, func() error { _, err := r.incomingTransfers(ctx, 3); return err }},
		{"transfer without metadata", wallet, "transfer", map[string]any{"tx_hash": hex64("s"), "fee": 30, "amount": 970, "tx_metadata": "", "tx_blob": "01", "spent_key_images": map[string]any{"key_images": []string{hex64("ki1")}}, "amounts_by_dest": map[string]any{"amounts": []int{870}}}, func() error {
			_, err := r.buildTransfer(ctx, 3, []xmrDestination{{claimant, 900}})
			return err
		}},
		{"transfer amounts not one per destination", wallet, "transfer", map[string]any{"tx_hash": hex64("s"), "fee": 30, "amount": 970, "tx_metadata": "0a", "tx_blob": "01", "spent_key_images": map[string]any{"key_images": []string{hex64("ki1")}}, "amounts_by_dest": map[string]any{"amounts": []int{870, 50}}}, func() error {
			_, err := r.buildTransfer(ctx, 3, []xmrDestination{{claimant, 900}})
			return err
		}},
		{"transfer spending no key image", wallet, "transfer", map[string]any{"tx_hash": hex64("s"), "fee": 30, "amount": 970, "tx_metadata": "0a", "tx_blob": "01", "spent_key_images": map[string]any{"key_images": []string{}}, "amounts_by_dest": map[string]any{"amounts": []int{870}}}, func() error {
			_, err := r.buildTransfer(ctx, 3, []xmrDestination{{claimant, 900}})
			return err
		}},
		{"relay without a hash", wallet, "relay_tx", map[string]any{"tx_hash": ""}, func() error { _, err := r.relayTx(ctx, "0a"); return err }},
		{"daemon status not OK", daemon, "/get_transactions", map[string]any{"status": "Failed", "txs": []any{}}, func() error { _, _, err := r.transactions(ctx, []string{hex64("x")}); return err }},
		{"undecodable as_json", daemon, "/get_transactions", map[string]any{"status": "OK", "txs": []any{map[string]any{"tx_hash": hex64("x"), "as_json": "{"}}}, func() error { _, _, err := r.transactions(ctx, []string{hex64("x")}); return err }},
		{"spent_status count", daemon, "/is_key_image_spent", map[string]any{"status": "OK", "spent_status": []int{0}}, func() error { _, err := r.keyImagesSpent(ctx, []string{hex64("a"), hex64("b")}); return err }},
		{"spent_status 3", daemon, "/is_key_image_spent", map[string]any{"status": "OK", "spent_status": []int{0, 3}}, func() error { _, err := r.keyImagesSpent(ctx, []string{hex64("a"), hex64("b")}); return err }},
		{"spent_status negative", daemon, "/is_key_image_spent", map[string]any{"status": "OK", "spent_status": []int{-1}}, func() error { _, err := r.keyImagesSpent(ctx, []string{hex64("a")}); return err }},
	}
	for _, b := range bad {
		b.node.result(b.key, b.v)
		err := b.call()
		var re *xmrRPCError
		if !errors.As(err, &re) || re.Class != "bad-reply" || strings.Contains(err.Error(), "abbey") {
			t.Errorf("%s: %v", b.name, err)
		}
	}
}

// TestXMREscrowAccountZeroRefusedOffline: transfer, incoming_transfers, get_address and get_transfer_by_txid refuse
// account 0 (and indices that are no account) before any request; so do malformed wallet names, destinations and
// key images. get_address reaches account 0 only through primaryAddress.
func TestXMREscrowAccountZeroRefusedOffline(t *testing.T) {
	ctx := context.Background()
	wallet, daemon, r := happyXMRNodes(t)
	claimant := testXMRAddr("claimant", true)
	ok := []xmrDestination{{claimant, 900}}
	for _, account := range []int64{0, -1, 1 << 32} {
		for name, call := range map[string]func() error{
			"transfer":             func() error { _, err := r.buildTransfer(ctx, account, ok); return err },
			"incoming_transfers":   func() error { _, err := r.incomingTransfers(ctx, account); return err },
			"get_address":          func() error { _, err := r.address(ctx, account, 0); return err },
			"get_transfer_by_txid": func() error { _, _, err := r.transferByTxid(ctx, account, hex64("t")); return err },
		} {
			if err := call(); !errors.Is(err, errXMRAccountIndex) {
				t.Errorf("%s account %d: %v", name, account, err)
			}
		}
	}
	for name, call := range map[string]func() error{
		"no destinations": func() error { _, err := r.buildTransfer(ctx, 3, nil); return err },
		"zero amount":     func() error { _, err := r.buildTransfer(ctx, 3, []xmrDestination{{claimant, 0}}); return err },
		"stagenet destination": func() error {
			_, err := r.buildTransfer(ctx, 3, []xmrDestination{{testXMRAddressWith(36, testXMRKey("s"), testXMRKey("v"), nil, false), 5}})
			return err
		},
		"integrated destination": func() error {
			_, err := r.buildTransfer(ctx, 3, []xmrDestination{{testXMRAddressWith(19, testXMRKey("s"), testXMRKey("v"), nil, false), 5}})
			return err
		},
		"pooled wallet name":  func() error { return r.openWallet(ctx, "opsecmkt", "") },
		"path in wallet name": func() error { return r.createWallet(ctx, "../escrow-202610-01", "pw") },
		"short key image":     func() error { return r.freeze(ctx, "abcd") },
		"non-hex key image":   func() error { return r.thaw(ctx, strings.Repeat("zz", 32)) },
		"bad txid":            func() error { _, _, err := r.transferByTxid(ctx, 3, "x"); return err },
		"empty metadata":      func() error { _, err := r.relayTx(ctx, ""); return err },
		"no lookahead":        func() error { return r.setSubaddressLookahead(ctx, "pw", 0, 0) },
		"probe pooled wallet": func() error { _, err := r.probeWallet(ctx, "opsecmkt", ""); return err },
		"bad tx hash":         func() error { _, _, err := r.transactions(ctx, []string{"x"}); return err },
		"no key images":       func() error { _, err := r.keyImagesSpent(ctx, nil); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := wallet.count() + daemon.count(); n != 0 {
		t.Fatalf("refused calls made %d requests", n)
	}
}

// TestXMREscrowNetworkCheck: every P3-R2 refusal has its fixed error, and the accepted edges pass. With no wallet
// open (-13), only the daemon is checked.
func TestXMREscrowNetworkCheck(t *testing.T) {
	ctx := context.Background()
	info := func(nettype string, mainnet bool, height int64) map[string]any {
		return map[string]any{"nettype": nettype, "mainnet": mainnet, "stagenet": nettype == "stagenet", "testnet": nettype == "testnet", "height": height, "status": "OK"}
	}
	cases := []struct {
		name         string
		info         map[string]any
		hardFork     int64
		walletHeight int64
		want         error
	}{
		{"fakechain", info("fakechain", false, 120), 16, 120, nil},
		{"wallet 10 behind", info("fakechain", false, 120), 16, 110, nil},
		{"wallet 10 ahead", info("fakechain", false, 120), 16, 130, nil},
		{"hard fork 17", info("fakechain", false, 120), 17, 120, nil},
		{"height 999,999", info("fakechain", false, 999_999), 16, 999_999, nil},
		{"mainnet", info("mainnet", true, 120), 16, 120, errXMRNotFakechain},
		{"stagenet", info("stagenet", false, 120), 16, 120, errXMRNotFakechain},
		{"testnet", info("testnet", false, 120), 16, 120, errXMRNotFakechain},
		{"empty nettype", info("", false, 120), 16, 120, errXMRNotFakechain},
		{"fakechain with the mainnet flag", info("fakechain", true, 120), 16, 120, errXMRNotFakechain},
		{"fakechain with the stagenet flag", map[string]any{"nettype": "fakechain", "stagenet": true, "height": 120}, 16, 120, errXMRNotFakechain},
		{"fakechain with the testnet flag", map[string]any{"nettype": "fakechain", "testnet": true, "height": 120}, 16, 120, errXMRNotFakechain},
		{"hard fork 15", info("fakechain", false, 120), 15, 120, errXMRHardFork},
		{"daemon height 1,000,000", info("fakechain", false, 1_000_000), 16, 1_000_000, errXMRDaemonHeight},
		{"wallet height 1,000,000", info("fakechain", false, 999_995), 16, 1_000_000, errXMRWalletHeight},
		{"wallet 11 behind", info("fakechain", false, 120), 16, 109, errXMRWalletLag},
		{"wallet 11 ahead", info("fakechain", false, 120), 16, 131, errXMRWalletLag},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wallet, daemon, r := happyXMRNodes(t)
			daemon.result("get_info", c.info)
			daemon.result("hard_fork_info", map[string]any{"version": c.hardFork, "enabled": true})
			wallet.result("get_height", map[string]any{"height": c.walletHeight})
			open, err := r.checkNetwork(ctx)
			if !errors.Is(err, c.want) || (err == nil && !open) {
				t.Fatalf("got %v (open %v), want %v", err, open, c.want)
			}
			if err != nil && (strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "s3cret")) {
				t.Fatalf("refusal names the node: %v", err)
			}
		})
	}
	// The wallet is refreshed before its height is read.
	wallet, _, r := happyXMRNodes(t)
	if _, err := r.checkNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if order := strings.Join(wallet.methods(), ","); order != "refresh,get_height" {
		t.Fatalf("wallet calls %v", order)
	}
	// No wallet open: the daemon passes, the wallet is not checked, and that is not an error.
	wallet, _, r = happyXMRNodes(t)
	wallet.set("refresh", func(json.RawMessage) (any, int, string) { return nil, -13, "No wallet file" })
	if open, err := r.checkNetwork(ctx); err != nil || open {
		t.Fatalf("no wallet open: %v %v", open, err)
	}
	if wallet.count() != 1 {
		t.Fatalf("wallet requests after -13: %d", wallet.count())
	}
}

// TestXMREscrowRPCErrorClasses: failures become fixed classes that never carry the node's message, its URL or the
// credentials in it; a connection that ends after the whole request was sent (a wallet-rpc that crashed while it
// answered) is marked Dropped, a connection refused or a timeout is not.
func TestXMREscrowRPCErrorClasses(t *testing.T) {
	ctx := context.Background()
	old := rpcTimeout
	rpcTimeout = 200 * time.Millisecond
	t.Cleanup(func() { rpcTimeout = old })
	wallet, daemon, r := happyXMRNodes(t)
	secretText := "Cannot open wallet escrow-202610-01 with password e9c16b29 at /wallet/escrow"
	wallet.set("get_height", func(json.RawMessage) (any, int, string) { return nil, -1, secretText })
	wallet.set("get_accounts", func(json.RawMessage) (any, int, string) { return "not an object", 0, "" })
	daemon.setDelay("get_info", time.Second)
	digest := newPayRPCServer(t, "escrowuser", "s3cret-escrow-pass", true, func(string, string, json.RawMessage) (any, int, string) { return map[string]any{}, 0, "" })
	wrong, err := newXMREscrowRPC(strings.Replace(digest.url(""), "s3cret-escrow-pass", "wrong-escrow-pass", 1), daemon.url())
	if err != nil {
		t.Fatal(err)
	}
	down := newFakeXMRNode(t)
	gone, err := newXMREscrowRPC(down.url(), daemon.url())
	if err != nil {
		t.Fatal(err)
	}
	down.Close()
	// A wallet-rpc that goes away while it answers (it crashed building a transaction): it reads the whole request, then
	// closes the connection with no answer, or after the start of one.
	// stall: the start of an answer, then nothing until the client's timeout (not a drop).
	crash := func(partial, stall bool) *xmrEscrowRPC {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			io.Copy(io.Discard, req.Body)
			if partial {
				w.Header().Set("Content-Length", "4096")
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, `{"jsonrpc":"2.0","id":"0","result":{"hei`)
				w.(http.Flusher).Flush()
			}
			if stall {
				time.Sleep(time.Second)
			}
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
		}))
		t.Cleanup(srv.Close)
		c, err := newXMREscrowRPC(srv.URL, daemon.url())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	dropped, cut, stalled := crash(false, false), crash(true, false), crash(true, true)
	cases := []struct {
		name, class string
		dropped     bool // the connection ended after the whole request was sent (xmrRPCError.Dropped)
		call        func() error
	}{
		{"rpc error", "rpc:-1", false, func() error { _, err := r.height(ctx); return err }},
		{"bad reply", "bad-reply", false, func() error { _, err := r.accounts(ctx); return err }},
		{"timeout", "timeout", false, func() error { _, err := r.daemonInfo(ctx); return err }},
		{"credentials", "credentials", false, func() error { _, err := wrong.height(ctx); return err }},
		{"unreachable", "unreachable", false, func() error { _, err := gone.height(ctx); return err }},
		{"dropped", "unreachable", true, func() error { _, err := dropped.height(ctx); return err }},
		{"cut mid-answer", "bad-reply", true, func() error { _, err := cut.height(ctx); return err }},
		{"timeout mid-answer", "bad-reply", false, func() error { _, err := stalled.height(ctx); return err }},
	}
	for _, c := range cases {
		err := c.call()
		var re *xmrRPCError
		if !errors.As(err, &re) || re.Class != c.class || re.Dropped != c.dropped {
			t.Errorf("%s: got %v (dropped %v), want class %s, dropped %v", c.name, err, re != nil && re.Dropped, c.class, c.dropped)
			continue
		}
		for _, leak := range []string{"127.0.0.1", "s3cret", "wrong-escrow", "escrowuser", "password", "e9c16b29", "/wallet"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: error carries %q: %v", c.name, leak, err)
			}
		}
		if got := xmrErrorClass(err); got != re.Method+" "+re.Class {
			t.Errorf("%s: class line %q", c.name, got)
		}
	}
}

// TestXMREscrowSlowCallsGetLongerTimeout: open (and the restore sweep's probe), create, close, store,
// change_wallet_password and rescan_blockchain run on xmrWalletSlowTimeout, and refresh on the longer
// xmrWalletRefreshTimeout, not rpcTimeout; an ordinary read still times out at rpcTimeout.
func TestXMREscrowSlowCallsGetLongerTimeout(t *testing.T) {
	if xmrWalletSlowTimeout != 2*time.Minute || xmrWalletSlowTimeout <= rpcTimeout {
		t.Fatalf("slow timeout %s", xmrWalletSlowTimeout)
	}
	ctx := context.Background()
	old := rpcTimeout
	rpcTimeout = 100 * time.Millisecond
	t.Cleanup(func() { rpcTimeout = old })
	wallet, _, r := happyXMRNodes(t)
	for _, m := range []string{"create_wallet", "open_wallet", "close_wallet", "refresh", "store", "change_wallet_password", "rescan_blockchain", "get_height"} {
		wallet.setDelay(m, 250*time.Millisecond)
	}
	for name, call := range map[string]func() error{
		"create_wallet":          func() error { return r.createWallet(ctx, "escrow-202610-01", "pw") },
		"open_wallet":            func() error { return r.openWallet(ctx, "escrow-202610-01", "pw") },
		"close_wallet":           func() error { return r.closeWallet(ctx) },
		"refresh":                func() error { return r.refresh(ctx) },
		"store":                  func() error { return r.store(ctx) },
		"change_wallet_password": func() error { return r.changeWalletPassword(ctx, "old", "new") },
		"rescan_blockchain":      func() error { return r.rescan(ctx) },
		"probe (open_wallet)":    func() error { _, err := r.probeWallet(ctx, "escrow-202610-01", "pw"); return err },
	} {
		if err := call(); err != nil {
			t.Errorf("%s cut short: %v", name, err)
		}
	}
	var re *xmrRPCError
	if _, err := r.height(ctx); !errors.As(err, &re) || re.Class != "timeout" {
		t.Fatalf("get_height: %v", err)
	}
}

// TestXMREscrowRefreshOutlastsSlowTimeout (A-429): refresh runs on its own bound, xmrWalletRefreshTimeout, longer than
// xmrWalletSlowTimeout (PAYE-E2 measured a 2.5-minute catch-up refresh) and short enough that an open_wallet and the
// refresh fit one pass. The network check's refresh (the watcher's, after each open_wallet) runs on it too. With both
// bounds shortened: a refresh that answers after the slow bound is waited for; one past its own bound times out.
func TestXMREscrowRefreshOutlastsSlowTimeout(t *testing.T) {
	if xmrWalletRefreshTimeout != 8*time.Minute || xmrWalletRefreshTimeout+xmrWalletSlowTimeout > xmrEscrowPassTimeout {
		t.Fatalf("refresh timeout %s (slow %s, pass %s)", xmrWalletRefreshTimeout, xmrWalletSlowTimeout, xmrEscrowPassTimeout)
	}
	ctx := context.Background()
	oldSlow, oldRefresh := xmrWalletSlowTimeout, xmrWalletRefreshTimeout
	xmrWalletSlowTimeout, xmrWalletRefreshTimeout = 100*time.Millisecond, time.Second
	t.Cleanup(func() { xmrWalletSlowTimeout, xmrWalletRefreshTimeout = oldSlow, oldRefresh })
	wallet, _, r := happyXMRNodes(t)
	wallet.setDelay("refresh", 300*time.Millisecond)
	wallet.setDelay("open_wallet", 300*time.Millisecond)
	if err := r.refresh(ctx); err != nil {
		t.Fatalf("a refresh longer than the slow bound was cut short: %v", err)
	}
	if _, err := r.checkNetwork(ctx); err != nil {
		t.Fatalf("the network check's refresh was cut short: %v", err)
	}
	var re *xmrRPCError
	if err := r.openWallet(ctx, "escrow-202610-01", "pw"); !errors.As(err, &re) || re.Class != "timeout" {
		t.Fatalf("open_wallet past the slow bound: %v", err)
	}
	wallet.setDelay("refresh", 1500*time.Millisecond)
	if err := r.refresh(ctx); !errors.As(err, &re) || re.Method != "refresh" || re.Class != "timeout" {
		t.Fatalf("a refresh past its own bound: %v", err)
	}
}
