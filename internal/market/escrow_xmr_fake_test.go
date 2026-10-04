package market

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeXMREscrow is a monero-wallet-rpc and a monerod for Monero escrow DB tests (simulated RPC, not chain evidence). It
// keeps the semantics the escrow watcher depends on: several wallet files, each with its own password, of which one at
// a time is open; create_wallet refuses an existing file (-21) and leaves the new wallet open; every call on a wallet
// answers -13 while none is open; each wallet has a 25-word seed (query_key), a primary address (account 0) and
// accounts added by create_account, each with its address 0 (a subaddress); and get_transfers lists each wallet's
// incoming transfers, one entry per transaction and subaddress, with their confirmations, unlock time, pool state and
// double_spend_seen. The daemon is a fakechain at a height the test moves.
//
// Retention and restore (slice X-f). change_wallet_password re-encrypts a wallet (open_wallet then needs the new
// password; a wrong old password is -22), get_balance sums every account, and get_transfers lists the transactions the
// open wallet built that are in the pool (pending) or mined (out) when asked. open_wallet answers -1 for a file that does
// not exist, one another wallet program has open (locked) or a wrong password, with monero-wallet-rpc's message for each. A wallet knows its accounts plus a lookahead
// of 50 more (set_subaddress_lookahead, password checked, changes it): refresh scans each transfer to an account the
// wallet does not know once, and sees it only when it is within the lookahead (the wallet then knows every account up to
// it); one scanned outside it is missed until rescan_blockchain scans again with the lookahead in force then.
//
// Settlements (slice X-d). Each incoming transfer entry is one output with a key image, a frozen flag and an unlock
// state (unlocked from 10 confirmations with no unlock time), listed by incoming_transfers until it is spent; freeze
// and thaw set the flag. transfer from one account spends unfrozen unlocked outputs, newest first, until the
// destinations are covered (-17 not enough money otherwise), takes the network fee (fakeXMRFee) from destination 0
// alone when subtract_fee_from_outputs is [0] (an address in invalid is refused, -2, as validate_address says; a
// transfer to one in crash crashes the wallet-rpc, fakeXMRCrash), and with do_not_relay only builds: the transaction is
// kept by its metadata and nothing is spent. relay_tx puts a built transaction in the daemon's pool and marks its
// outputs spent; relaying it again answers the same hash (idempotent, as monerod 0.18.5.1 does), and one whose key
// images another transaction spent is refused (-4, rejected by the daemon). The daemon's pool and chain answer
// /get_transactions (missed_tx for an unknown hash; decoded as ring 16, rct type 6) and /is_key_image_spent (1 on
// chain, 2 in the pool); mine, popBlocks (a reorganisation: the popped blocks' transactions go back to the pool),
// flushPool and spendOutside (a wallet restored from the seed spends outputs outside the market) move them.
// get_transfer_by_txid knows the transactions the open wallet relayed (-8 otherwise).
//
// Faults: a call can be made to take effect and then lose its answer (the connection closes, as a timeout after the
// wallet acted does), to answer -13 (the wallet-rpc lost its open wallet), to be refused by the daemon (-4, relay_tx)
// or, for open_wallet, to open another wallet file than the one asked for; the daemon's /get_transactions and
// /is_key_image_spent can lose their answer too. transferLie edits transfer's reply before it is sent (a lying wallet).
// A hook runs before a method's effect, so a test can change the database mid-pass. Every request is recorded.
type fakeXMREscrow struct {
	t              *testing.T
	wallet, daemon *httptest.Server

	mu        sync.Mutex
	height    int64 // the daemon's height; the open wallet's too after a refresh, plus walletLag
	walletLag int64
	files     map[string]*fakeXMRWalletFile
	open      *fakeXMRWalletFile // nil while no wallet is open
	calls     []fakeXMRCall
	faults    map[string][]fakeXMRFault
	hooks     map[string]func()
	handlers  map[string]func(f *fakeXMREscrow, params json.RawMessage) (any, int64, string) // wallet methods, by name

	txs         map[string]*fakeXMRTx // transactions the daemon knows, by hash: in the pool (height 0) or mined
	built       map[string]*fakeXMRTx // transactions transfer built, by metadata
	seq         int                   // numbers built transactions and key images
	transferLie func(res map[string]any)
	invalid     map[string]bool // addresses whose keys are not curve points: transfer refuses them (-2), validate_address says invalid
	crash       map[string]bool // addresses a transfer to which crashes the wallet-rpc (fakeXMRCrash)
}

// fakeXMRTx is a settlement transaction transfer built, or one spendOutside made.
type fakeXMRTx struct {
	Hash, Metadata, Blob string
	Wallet               string // the wallet file that built it ("" for an outside spend)
	Account              int64
	KeyImages            []string
	Fee                  int64
	Height               int64 // the block it was mined in; 0 while in the pool
}

// fakeXMRFee is the network fee of a transaction with nIn inputs and nOut outputs (change included): about what a young
// fakechain charges (0.0018744 XMR for one input and two outputs, chair probe on monerod 0.18.5.1).
func fakeXMRFee(nIn, nOut int) int64 {
	return 1874400000 + 300000000*int64(nIn-1) + 100000000*int64(max(nOut-2, 0))
}

// fakeXMRWalletFile is one wallet file of the fake wallet-rpc.
type fakeXMRWalletFile struct {
	name, password, mnemonic string
	accounts                 []string // each account's address 0; [0] is the primary address
	transfers                []*fakeXMRTransfer
	lookahead                int64 // accounts scanned beyond the last one the wallet knows (set_subaddress_lookahead's major)
	locked                   bool  // another wallet program has the file open: open_wallet refuses it
}

// visible: the wallet sees transfer t: to an account it knows, or seen by a scan within the lookahead.
func (w *fakeXMRWalletFile) visible(t *fakeXMRTransfer) bool {
	return t.Account < int64(len(w.accounts)) || t.seen
}

// scan is a refresh (again false: each transfer to an unknown account is scanned once) or a rescan_blockchain (again
// true): a transfer within the lookahead is seen, and the wallet then knows every account up to its own.
func (w *fakeXMRWalletFile) scan(again bool) {
	for _, t := range w.transfers {
		if w.visible(t) || t.scanned && !again {
			continue
		}
		t.scanned = true
		if t.Account < int64(len(w.accounts))-1+w.lookahead {
			t.seen = true
			for n := len(w.accounts); int64(n) <= t.Account; n++ {
				w.accounts = append(w.accounts, testXMRAddr("fake "+w.name+" account "+strconv.Itoa(n), true))
			}
		}
	}
}

// fakeXMRTransfer is one incoming transfer entry: a transaction's outputs to one subaddress, as get_transfers lists it.
type fakeXMRTransfer struct {
	TxID           string
	Account, Minor int64
	Amount         int64
	Confs          int64 // 0 while in the pool
	Pool           bool
	UnlockTime     int64
	DoubleSpend    bool
	KeyImage       string
	Frozen         bool
	SpentBy        string // the transaction that spent it ("" while unspent)
	scanned, seen  bool   // to an account the wallet did not know: scanned by a refresh, and seen by it (scan)
}

// unlocked: spendable, as the wallet's 10-block lock and the unlock time decide.
func (t *fakeXMRTransfer) unlocked() bool {
	return !t.Pool && !t.DoubleSpend && t.Confs >= 10 && t.UnlockTime == 0
}

type fakeXMRFault int

// fakeXMRCrash is the code a handler returns for a wallet-rpc that crashed handling the call (monero-wallet-rpc
// 0.18.5.1 building a transfer to some checksum-valid addresses whose keys are not usable): the connection closes with
// no answer and the wallet-rpc comes back with no wallet open.
const fakeXMRCrash = -1 << 40

const (
	fakeXMRDrop        fakeXMRFault = iota + 1 // the call takes effect, then the connection closes with no answer
	fakeXMRNotOpen                             // the call answers -13: no wallet open
	fakeXMRWrongWallet                         // open_wallet opens another wallet file than the one asked for
	fakeXMRRejected                            // the call answers -4: the daemon rejected the transaction (relay_tx)
)

func newFakeXMREscrow(t *testing.T) *fakeXMREscrow {
	t.Helper()
	f := &fakeXMREscrow{t: t, height: 120, files: map[string]*fakeXMRWalletFile{}, faults: map[string][]fakeXMRFault{}, hooks: map[string]func(){},
		handlers: map[string]func(*fakeXMREscrow, json.RawMessage) (any, int64, string){}, txs: map[string]*fakeXMRTx{}, built: map[string]*fakeXMRTx{}, invalid: map[string]bool{}, crash: map[string]bool{}}
	for name, h := range fakeXMRWalletMethods {
		f.handlers[name] = h
	}
	f.wallet = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(w, r, true) }))
	f.daemon = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(w, r, false) }))
	t.Cleanup(f.wallet.Close)
	t.Cleanup(f.daemon.Close)
	return f
}

// setEnv points ESCROW_XMR at the fake, with the seed key file at keyPath, five slots and no market fee.
func (f *fakeXMREscrow) setEnv(t *testing.T, keyPath string) {
	t.Helper()
	for k, v := range map[string]string{"ESCROW_XMR": "on", "ESCROW_XMR_WALLET_RPC_URL": f.wallet.URL, "ESCROW_XMR_DAEMON_URL": f.daemon.URL,
		"MONERO_WALLET_SECRET": testXMRSecret, "ESCROW_XMR_SEED_KEY": keyPath, "ESCROW_XMR_FEE_BPS": "", "ESCROW_XMR_FEE_ADDRESS": "",
		"ESCROW_XMR_SLOTS": "5", "ESCROW_XMR_MAX_FEE": "", "MONERO_WALLET_RPC_URL": "", "MONERO_NETWORK": "",
		"ESCROW_BTC": "", "BITCOIN_RPC_URL": "", "BITCOIN_CHAIN": ""} {
		t.Setenv(k, v)
	}
}

func (f *fakeXMREscrow) serve(w http.ResponseWriter, r *http.Request, wallet bool) {
	body, _ := io.ReadAll(r.Body)
	if !wallet && r.URL.Path != "/json_rpc" {
		f.serveDaemonOther(w, r.URL.Path, body)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if r.URL.Path != "/json_rpc" || json.Unmarshal(body, &req) != nil {
		w.WriteHeader(404)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeXMRCall{r.URL.Path, req.Method, string(req.Params)})
	var fault fakeXMRFault
	if q := f.faults[req.Method]; wallet && len(q) > 0 {
		fault, f.faults[req.Method] = q[0], q[1:]
	}
	for key, q := range f.faults {
		method, param, ok := strings.Cut(key, "\x00")
		if ok && fault == 0 && wallet && method == req.Method && strings.Contains(string(req.Params), param) && len(q) > 0 {
			fault, f.faults[key] = q[0], q[1:]
		}
	}
	hook := f.hooks[req.Method]
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	var result any
	var code int64
	var msg string
	switch {
	case !wallet:
		result, code, msg = f.daemonMethod(req.Method)
	case fault == fakeXMRNotOpen:
		code, msg = -13, "No wallet file"
	case fault == fakeXMRRejected:
		code, msg = -4, "transaction was rejected by daemon"
	case fault == fakeXMRWrongWallet && req.Method == "open_wallet":
		for _, name := range slices.Sorted(maps.Keys(f.files)) {
			if f.open == nil || name != f.open.name {
				f.open = f.files[name]
				break
			}
		}
		result = map[string]any{}
	default:
		h := f.handlers[req.Method]
		switch {
		case h == nil:
			code, msg = -32601, "Method not found"
		case f.open == nil && !slices.Contains([]string{"create_wallet", "open_wallet", "close_wallet"}, req.Method):
			code, msg = -13, "No wallet file"
		default:
			result, code, msg = h(f, req.Params)
		}
	}
	f.mu.Unlock()
	if fault == fakeXMRDrop || code == fakeXMRCrash {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
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

func (f *fakeXMREscrow) daemonMethod(method string) (any, int64, string) {
	switch method {
	case "get_info":
		return map[string]any{"nettype": "fakechain", "mainnet": false, "stagenet": false, "testnet": false, "height": f.height, "status": "OK"}, 0, ""
	case "hard_fork_info":
		return map[string]any{"version": 16, "enabled": true, "status": "OK"}, 0, ""
	}
	return nil, -32601, "Method not found"
}

// serveDaemonOther answers monerod's plain JSON endpoints /get_transactions and /is_key_image_spent.
func (f *fakeXMREscrow) serveDaemonOther(w http.ResponseWriter, path string, body []byte) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeXMRCall{path, path, string(body)})
	var fault fakeXMRFault
	if q := f.faults[path]; len(q) > 0 {
		fault, f.faults[path] = q[0], q[1:]
	}
	var res map[string]any
	switch path {
	case "/get_transactions":
		var q struct {
			Hashes []string `json:"txs_hashes"`
		}
		json.Unmarshal(body, &q)
		txs, missed := []any{}, []string{}
		for _, h := range q.Hashes {
			tx := f.txs[h]
			if tx == nil {
				missed = append(missed, h)
				continue
			}
			e := map[string]any{"tx_hash": h, "in_pool": tx.Height == 0, "double_spend_seen": false, "as_json": testXMRTxJSON(tx.KeyImages...)}
			if tx.Height > 0 {
				e["block_height"], e["confirmations"] = tx.Height, f.height-tx.Height
			}
			txs = append(txs, e)
		}
		res = map[string]any{"txs": txs, "status": "OK"}
		if len(missed) > 0 {
			res["missed_tx"] = missed
		}
	case "/is_key_image_spent":
		var q struct {
			KeyImages []string `json:"key_images"`
		}
		json.Unmarshal(body, &q)
		status := make([]int, len(q.KeyImages))
		for i, ki := range q.KeyImages {
			if tx := f.spender(ki); tx != nil && tx.Height > 0 {
				status[i] = 1
			} else if tx != nil {
				status[i] = 2
			}
		}
		res = map[string]any{"spent_status": status, "status": "OK"}
	}
	f.mu.Unlock()
	if res == nil {
		w.WriteHeader(404)
		return
	}
	if fault == fakeXMRDrop {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
		return
	}
	json.NewEncoder(w).Encode(res)
}

// spender is the transaction in the daemon's pool or chain that spends key image ki, or nil. f.mu is held.
func (f *fakeXMREscrow) spender(ki string) *fakeXMRTx {
	for _, h := range slices.Sorted(maps.Keys(f.txs)) {
		if slices.Contains(f.txs[h].KeyImages, ki) {
			return f.txs[h]
		}
	}
	return nil
}

// keyImage is output t's key image, given one on first use. f.mu is held.
func (f *fakeXMREscrow) keyImage(t *fakeXMRTransfer) string {
	if t.KeyImage == "" {
		f.seq++
		t.KeyImage = hex64("fake key image " + strconv.Itoa(f.seq) + " " + t.TxID)
	}
	return t.KeyImage
}

// fakeXMRWalletMethods are the wallet-rpc methods the fake answers; they run with f.mu held.
var fakeXMRWalletMethods = map[string]func(f *fakeXMREscrow, params json.RawMessage) (any, int64, string){
	"create_wallet": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct{ Filename, Password string }
		json.Unmarshal(p, &q)
		if f.files[q.Filename] != nil {
			return nil, -21, "Cannot create wallet. Already exists."
		}
		f.open = f.newFile(q.Filename, q.Password)
		return map[string]any{}, 0, ""
	},
	"open_wallet": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct{ Filename, Password string }
		json.Unmarshal(p, &q)
		w := f.files[q.Filename]
		switch {
		case w == nil:
			return nil, -1, "Failed to open wallet : file not found \"/w/" + q.Filename + ".keys\""
		case w.locked:
			return nil, -1, "Failed to open wallet : internal error: \"/w/" + q.Filename + ".keys\" is opened by another wallet program"
		case w.password != q.Password:
			return nil, -1, "Failed to open wallet : invalid password"
		}
		f.open = w
		return map[string]any{}, 0, ""
	},
	"close_wallet": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		f.open = nil
		return map[string]any{}, 0, ""
	},
	"store": func(*fakeXMREscrow, json.RawMessage) (any, int64, string) { return map[string]any{}, 0, "" },
	"refresh": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		f.open.scan(false)
		return map[string]any{"blocks_fetched": 0}, 0, ""
	},
	"rescan_blockchain": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		f.open.scan(true)
		return map[string]any{}, 0, ""
	},
	"set_subaddress_lookahead": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct {
			Password string `json:"password"`
			Major    int64  `json:"major_idx"`
			Minor    int64  `json:"minor_idx"`
		}
		json.Unmarshal(p, &q)
		switch {
		case q.Password != f.open.password:
			return nil, -22, "Invalid password."
		case q.Major == 0 || q.Minor == 0:
			return nil, -1, "Subaddress lookahead may not be zero"
		}
		f.open.lookahead = q.Major
		return map[string]any{}, 0, ""
	},
	"change_wallet_password": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct {
			Old string `json:"old_password"`
			New string `json:"new_password"`
		}
		json.Unmarshal(p, &q)
		if q.Old != f.open.password {
			return nil, -22, "Invalid original password."
		}
		f.open.password = q.New
		return map[string]any{}, 0, ""
	},
	"get_balance": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		var bal, unlocked int64
		per := []any{}
		for _, t := range f.open.transfers {
			if !f.open.visible(t) || t.Pool || t.DoubleSpend || t.SpentBy != "" {
				continue
			}
			u := int64(0)
			if t.unlocked() {
				u = t.Amount
			}
			bal, unlocked = bal+t.Amount, unlocked+u
			per = append(per, map[string]any{"account_index": t.Account, "address_index": t.Minor, "balance": t.Amount, "unlocked_balance": u})
		}
		return map[string]any{"balance": bal, "unlocked_balance": unlocked, "per_subaddress": per}, 0, ""
	},
	"get_height": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		return map[string]any{"height": f.height + f.walletLag}, 0, ""
	},
	"query_key": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		return map[string]any{"key": f.open.mnemonic}, 0, ""
	},
	"create_account": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		n := len(f.open.accounts)
		f.open.accounts = append(f.open.accounts, testXMRAddr("fake "+f.open.name+" account "+strconv.Itoa(n), true))
		return map[string]any{"account_index": n, "address": f.open.accounts[n]}, 0, ""
	},
	"get_address": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct {
			Account int64   `json:"account_index"`
			Minor   []int64 `json:"address_index"`
		}
		json.Unmarshal(p, &q)
		if q.Account < 0 || q.Account >= int64(len(f.open.accounts)) || len(q.Minor) != 1 {
			return nil, -14, "account index is out of bound"
		}
		addr := f.open.accounts[q.Account]
		if q.Minor[0] != 0 {
			addr = testXMRAddr("fake "+f.open.name+" account "+strconv.FormatInt(q.Account, 10)+" minor "+strconv.FormatInt(q.Minor[0], 10), true)
		}
		return map[string]any{"address": addr, "addresses": []any{map[string]any{"address": addr, "address_index": q.Minor[0], "label": "", "used": false}}}, 0, ""
	},
	"get_accounts": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		var list []any
		for i, addr := range f.open.accounts {
			var bal, unlocked int64
			for _, t := range f.open.transfers {
				if t.Account == int64(i) && !t.Pool && !t.DoubleSpend && t.SpentBy == "" {
					bal += t.Amount
					if t.Confs >= 10 && t.UnlockTime == 0 {
						unlocked += t.Amount
					}
				}
			}
			list = append(list, map[string]any{"account_index": i, "base_address": addr, "balance": bal, "unlocked_balance": unlocked, "label": ""})
		}
		return map[string]any{"subaddress_accounts": list}, 0, ""
	},
	"get_transfers": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct{ Out, Pending bool }
		json.Unmarshal(p, &q)
		in, pool, out, pending := []any{}, []any{}, []any{}, []any{}
		for _, h := range slices.Sorted(maps.Keys(f.txs)) {
			if tx := f.txs[h]; tx.Wallet == f.open.name {
				e := map[string]any{"txid": tx.Hash, "type": "pending", "amount": 0, "fee": tx.Fee, "unlock_time": 0, "subaddr_index": map[string]any{"major": tx.Account, "minor": 0}}
				if tx.Height == 0 {
					pending = append(pending, e)
				} else {
					e["type"], e["height"], e["confirmations"] = "out", tx.Height, f.height-tx.Height
					out = append(out, e)
				}
			}
		}
		for _, t := range f.open.transfers {
			if !f.open.visible(t) {
				continue
			}
			e := map[string]any{"txid": t.TxID, "type": "in", "amount": t.Amount, "fee": 30000000, "confirmations": t.Confs, "height": max(f.height-t.Confs, 0),
				"unlock_time": t.UnlockTime, "double_spend_seen": t.DoubleSpend, "subaddr_index": map[string]any{"major": t.Account, "minor": t.Minor}}
			if t.Pool {
				e["type"], e["height"] = "pool", 0
				delete(e, "confirmations")
				pool = append(pool, e)
			} else {
				in = append(in, e)
			}
		}
		res := map[string]any{"in": in, "pool": pool}
		if q.Out {
			res["out"] = out
		}
		if q.Pending {
			res["pending"] = pending
		}
		return res, 0, ""
	},
	"incoming_transfers": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct {
			Type    string `json:"transfer_type"`
			Account int64  `json:"account_index"`
		}
		json.Unmarshal(p, &q)
		var list []any
		for _, t := range f.open.transfers {
			if t.Account != q.Account || !f.open.visible(t) || t.Pool || t.SpentBy != "" || q.Type != "available" {
				continue
			}
			list = append(list, map[string]any{"amount": t.Amount, "block_height": max(f.height-t.Confs, 0), "frozen": t.Frozen, "key_image": f.keyImage(t),
				"spent": false, "tx_hash": t.TxID, "unlocked": t.unlocked(), "subaddr_index": map[string]any{"major": t.Account, "minor": t.Minor}})
		}
		if len(list) == 0 {
			return map[string]any{}, 0, "" // monero-wallet-rpc omits transfers when there are none
		}
		return map[string]any{"transfers": list}, 0, ""
	},
	"freeze": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) { return f.setFrozen(p, true) },
	"thaw":   func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) { return f.setFrozen(p, false) },
	"transfer": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct {
			Destinations []struct {
				Address string `json:"address"`
				Amount  int64  `json:"amount"`
			} `json:"destinations"`
			Account     int64 `json:"account_index"`
			SubtractFee []int `json:"subtract_fee_from_outputs"`
			DoNotRelay  bool  `json:"do_not_relay"`
		}
		json.Unmarshal(p, &q)
		if q.Account < 1 || q.Account >= int64(len(f.open.accounts)) {
			return nil, -14, "account index is out of bound"
		}
		for _, d := range q.Destinations {
			if f.invalid[d.Address] {
				return nil, -2, "WALLET_RPC_ERROR_CODE_WRONG_ADDRESS: " + d.Address
			}
			if f.crash[d.Address] {
				f.open = nil // restarted, as by a container restart policy: no wallet open
				return nil, fakeXMRCrash, ""
			}
		}
		var want, have int64
		for _, d := range q.Destinations {
			want += d.Amount
		}
		var spend []*fakeXMRTransfer
		for _, t := range slices.Backward(f.open.transfers) { // the newest first: an output left unfrozen is spent
			if have < want && t.Account == q.Account && t.SpentBy == "" && !t.Frozen && t.unlocked() {
				spend, have = append(spend, t), have+t.Amount
			}
		}
		if have < want || len(spend) == 0 {
			return nil, -17, "not enough money"
		}
		f.seq++
		hash := hex64("fake settlement tx " + strconv.Itoa(f.seq))
		tx := &fakeXMRTx{Hash: hash, Metadata: hex.EncodeToString([]byte("metadata " + hash)), Blob: hex.EncodeToString([]byte("blob " + hash)),
			Wallet: f.open.name, Account: q.Account, Fee: fakeXMRFee(len(spend), max(len(q.Destinations)+1, 2))}
		for _, t := range spend {
			tx.KeyImages = append(tx.KeyImages, f.keyImage(t))
		}
		amounts := make([]int64, len(q.Destinations))
		var total int64
		for i, d := range q.Destinations {
			amounts[i] = d.Amount
			if i == 0 && slices.Equal(q.SubtractFee, []int{0}) {
				amounts[i] -= tx.Fee
			}
			total += amounts[i]
		}
		f.built[tx.Metadata] = tx
		res := map[string]any{"tx_hash": hash, "fee": tx.Fee, "amount": total, "tx_metadata": tx.Metadata, "tx_blob": tx.Blob,
			"spent_key_images": map[string]any{"key_images": slices.Clone(tx.KeyImages)}, "amounts_by_dest": map[string]any{"amounts": amounts}}
		if f.transferLie != nil {
			f.transferLie(res)
		}
		if !q.DoNotRelay {
			f.relay(tx)
		}
		return res, 0, ""
	},
	"validate_address": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct {
			Address string `json:"address"`
		}
		json.Unmarshal(p, &q)
		return map[string]any{"valid": !f.invalid[q.Address], "integrated": false, "subaddress": false, "nettype": "mainnet", "openalias_address": ""}, 0, ""
	},
	"relay_tx": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct {
			Hex string `json:"hex"`
		}
		json.Unmarshal(p, &q)
		tx := f.built[q.Hex]
		if tx == nil {
			return nil, -8, "Failed to parse tx metadata"
		}
		if f.txs[tx.Hash] == nil {
			for _, ki := range tx.KeyImages {
				if f.spender(ki) != nil {
					return nil, -4, "transaction was rejected by daemon"
				}
			}
			f.relay(tx)
		}
		return map[string]any{"tx_hash": tx.Hash}, 0, ""
	},
	"get_transfer_by_txid": func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct {
			TxID    string `json:"txid"`
			Account int64  `json:"account_index"`
		}
		json.Unmarshal(p, &q)
		tx := f.txs[q.TxID]
		if tx == nil || tx.Wallet != f.open.name || tx.Account != q.Account {
			return nil, -8, "Transaction not found."
		}
		e := map[string]any{"txid": tx.Hash, "type": "pending", "amount": 0, "fee": tx.Fee, "unlock_time": 0, "double_spend_seen": false,
			"subaddr_index": map[string]any{"major": tx.Account, "minor": 0}}
		if tx.Height > 0 {
			e["type"], e["height"], e["confirmations"] = "out", tx.Height, f.height-tx.Height
		}
		return map[string]any{"transfer": e, "transfers": []any{e}}, 0, ""
	},
}

// setFrozen freezes or thaws the open wallet's output with the key image in params.
func (f *fakeXMREscrow) setFrozen(p json.RawMessage, frozen bool) (any, int64, string) {
	var q struct {
		KeyImage string `json:"key_image"`
	}
	json.Unmarshal(p, &q)
	for _, t := range f.open.transfers {
		if f.keyImage(t) == q.KeyImage {
			t.Frozen = frozen
			return map[string]any{}, 0, ""
		}
	}
	return nil, -10, "Key image not found"
}

// relay puts tx in the daemon's pool and marks the outputs it spends spent, in every wallet file. f.mu is held.
func (f *fakeXMREscrow) relay(tx *fakeXMRTx) {
	tx.Height = 0
	f.txs[tx.Hash] = tx
	for _, w := range f.files {
		for _, t := range w.transfers {
			if slices.Contains(tx.KeyImages, t.KeyImage) {
				t.SpentBy = tx.Hash
			}
		}
	}
}

// newFile makes wallet file name with password, its own seed and primary address, and no other account.
func (f *fakeXMREscrow) newFile(name, password string) *fakeXMRWalletFile {
	words := strings.Fields("abbey acid aisle album alpine amply anchor angle apart argue arrow atom audio avoid awful axis")
	var seed []string
	for i := range 25 {
		h := sha256.Sum256([]byte(name + "/" + strconv.Itoa(i)))
		seed = append(seed, words[int(h[0])%len(words)])
	}
	w := &fakeXMRWalletFile{name: name, password: password, mnemonic: strings.Join(seed, " "), accounts: []string{testXMRAddr("fake "+name+" primary", false)}, lookahead: 50}
	f.files[name] = w
	return w
}

// handle adds or replaces a wallet method (a later slice's freeze, transfer or relay_tx).
func (f *fakeXMREscrow) handle(method string, h func(f *fakeXMREscrow, params json.RawMessage) (any, int64, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = h
}

// fault queues fault for the next call of method on the wallet-rpc.
func (f *fakeXMREscrow) fault(method string, fault fakeXMRFault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[method] = append(f.faults[method], fault)
}

// faultFor queues fault for the next call of method whose params contain param (a wallet name, say).
func (f *fakeXMREscrow) faultFor(method, param string, fault fakeXMRFault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[method+"\x00"+param] = append(f.faults[method+"\x00"+param], fault)
}

// hook runs fn before each later call of method (nil removes it).
func (f *fakeXMREscrow) hook(method string, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[method] = fn
}

// edit runs fn on the fake's state under its lock.
func (f *fakeXMREscrow) edit(fn func(f *fakeXMREscrow)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// file returns wallet file name (a copy of its seed and accounts), or ok false.
func (f *fakeXMREscrow) file(name string) (w fakeXMRWalletFile, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.files[name]
	if p == nil {
		return w, false
	}
	return fakeXMRWalletFile{name: p.name, password: p.password, mnemonic: p.mnemonic, accounts: slices.Clone(p.accounts)}, true
}

// pay adds an incoming transfer to account of wallet name (which must exist), in the pool when confs is 0.
func (f *fakeXMREscrow) pay(name string, account int64, txid string, amount, confs int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.files[name]
	if w == nil {
		f.t.Fatalf("fake wallet-rpc: no wallet %s", name)
	}
	w.transfers = append(w.transfers, &fakeXMRTransfer{TxID: txid, Account: account, Amount: amount, Confs: confs, Pool: confs == 0})
}

// setTransfer applies fn to every transfer of txid in every wallet (confirmations, pool state, unlock time, double spend).
func (f *fakeXMREscrow) setTransfer(txid string, fn func(t *fakeXMRTransfer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.files {
		for _, t := range w.transfers {
			if t.TxID == txid {
				fn(t)
			}
		}
	}
}

// setConfs sets the confirmations of txid (0 puts it back in the pool).
func (f *fakeXMREscrow) setConfs(txid string, confs int64) {
	f.setTransfer(txid, func(t *fakeXMRTransfer) { t.Confs, t.Pool = confs, confs == 0 })
}

// accountOf is the wallet and account whose address 0 is addr.
func (f *fakeXMREscrow) accountOf(addr string) (string, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, w := range f.files {
		if i := slices.Index(w.accounts, addr); i >= 0 {
			return name, int64(i)
		}
	}
	f.t.Fatalf("fake wallet-rpc: no account at %s", addr)
	return "", 0
}

// payAddress pays escrow address addr (an account's address 0) in transaction txid.
func (f *fakeXMREscrow) payAddress(addr, txid string, amount, confs int64) {
	name, account := f.accountOf(addr)
	f.pay(name, account, txid, amount, confs)
}

// callLog is a copy of every request either node received, in order.
func (f *fakeXMREscrow) callLog() []fakeXMRCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// count is the number of requests either node received; countOf those of one wallet method.
func (f *fakeXMREscrow) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeXMREscrow) countOf(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// openName is the name of the open wallet, or "".
func (f *fakeXMREscrow) openName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open == nil {
		return ""
	}
	return f.open.name
}

// mine adds n blocks: each mines every transaction in the pool and moves the daemon (and the wallet) up one block.
func (f *fakeXMREscrow) mine(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for range n {
		for _, tx := range f.txs {
			if tx.Height == 0 {
				tx.Height = f.height // the new block's index
			}
		}
		f.height++
	}
}

// popBlocks removes the top n blocks (a reorganisation): their transactions go back to the pool, as monerod's
// pop_blocks returns them.
func (f *fakeXMREscrow) popBlocks(n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.height -= n
	for _, tx := range f.txs {
		if tx.Height >= f.height {
			tx.Height = 0
		}
	}
}

// flushPool drops hash from the daemon's pool (flush_txpool): the outputs it spent are unspent again.
func (f *fakeXMREscrow) flushPool(hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tx := f.txs[hash]; tx == nil || tx.Height != 0 {
		f.t.Fatalf("fake daemon: %s is not in the pool", hash)
	}
	delete(f.txs, hash)
	for _, w := range f.files {
		for _, t := range w.transfers {
			if t.SpentBy == hash {
				t.SpentBy = ""
			}
		}
	}
}

// spendOutside puts in the pool a transaction the market did not make spending the outputs with these key images (a
// wallet restored from the sealed seed), and returns its hash.
func (f *fakeXMREscrow) spendOutside(keyImages ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	tx := &fakeXMRTx{Hash: hex64("fake outside spend " + strconv.Itoa(f.seq)), KeyImages: keyImages}
	f.relay(tx)
	return tx.Hash
}

// tx is a copy of the transaction hash in the daemon's pool or chain, or ok false.
func (f *fakeXMREscrow) tx(hash string) (tx fakeXMRTx, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.txs[hash]; p != nil {
		return *p, true
	}
	return tx, false
}

// outputs lists copies of the outputs of escrow address addr's account, each with its key image.
func (f *fakeXMREscrow) outputs(addr string) []fakeXMRTransfer {
	name, account := f.accountOf(addr)
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeXMRTransfer
	for _, t := range f.files[name].transfers {
		if t.Account == account {
			f.keyImage(t)
			out = append(out, *t)
		}
	}
	return out
}
