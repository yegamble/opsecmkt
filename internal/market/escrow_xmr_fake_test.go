package market

import (
	"crypto/sha256"
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
// Faults: a call can be made to take effect and then lose its answer (the connection closes, as a timeout after the
// wallet acted does), to answer -13 (the wallet-rpc lost its open wallet), or, for open_wallet, to open another wallet
// file than the one asked for. A hook runs before a method's effect, so a test can change the database mid-pass.
// Every request is recorded. Later slices add the methods they need (freeze, transfer, relay_tx) with handle.
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
}

// fakeXMRWalletFile is one wallet file of the fake wallet-rpc.
type fakeXMRWalletFile struct {
	name, password, mnemonic string
	accounts                 []string // each account's address 0; [0] is the primary address
	transfers                []*fakeXMRTransfer
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
}

type fakeXMRFault int

const (
	fakeXMRDrop        fakeXMRFault = iota + 1 // the call takes effect, then the connection closes with no answer
	fakeXMRNotOpen                             // the call answers -13: no wallet open
	fakeXMRWrongWallet                         // open_wallet opens another wallet file than the one asked for
)

func newFakeXMREscrow(t *testing.T) *fakeXMREscrow {
	t.Helper()
	f := &fakeXMREscrow{t: t, height: 120, files: map[string]*fakeXMRWalletFile{}, faults: map[string][]fakeXMRFault{}, hooks: map[string]func(){},
		handlers: map[string]func(*fakeXMREscrow, json.RawMessage) (any, int64, string){}}
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
	if fault == fakeXMRDrop {
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
		if w == nil || w.password != q.Password {
			return nil, -1, "Failed to open wallet"
		}
		f.open = w
		return map[string]any{}, 0, ""
	},
	"close_wallet": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		f.open = nil
		return map[string]any{}, 0, ""
	},
	"store": func(*fakeXMREscrow, json.RawMessage) (any, int64, string) { return map[string]any{}, 0, "" },
	"refresh": func(*fakeXMREscrow, json.RawMessage) (any, int64, string) {
		return map[string]any{"blocks_fetched": 0}, 0, ""
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
				if t.Account == int64(i) && !t.Pool && !t.DoubleSpend {
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
	"get_transfers": func(f *fakeXMREscrow, _ json.RawMessage) (any, int64, string) {
		in, pool := []any{}, []any{}
		for _, t := range f.open.transfers {
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
		return map[string]any{"in": in, "pool": pool}, 0, ""
	},
}

// newFile makes wallet file name with password, its own seed and primary address, and no other account.
func (f *fakeXMREscrow) newFile(name, password string) *fakeXMRWalletFile {
	words := strings.Fields("abbey acid aisle album alpine amply anchor angle apart argue arrow atom audio avoid awful axis")
	var seed []string
	for i := range 25 {
		h := sha256.Sum256([]byte(name + "/" + strconv.Itoa(i)))
		seed = append(seed, words[int(h[0])%len(words)])
	}
	w := &fakeXMRWalletFile{name: name, password: password, mnemonic: strings.Join(seed, " "), accounts: []string{testXMRAddr("fake "+name+" primary", false)}}
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
