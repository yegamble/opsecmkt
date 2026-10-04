package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Monero escrow (WR-escrow Phase 3, A-248): the adapter for the escrow's own monero-wallet-rpc and monerod. It is
// apart from the pooled adapter (payments_monero.go, which is hard-wired to account 0, an empty wallet password and
// refuses fakechain) and shares only rpcClient.
//
// Each escrow wallet (escrow-YYYYMM-NN) holds one account per order; account 0 is the wallet's primary account and
// is never an escrow, never spent and never read as one. Every method that spends or reads one account takes the
// account index explicitly and sends it; buildTransfer, incomingTransfers, address and transferByTxid refuse
// account 0 before any request. Only primaryAddress reads account 0, for the primary-address check (P3-R6).
//
// Every RPC error is reduced to a fixed class (xmrRPCError) before anyone logs it: the wallet's error text can quote
// a wallet name, path or address, and rpcClient's transport errors name the host.

// xmrWalletSlowTimeout bounds the wallet calls that load, scan or rewrite a whole wallet: create_wallet, open_wallet,
// close_wallet (which stores), refresh, store, change_wallet_password (which rewrites the keys file and stores) and
// rescan_blockchain.
// monero-wallet-rpc answers one request at a time; opening a wallet
// rebuilds its subaddress table for every account (an escrow wallet holds one account per order, thousands a month,
// P3-R12), refresh scans every block since the last pass and store rewrites the encrypted cache, so on a large wallet
// each takes far longer than rpcTimeout's 10 s. Two minutes matches escrowKeypoolTimeout (the slow wallet call on the
// Bitcoin side) and still lets a watcher pass (watcherPassTimeout, 5 min) open, refresh and store one wallet. PAYE-E2
// (P3-R12) measures these calls; this bound is revisited if it measures them near the limit.
const xmrWalletSlowTimeout = 2 * time.Minute

// P3-R2: the escrow runs on a local fakechain only.
const (
	xmrMinHardFork  = 16        // CLSAG and Bulletproofs+, the rules monerod 0.18 enforces on mainnet
	xmrMaxHeight    = 1_000_000 // no local fakechain gets near it; every public Monero network is past it
	xmrMaxWalletLag = 10        // blocks between the wallet's height after a refresh and the daemon's
)

// xmrRefusal is a fixed refusal of the escrow nodes' network (P3-R2): it never carries anything the nodes sent.
// wallet marks a refusal of the wallet-rpc (its height), as opposed to the daemon.
type xmrRefusal struct {
	msg    string
	wallet bool
}

func (e *xmrRefusal) Error() string { return e.msg }

var (
	errXMRNotFakechain = &xmrRefusal{"the escrow daemon (ESCROW_XMR_DAEMON_URL) is not on a local fakechain (monerod --regtest); Monero escrow runs on a local fakechain only in this release", false}
	errXMRHardFork     = &xmrRefusal{"the escrow daemon (ESCROW_XMR_DAEMON_URL) runs a hard fork version below 16; start monerod --regtest from release 0.18 or later", false}
	errXMRDaemonHeight = &xmrRefusal{"the escrow daemon (ESCROW_XMR_DAEMON_URL) is at height 1,000,000 or more, which no local fakechain reaches; it is not a local fakechain", false}
	errXMRWalletHeight = &xmrRefusal{"the escrow wallet-rpc (ESCROW_XMR_WALLET_RPC_URL) is at height 1,000,000 or more, which no local fakechain reaches; it is attached to another network", true}
	errXMRWalletLag    = &xmrRefusal{"the escrow wallet-rpc (ESCROW_XMR_WALLET_RPC_URL) is more than 10 blocks from the escrow daemon after a refresh; it must use the daemon at ESCROW_XMR_DAEMON_URL (--daemon-address)", true}
)

// Offline refusals: the request is never sent.
var (
	errXMRAccountIndex = errors.New("monero escrow: the account index must be an escrow account (1 to 4294967295); account 0 is the wallet's primary account and is never spent or read as an escrow")
	errXMRWalletName   = errors.New("monero escrow: a wallet name must be escrow-YYYYMM-NN")
	errXMRDestination  = errors.New("monero escrow: every destination needs a positive amount and a fakechain standard address or subaddress")
	errXMRHex          = errors.New("monero escrow: a transaction hash or key image must be 64 hexadecimal characters, and tx metadata non-empty hex")
	errXMRLookahead    = errors.New("monero escrow: the subaddress lookahead must be positive")
)

// xmrRPCError is a failed escrow RPC reduced to a fixed class: rpc:<code>, credentials (HTTP 401), forbidden (HTTP
// 403), timeout, unreachable or bad-reply (classifyEscrowRPC's classes). It never carries the node's message.
type xmrRPCError struct {
	Service, Method, Class string // Service: "wallet" or "daemon"
	Code                   int64  // the JSON-RPC error code for class rpc:<code>
	// Dropped: the connection ended after the whole request was sent, with no answer or a cut one (class unreachable or
	// bad-reply): the node went away while it handled the call, as a monero-wallet-rpc that crashed building a
	// transaction does. Never set for a connection refused, a timeout or an answer that is not JSON-RPC.
	Dropped bool
	// Refusal: why open_wallet refused (xmrOpenNoFile, xmrOpenLocked or xmrOpenPassword), or "" when not known.
	Refusal string
}

func (e *xmrRPCError) Error() string {
	if e.Refusal != "" {
		return "monero escrow " + e.Service + " RPC " + e.Method + ": " + e.Class + " (" + e.Refusal + ")"
	}
	return "monero escrow " + e.Service + " RPC " + e.Method + ": " + e.Class
}

// open_wallet's refusals. monero-wallet-rpc 0.18.5.1 answers every failed open_wallet with -1 and "Failed to open wallet
// : " followed by wallet2's error text, which is read here into one of these fixed classes and never kept.
const (
	xmrOpenNoFile   = "no-file"        // wallet2 file_not_found: "file not found ..."
	xmrOpenLocked   = "locked"         // the keys file "is opened by another wallet program" (a file lock, transient)
	xmrOpenPassword = "wrong-password" // wallet2 invalid_password: "invalid password", the keys file did not decrypt
)

// classifyXMROpen classifies open_wallet's error as classifyXMRRPC does, with its refusal class.
func classifyXMROpen(err error) error {
	out := classifyXMRRPC("wallet", "open_wallet", err)
	var re *rpcError
	if errors.As(err, &re) && re.Code == -1 {
		msg := strings.ToLower(re.Message)
		refusal := ""
		switch {
		case strings.Contains(msg, "file not found"):
			refusal = xmrOpenNoFile
		case strings.Contains(msg, "is opened by another wallet program"):
			refusal = xmrOpenLocked
		case strings.HasSuffix(msg, ": invalid password"):
			refusal = xmrOpenPassword
		}
		out.(*xmrRPCError).Refusal = refusal
	}
	return out
}

// xmrOpenRefusal is the refusal class of an open_wallet error, or "".
func xmrOpenRefusal(err error) string {
	var re *xmrRPCError
	if errors.As(err, &re) {
		return re.Refusal
	}
	return ""
}

// classifyXMRRPC maps an rpcClient error to xmrRPCError with the Bitcoin escrow adapter's classes.
func classifyXMRRPC(service, method string, err error) error {
	if err == nil {
		return nil
	}
	var be *escrowRPCError
	errors.As(classifyEscrowRPC(method, err), &be)
	var dropped *rpcDroppedError
	return &xmrRPCError{Service: service, Method: method, Class: be.Class, Code: be.Code, Dropped: errors.As(err, &dropped)}
}

func xmrBadReply(service, method string) error {
	return &xmrRPCError{Service: service, Method: method, Class: "bad-reply"}
}

// xmrErrorClass is the class of an escrow error for a log line: an xmrRPCError's service, method and class, or the
// generic errorCause.
func xmrErrorClass(err error) string {
	var re *xmrRPCError
	if errors.As(err, &re) {
		return re.Method + " " + re.Class
	}
	return errorCause(err)
}

// xmrNotOpen reports a wallet call refused because monero-wallet-rpc has no wallet open (-13, NOT_OPEN).
func xmrNotOpen(err error) bool {
	var re *xmrRPCError
	return errors.As(err, &re) && re.Service == "wallet" && re.Code == -13
}

type xmrEscrowRPC struct {
	wallet, daemon *rpcClient
}

// newXMREscrowRPC builds the adapter without contacting either node. Errors name the setting, never its value.
func newXMREscrowRPC(walletURL, daemonURL string) (*xmrEscrowRPC, error) {
	w, err := newRPCClient("monero escrow wallet", walletURL, "2.0", true)
	if err != nil {
		return nil, errors.New("ESCROW_XMR_WALLET_RPC_URL must be http(s)://[user:password@]host:port")
	}
	d, err := newRPCClient("monero escrow daemon", daemonURL, "2.0", true)
	if err != nil {
		return nil, errors.New("ESCROW_XMR_DAEMON_URL must be http(s)://[user:password@]host:port")
	}
	return &xmrEscrowRPC{wallet: w, daemon: d}, nil
}

func (r *xmrEscrowRPC) walletCall(ctx context.Context, method string, params, out any) error {
	return r.walletCallWithin(ctx, rpcTimeout, method, params, out)
}

func (r *xmrEscrowRPC) walletCallWithin(ctx context.Context, timeout time.Duration, method string, params, out any) error {
	return classifyXMRRPC("wallet", method, r.wallet.callWithin(ctx, timeout, "/json_rpc", method, params, out))
}

func (r *xmrEscrowRPC) daemonCall(ctx context.Context, method string, params, out any) error {
	return classifyXMRRPC("daemon", method, r.daemon.call(ctx, "/json_rpc", method, params, out))
}

// daemonOther posts req as plain JSON to one of monerod's other endpoints (path, e.g. /get_transactions), which
// answer plain JSON with a status field, and decodes the answer into out when status is "OK".
func (r *xmrEscrowRPC) daemonOther(ctx context.Context, path string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	raw, status, err := r.daemon.exchange(ctx, rpcTimeout, path, path, body)
	if err != nil {
		return classifyXMRRPC("daemon", path, err)
	}
	var st struct {
		Status string `json:"status"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &st) != nil || st.Status != "OK" {
		return xmrBadReply("daemon", path)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(out) != nil {
		return xmrBadReply("daemon", path)
	}
	return nil
}

// xmrCount parses a non-negative integer the node sent (an amount, height or index); ok is false for anything else.
func xmrCount(n json.Number) (int64, bool) {
	v, err := strconv.ParseInt(string(n), 10, 64)
	return v, err == nil && v >= 0
}

func xmrHex64(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil
}

func xmrHexBlob(s string) bool {
	_, err := hex.DecodeString(s)
	return s != "" && err == nil
}

// xmrAccountOK: an escrow account index (monero-wallet-rpc's indices are uint32; 0 is the primary account).
func xmrAccountOK(account int64) bool { return account >= 1 && account <= math.MaxUint32 }

// xmrWalletNameOK: escrow-YYYYMM-NN, so the adapter can never open or create the pooled wallet or a path.
func xmrWalletNameOK(name string) bool {
	rest, ok := strings.CutPrefix(name, "escrow-")
	if !ok || len(rest) != 9 || rest[6] != '-' {
		return false
	}
	for i, c := range rest {
		if i != 6 && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// --- wallet lifecycle ---

// createWallet creates wallet name with password (xmrWalletPassword) and leaves it open.
func (r *xmrEscrowRPC) createWallet(ctx context.Context, name, password string) error {
	if !xmrWalletNameOK(name) {
		return errXMRWalletName
	}
	return r.walletCallWithin(ctx, xmrWalletSlowTimeout, "create_wallet", map[string]any{"filename": name, "password": password, "language": "English"}, nil)
}

// openWallet opens wallet name, closing (and storing) whichever wallet was open.
func (r *xmrEscrowRPC) openWallet(ctx context.Context, name, password string) error {
	if !xmrWalletNameOK(name) {
		return errXMRWalletName
	}
	return classifyXMROpen(r.wallet.callWithin(ctx, xmrWalletSlowTimeout, "/json_rpc", "open_wallet", map[string]any{"filename": name, "password": password}, nil))
}

// closeWallet stores and closes the open wallet.
func (r *xmrEscrowRPC) closeWallet(ctx context.Context) error {
	return r.walletCallWithin(ctx, xmrWalletSlowTimeout, "close_wallet", nil, nil)
}

func (r *xmrEscrowRPC) store(ctx context.Context) error {
	return r.walletCallWithin(ctx, xmrWalletSlowTimeout, "store", nil, nil)
}

func (r *xmrEscrowRPC) refresh(ctx context.Context) error {
	return r.walletCallWithin(ctx, xmrWalletSlowTimeout, "refresh", nil, nil)
}

// mnemonic is the open wallet's 25-word seed (query_key mnemonic), only for sealing it (escrow_xmr_seal.go). No
// error carries it.
func (r *xmrEscrowRPC) mnemonic(ctx context.Context) (string, error) {
	var res struct {
		Key string `json:"key"`
	}
	if err := r.walletCall(ctx, "query_key", map[string]any{"key_type": "mnemonic"}, &res); err != nil {
		return "", err
	}
	if len(strings.Fields(res.Key)) != 25 {
		return "", xmrBadReply("wallet", "query_key")
	}
	return res.Key, nil
}

// height is the open wallet's blockchain height (get_height).
func (r *xmrEscrowRPC) height(ctx context.Context) (int64, error) {
	var res struct {
		Height json.Number `json:"height"`
	}
	if err := r.walletCall(ctx, "get_height", nil, &res); err != nil {
		return 0, err
	}
	h, ok := xmrCount(res.Height)
	if !ok {
		return 0, xmrBadReply("wallet", "get_height")
	}
	return h, nil
}

// changeWalletPassword re-encrypts the open wallet's keys file and stores the wallet, so it runs on
// xmrWalletSlowTimeout.
func (r *xmrEscrowRPC) changeWalletPassword(ctx context.Context, oldPassword, newPassword string) error {
	return r.walletCallWithin(ctx, xmrWalletSlowTimeout, "change_wallet_password", map[string]any{"old_password": oldPassword, "new_password": newPassword}, nil)
}

// probeWallet opens wallet name as openWallet does, for the restore sweep (P3-R10), without storing the wallet open
// before (autosave_current false: each pass stores its wallets, and a probe of a name with no file would rewrite it
// for nothing): found is false when no such file exists (xmrOpenNoFile). Any other refusal, a wrong password or a
// lock say, is an error. A failed open_wallet leaves the wallet that was open, open.
func (r *xmrEscrowRPC) probeWallet(ctx context.Context, name, password string) (found bool, err error) {
	if !xmrWalletNameOK(name) {
		return false, errXMRWalletName
	}
	err = classifyXMROpen(r.wallet.callWithin(ctx, xmrWalletSlowTimeout, "/json_rpc", "open_wallet", map[string]any{"filename": name, "password": password, "autosave_current": false}, nil))
	if xmrOpenRefusal(err) == xmrOpenNoFile {
		return false, nil
	}
	return err == nil, err
}

// setSubaddressLookahead sets how many accounts (major) and addresses per account (minor) the open wallet scans ahead
// of the last one it knows, so a wallet older than the database sees deposits to accounts the database has (P3-R10).
// monero-wallet-rpc checks the wallet's password and writes the lookahead into its keys file.
func (r *xmrEscrowRPC) setSubaddressLookahead(ctx context.Context, password string, major, minor int64) error {
	if major < 1 || minor < 1 || major > math.MaxUint32 || minor > math.MaxUint32 {
		return errXMRLookahead
	}
	return r.walletCall(ctx, "set_subaddress_lookahead", map[string]any{"password": password, "major_idx": major, "minor_idx": minor}, nil)
}

// rescan scans the chain again from the open wallet's restore height (rescan_blockchain, not hard), so blocks already
// scanned are read with the lookahead now in force. It forgets what the chain cannot tell (frozen outputs, the
// destinations of earlier sends); the settlement builder freezes and thaws before each build.
func (r *xmrEscrowRPC) rescan(ctx context.Context) error {
	return r.walletCallWithin(ctx, xmrWalletSlowTimeout, "rescan_blockchain", map[string]any{"hard": false}, nil)
}

// --- accounts and addresses ---

// createAccount adds an account to the open wallet and returns its index (never 0) and its address 0, a
// subaddress.
func (r *xmrEscrowRPC) createAccount(ctx context.Context) (int64, string, error) {
	var res struct {
		Index   json.Number `json:"account_index"`
		Address string      `json:"address"`
	}
	if err := r.walletCall(ctx, "create_account", nil, &res); err != nil {
		return 0, "", err
	}
	idx, ok := xmrCount(res.Index)
	if a, err := parseXMRAddress(res.Address); !ok || !xmrAccountOK(idx) || err != nil || a.Kind != xmrSubaddress {
		return 0, "", xmrBadReply("wallet", "create_account")
	}
	return idx, res.Address, nil
}

// primaryAddress is the open wallet's primary address (account 0, address 0), the one read of account 0: the
// settlement check compares it with the stored one (P3-R6).
func (r *xmrEscrowRPC) primaryAddress(ctx context.Context) (string, error) {
	a, err := r.getAddress(ctx, 0, 0)
	if err != nil {
		return "", err
	}
	if p, _ := parseXMRAddress(a); p.Kind != xmrStandard {
		return "", xmrBadReply("wallet", "get_address")
	}
	return a, nil
}

// address is address minor of escrow account account, a subaddress.
func (r *xmrEscrowRPC) address(ctx context.Context, account, minor int64) (string, error) {
	if !xmrAccountOK(account) {
		return "", errXMRAccountIndex
	}
	a, err := r.getAddress(ctx, account, minor)
	if err != nil {
		return "", err
	}
	if p, _ := parseXMRAddress(a); p.Kind != xmrSubaddress {
		return "", xmrBadReply("wallet", "get_address")
	}
	return a, nil
}

func (r *xmrEscrowRPC) getAddress(ctx context.Context, account, minor int64) (string, error) {
	if minor < 0 || minor > math.MaxUint32 {
		return "", errXMRAccountIndex
	}
	var res struct {
		Addresses []struct {
			Address string      `json:"address"`
			Index   json.Number `json:"address_index"`
		} `json:"addresses"`
	}
	if err := r.walletCall(ctx, "get_address", map[string]any{"account_index": account, "address_index": []int64{minor}}, &res); err != nil {
		return "", err
	}
	if len(res.Addresses) != 1 {
		return "", xmrBadReply("wallet", "get_address")
	}
	if idx, ok := xmrCount(res.Addresses[0].Index); !ok || idx != minor {
		return "", xmrBadReply("wallet", "get_address")
	}
	return res.Addresses[0].Address, nil
}

// xmrAccount is one account of the open wallet (get_accounts), account 0 included.
type xmrAccount struct {
	Index             int64
	BaseAddress       string
	Balance, Unlocked int64
}

func (r *xmrEscrowRPC) accounts(ctx context.Context) ([]xmrAccount, error) {
	var res struct {
		Accounts []struct {
			Index    json.Number `json:"account_index"`
			Base     string      `json:"base_address"`
			Balance  json.Number `json:"balance"`
			Unlocked json.Number `json:"unlocked_balance"`
		} `json:"subaddress_accounts"`
	}
	if err := r.walletCall(ctx, "get_accounts", nil, &res); err != nil {
		return nil, err
	}
	out := make([]xmrAccount, 0, len(res.Accounts))
	for _, a := range res.Accounts {
		idx, ok1 := xmrCount(a.Index)
		bal, ok2 := xmrCount(a.Balance)
		unl, ok3 := xmrCount(a.Unlocked)
		if !ok1 || !ok2 || !ok3 {
			return nil, xmrBadReply("wallet", "get_accounts")
		}
		out = append(out, xmrAccount{Index: idx, BaseAddress: a.Base, Balance: bal, Unlocked: unl})
	}
	return out, nil
}

// xmrBalance is the open wallet's balance over all its accounts (get_balance all_accounts), with each funded
// subaddress's share.
type xmrBalance struct {
	Balance, Unlocked int64
	PerSubaddress     []xmrSubaddressBalance
}

type xmrSubaddressBalance struct {
	Account, Minor    int64
	Balance, Unlocked int64
}

func (r *xmrEscrowRPC) balance(ctx context.Context) (xmrBalance, error) {
	var res struct {
		Balance  json.Number `json:"balance"`
		Unlocked json.Number `json:"unlocked_balance"`
		Per      []struct {
			Account  json.Number `json:"account_index"`
			Minor    json.Number `json:"address_index"`
			Balance  json.Number `json:"balance"`
			Unlocked json.Number `json:"unlocked_balance"`
		} `json:"per_subaddress"`
	}
	if err := r.walletCall(ctx, "get_balance", map[string]any{"all_accounts": true}, &res); err != nil {
		return xmrBalance{}, err
	}
	var b xmrBalance
	var ok1, ok2 bool
	b.Balance, ok1 = xmrCount(res.Balance)
	b.Unlocked, ok2 = xmrCount(res.Unlocked)
	if !ok1 || !ok2 {
		return xmrBalance{}, xmrBadReply("wallet", "get_balance")
	}
	for _, p := range res.Per {
		acc, ok1 := xmrCount(p.Account)
		minor, ok2 := xmrCount(p.Minor)
		bal, ok3 := xmrCount(p.Balance)
		unl, ok4 := xmrCount(p.Unlocked)
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return xmrBalance{}, xmrBadReply("wallet", "get_balance")
		}
		b.PerSubaddress = append(b.PerSubaddress, xmrSubaddressBalance{Account: acc, Minor: minor, Balance: bal, Unlocked: unl})
	}
	return b, nil
}

// --- transfers ---

// xmrTransfer is one wallet transfer entry (get_transfers, get_transfer_by_txid). Locked is a non-zero unlock_time:
// such a deposit is never counted (P3-R5).
type xmrTransfer struct {
	TxID                    string
	Type                    string // in, pool, out, pending, failed
	Account, Minor          int64  // subaddr_index
	Amount, Fee             int64
	Confirmations, Height   int64
	Locked, DoubleSpendSeen bool
}

type xmrTransferJSON struct {
	TxID            string      `json:"txid"`
	Type            string      `json:"type"`
	Amount          json.Number `json:"amount"`
	Fee             json.Number `json:"fee"`
	Confirmations   json.Number `json:"confirmations"`
	Height          json.Number `json:"height"`
	UnlockTime      json.Number `json:"unlock_time"`
	DoubleSpendSeen bool        `json:"double_spend_seen"`
	SubaddrIndex    struct {
		Major json.Number `json:"major"`
		Minor json.Number `json:"minor"`
	} `json:"subaddr_index"`
}

func (t xmrTransferJSON) parse() (xmrTransfer, bool) {
	out := xmrTransfer{TxID: t.TxID, Type: t.Type, DoubleSpendSeen: t.DoubleSpendSeen}
	var ok [7]bool
	out.Account, ok[0] = xmrCount(t.SubaddrIndex.Major)
	out.Minor, ok[1] = xmrCount(t.SubaddrIndex.Minor)
	out.Amount, ok[2] = xmrCount(t.Amount)
	out.Fee, ok[3] = xmrCount(orZero(t.Fee))
	out.Confirmations, ok[4] = xmrCount(orZero(t.Confirmations))
	out.Height, ok[5] = xmrCount(orZero(t.Height))
	unlock, err := strconv.ParseUint(string(orZero(t.UnlockTime)), 10, 64)
	ok[6] = err == nil
	out.Locked = unlock != 0
	return out, ok == [7]bool{true, true, true, true, true, true, true} && xmrHex64(t.TxID)
}

// orZero reads a field the wallet may omit (a pool entry's confirmations) as 0.
func orZero(n json.Number) json.Number {
	if n == "" {
		return "0"
	}
	return n
}

// transfers lists incoming and pool transfers to every account of the open wallet (get_transfers in, pool,
// all_accounts): the funding read (P3-R5). Account 0 entries are returned too; the caller alerts on them.
func (r *xmrEscrowRPC) transfers(ctx context.Context) ([]xmrTransfer, error) {
	var res struct {
		In   []xmrTransferJSON `json:"in"`
		Pool []xmrTransferJSON `json:"pool"`
	}
	if err := r.walletCall(ctx, "get_transfers", map[string]any{"in": true, "pool": true, "all_accounts": true}, &res); err != nil {
		return nil, err
	}
	var out []xmrTransfer
	for _, t := range append(res.In, res.Pool...) {
		p, ok := t.parse()
		if !ok {
			return nil, xmrBadReply("wallet", "get_transfers")
		}
		out = append(out, p)
	}
	return out, nil
}

// inPool reports whether any transfer of the open wallet, to or from any account, is in the daemon's pool (get_transfers
// pool and pending, all_accounts): a wallet is destroyed only with nothing there (P3-R9).
func (r *xmrEscrowRPC) inPool(ctx context.Context) (bool, error) {
	var res map[string][]json.RawMessage
	if err := r.walletCall(ctx, "get_transfers", map[string]any{"pool": true, "pending": true, "all_accounts": true}, &res); err != nil {
		return false, err
	}
	return len(res["pool"])+len(res["pending"]) > 0, nil
}

// transferByTxid is the open wallet's record of txid in escrow account account (get_transfer_by_txid); found is false
// when the wallet does not know it (-8, WRONG_TXID).
func (r *xmrEscrowRPC) transferByTxid(ctx context.Context, account int64, txid string) ([]xmrTransfer, bool, error) {
	if !xmrAccountOK(account) {
		return nil, false, errXMRAccountIndex
	}
	if !xmrHex64(txid) {
		return nil, false, errXMRHex
	}
	var res struct {
		Transfers []xmrTransferJSON `json:"transfers"`
	}
	err := r.walletCall(ctx, "get_transfer_by_txid", map[string]any{"txid": txid, "account_index": account}, &res)
	var re *xmrRPCError
	if errors.As(err, &re) && re.Code == -8 {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var out []xmrTransfer
	for _, t := range res.Transfers {
		p, ok := t.parse()
		if !ok || p.TxID != txid {
			return nil, false, xmrBadReply("wallet", "get_transfer_by_txid")
		}
		out = append(out, p)
	}
	return out, len(out) > 0, nil
}

// xmrOutput is one unspent output of an escrow account (incoming_transfers available, verbose).
type xmrOutput struct {
	KeyImage, TxHash        string
	Amount, BlockHeight     int64
	Minor                   int64
	Frozen, Spent, Unlocked bool
}

// incomingTransfers lists escrow account account's unspent outputs with their key images (P3-R6). Frozen outputs
// are listed with Frozen set.
func (r *xmrEscrowRPC) incomingTransfers(ctx context.Context, account int64) ([]xmrOutput, error) {
	if !xmrAccountOK(account) {
		return nil, errXMRAccountIndex
	}
	var res struct {
		Transfers []struct {
			Amount       json.Number `json:"amount"`
			BlockHeight  json.Number `json:"block_height"`
			Frozen       bool        `json:"frozen"`
			KeyImage     string      `json:"key_image"`
			Spent        bool        `json:"spent"`
			TxHash       string      `json:"tx_hash"`
			Unlocked     bool        `json:"unlocked"`
			SubaddrIndex struct {
				Major json.Number `json:"major"`
				Minor json.Number `json:"minor"`
			} `json:"subaddr_index"`
		} `json:"transfers"`
	}
	if err := r.walletCall(ctx, "incoming_transfers", map[string]any{"transfer_type": "available", "account_index": account, "verbose": true}, &res); err != nil {
		return nil, err
	}
	out := make([]xmrOutput, 0, len(res.Transfers))
	for _, t := range res.Transfers {
		major, ok1 := xmrCount(t.SubaddrIndex.Major)
		minor, ok2 := xmrCount(t.SubaddrIndex.Minor)
		amt, ok3 := xmrCount(t.Amount)
		height, ok4 := xmrCount(orZero(t.BlockHeight))
		if !ok1 || !ok2 || !ok3 || !ok4 || major != account || !xmrHex64(t.KeyImage) || !xmrHex64(t.TxHash) {
			return nil, xmrBadReply("wallet", "incoming_transfers")
		}
		out = append(out, xmrOutput{KeyImage: t.KeyImage, TxHash: t.TxHash, Amount: amt, BlockHeight: height, Minor: minor,
			Frozen: t.Frozen, Spent: t.Spent, Unlocked: t.Unlocked})
	}
	return out, nil
}

// freeze keeps the output with keyImage out of every transfer the wallet builds; thaw releases it.
func (r *xmrEscrowRPC) freeze(ctx context.Context, keyImage string) error {
	if !xmrHex64(keyImage) {
		return errXMRHex
	}
	return r.walletCall(ctx, "freeze", map[string]any{"key_image": keyImage}, nil)
}

func (r *xmrEscrowRPC) thaw(ctx context.Context, keyImage string) error {
	if !xmrHex64(keyImage) {
		return errXMRHex
	}
	return r.walletCall(ctx, "thaw", map[string]any{"key_image": keyImage}, nil)
}

// xmrDestination is one output of a settlement; index 0 is the claimant, who pays the network fee.
type xmrDestination struct {
	Address string
	Amount  int64
}

// xmrBuiltTx is a settlement transaction built but not relayed: everything X-d stores before relay_tx.
type xmrBuiltTx struct {
	TxHash         string
	Fee, Amount    int64 // Amount: the sum of the destinations' amounts after the fee
	Metadata, Blob string
	SpentKeyImages []string
	AmountsByDest  []int64 // one per destination, in order
}

// buildTransfer builds, and never relays, a transaction from escrow account account to dests (transfer with
// do_not_relay, get_tx_metadata and get_tx_hex), the network fee taken from dests[0] alone. It runs on
// payoutSendTimeout, as every send does. Nothing is broadcast until relayTx.
func (r *xmrEscrowRPC) buildTransfer(ctx context.Context, account int64, dests []xmrDestination) (xmrBuiltTx, error) {
	if !xmrAccountOK(account) {
		return xmrBuiltTx{}, errXMRAccountIndex
	}
	if len(dests) == 0 {
		return xmrBuiltTx{}, errXMRDestination
	}
	list := make([]map[string]any, len(dests))
	for i, d := range dests {
		if _, err := parseXMRAddress(d.Address); err != nil || d.Amount <= 0 {
			return xmrBuiltTx{}, errXMRDestination
		}
		list[i] = map[string]any{"address": d.Address, "amount": d.Amount}
	}
	params := map[string]any{"destinations": list, "account_index": account, "priority": 0, "subtract_fee_from_outputs": []int{0},
		"do_not_relay": true, "get_tx_metadata": true, "get_tx_hex": true}
	var res struct {
		TxHash         string      `json:"tx_hash"`
		Fee            json.Number `json:"fee"`
		Amount         json.Number `json:"amount"`
		Metadata       string      `json:"tx_metadata"`
		Blob           string      `json:"tx_blob"`
		SpentKeyImages struct {
			KeyImages []string `json:"key_images"`
		} `json:"spent_key_images"`
		AmountsByDest struct {
			Amounts []json.Number `json:"amounts"`
		} `json:"amounts_by_dest"`
	}
	if err := r.walletCallWithin(ctx, payoutSendTimeout, "transfer", params, &res); err != nil {
		return xmrBuiltTx{}, err
	}
	tx := xmrBuiltTx{TxHash: res.TxHash, Metadata: res.Metadata, Blob: res.Blob, SpentKeyImages: res.SpentKeyImages.KeyImages}
	fee, ok1 := xmrCount(res.Fee)
	amt, ok2 := xmrCount(res.Amount)
	ok := ok1 && ok2 && fee > 0 && xmrHex64(res.TxHash) && xmrHexBlob(res.Metadata) && xmrHexBlob(res.Blob) &&
		len(tx.SpentKeyImages) > 0 && len(res.AmountsByDest.Amounts) == len(dests)
	for _, ki := range tx.SpentKeyImages {
		ok = ok && xmrHex64(ki)
	}
	for _, n := range res.AmountsByDest.Amounts {
		v, good := xmrCount(n)
		ok = ok && good
		tx.AmountsByDest = append(tx.AmountsByDest, v)
	}
	if !ok {
		return xmrBuiltTx{}, xmrBadReply("wallet", "transfer")
	}
	tx.Fee, tx.Amount = fee, amt
	return tx, nil
}

// validAddress asks the open wallet whether addr is a destination it can pay (validate_address on the wallet's own
// network type): a standard address or subaddress whose checksum and keys are valid. The settlement builder asks only
// after transfer refused a destination (-2), to name which output it refused; parseXMRAddress checks the checksum
// offline but not that the keys are curve points.
func (r *xmrEscrowRPC) validAddress(ctx context.Context, addr string) (bool, error) {
	if _, err := parseXMRAddress(addr); err != nil {
		return false, nil
	}
	var res struct {
		Valid      bool `json:"valid"`
		Integrated bool `json:"integrated"`
	}
	if err := r.walletCall(ctx, "validate_address", map[string]any{"address": addr, "any_net_type": false}, &res); err != nil {
		return false, err
	}
	return res.Valid && !res.Integrated, nil
}

// relayTx broadcasts a built transaction from its stored metadata and returns its hash. Relaying the same metadata
// again spends the same key images, so it cannot pay twice. It runs on payoutSendTimeout.
func (r *xmrEscrowRPC) relayTx(ctx context.Context, metadata string) (string, error) {
	if !xmrHexBlob(metadata) {
		return "", errXMRHex
	}
	var res struct {
		TxHash string `json:"tx_hash"`
	}
	if err := r.walletCallWithin(ctx, payoutSendTimeout, "relay_tx", map[string]any{"hex": metadata}, &res); err != nil {
		return "", err
	}
	if !xmrHex64(res.TxHash) {
		return "", xmrBadReply("wallet", "relay_tx")
	}
	return res.TxHash, nil
}

// --- daemon ---

type xmrDaemonInfo struct {
	NetType                    string
	Mainnet, Stagenet, Testnet bool
	Height                     int64
}

func (r *xmrEscrowRPC) daemonInfo(ctx context.Context) (xmrDaemonInfo, error) {
	var res struct {
		NetType  string      `json:"nettype"`
		Mainnet  bool        `json:"mainnet"`
		Stagenet bool        `json:"stagenet"`
		Testnet  bool        `json:"testnet"`
		Height   json.Number `json:"height"`
	}
	if err := r.daemonCall(ctx, "get_info", nil, &res); err != nil {
		return xmrDaemonInfo{}, err
	}
	h, ok := xmrCount(res.Height)
	if !ok {
		return xmrDaemonInfo{}, xmrBadReply("daemon", "get_info")
	}
	return xmrDaemonInfo{NetType: res.NetType, Mainnet: res.Mainnet, Stagenet: res.Stagenet, Testnet: res.Testnet, Height: h}, nil
}

func (r *xmrEscrowRPC) hardForkVersion(ctx context.Context) (int64, error) {
	var res struct {
		Version json.Number `json:"version"`
	}
	if err := r.daemonCall(ctx, "hard_fork_info", nil, &res); err != nil {
		return 0, err
	}
	v, ok := xmrCount(res.Version)
	if !ok {
		return 0, xmrBadReply("daemon", "hard_fork_info")
	}
	return v, nil
}

// xmrDaemonTx is a transaction as the escrow daemon knows it (/get_transactions with decode_as_json): where it is,
// and from its decoded body each key input's ring size and key image and the RingCT type (6: CLSAG with
// Bulletproofs+).
type xmrDaemonTx struct {
	Hash                       string
	InPool, DoubleSpendSeen    bool
	BlockHeight, Confirmations int64
	RCTType                    int64
	RingSizes                  []int
	KeyImages                  []string
}

// transactions looks hashes up in the escrow daemon; missed lists the hashes it does not know.
func (r *xmrEscrowRPC) transactions(ctx context.Context, hashes []string) (txs []xmrDaemonTx, missed []string, err error) {
	if len(hashes) == 0 {
		return nil, nil, errXMRHex
	}
	for _, h := range hashes {
		if !xmrHex64(h) {
			return nil, nil, errXMRHex
		}
	}
	var res struct {
		Missed []string `json:"missed_tx"`
		Txs    []struct {
			Hash            string      `json:"tx_hash"`
			InPool          bool        `json:"in_pool"`
			DoubleSpendSeen bool        `json:"double_spend_seen"`
			BlockHeight     json.Number `json:"block_height"`
			Confirmations   json.Number `json:"confirmations"`
			AsJSON          string      `json:"as_json"`
		} `json:"txs"`
	}
	const path = "/get_transactions"
	if err := r.daemonOther(ctx, path, map[string]any{"txs_hashes": hashes, "decode_as_json": true}, &res); err != nil {
		return nil, nil, err
	}
	for _, t := range res.Txs {
		var body struct {
			Vin []struct {
				Key *struct {
					KeyOffsets []json.Number `json:"key_offsets"`
					KeyImage   string        `json:"k_image"`
				} `json:"key"`
			} `json:"vin"`
			RCT struct {
				Type json.Number `json:"type"`
			} `json:"rct_signatures"`
		}
		dec := json.NewDecoder(strings.NewReader(t.AsJSON))
		dec.UseNumber()
		height, ok1 := xmrCount(orZero(t.BlockHeight))
		confs, ok2 := xmrCount(orZero(t.Confirmations))
		if dec.Decode(&body) != nil || !ok1 || !ok2 || !xmrHex64(t.Hash) {
			return nil, nil, xmrBadReply("daemon", path)
		}
		rct, ok := xmrCount(body.RCT.Type)
		if !ok {
			return nil, nil, xmrBadReply("daemon", path)
		}
		d := xmrDaemonTx{Hash: t.Hash, InPool: t.InPool, DoubleSpendSeen: t.DoubleSpendSeen, BlockHeight: height, Confirmations: confs, RCTType: rct}
		for _, in := range body.Vin {
			if in.Key == nil { // a coinbase input has no ring
				continue
			}
			d.RingSizes = append(d.RingSizes, len(in.Key.KeyOffsets))
			d.KeyImages = append(d.KeyImages, in.Key.KeyImage)
		}
		txs = append(txs, d)
	}
	return txs, res.Missed, nil
}

// keyImagesSpent reports each key image's state in the escrow daemon (/is_key_image_spent): 0 unspent, 1 spent on
// chain, 2 spent in the pool.
func (r *xmrEscrowRPC) keyImagesSpent(ctx context.Context, keyImages []string) ([]int, error) {
	if len(keyImages) == 0 {
		return nil, errXMRHex
	}
	for _, ki := range keyImages {
		if !xmrHex64(ki) {
			return nil, errXMRHex
		}
	}
	var res struct {
		Spent []json.Number `json:"spent_status"`
	}
	const path = "/is_key_image_spent"
	if err := r.daemonOther(ctx, path, map[string]any{"key_images": keyImages}, &res); err != nil {
		return nil, err
	}
	if len(res.Spent) != len(keyImages) {
		return nil, xmrBadReply("daemon", path)
	}
	out := make([]int, len(res.Spent))
	for i, n := range res.Spent {
		v, ok := xmrCount(n)
		if !ok || v > 2 {
			return nil, xmrBadReply("daemon", path)
		}
		out[i] = int(v)
	}
	return out, nil
}

// --- network check (P3-R2) ---

// checkDaemon requires the escrow daemon to be a local fakechain: nettype fakechain with no public-network flag,
// hard fork version at least 16 and a height below 1,000,000. It returns the daemon's height.
func (r *xmrEscrowRPC) checkDaemon(ctx context.Context) (int64, error) {
	info, err := r.daemonInfo(ctx)
	if err != nil {
		return 0, err
	}
	if info.NetType != "fakechain" || info.Mainnet || info.Stagenet || info.Testnet {
		return 0, errXMRNotFakechain
	}
	v, err := r.hardForkVersion(ctx)
	if err != nil {
		return 0, err
	}
	if v < xmrMinHardFork {
		return 0, errXMRHardFork
	}
	if info.Height >= xmrMaxHeight {
		return 0, errXMRDaemonHeight
	}
	return info.Height, nil
}

// checkWallet refreshes the open wallet and requires its height to be below 1,000,000 and within 10 blocks of
// daemonHeight: a wallet-rpc attached to another daemon is refused. The watcher runs it after each open_wallet.
func (r *xmrEscrowRPC) checkWallet(ctx context.Context, daemonHeight int64) error {
	if err := r.refresh(ctx); err != nil {
		return err
	}
	h, err := r.height(ctx)
	if err != nil {
		return err
	}
	if h >= xmrMaxHeight {
		return errXMRWalletHeight
	}
	if h-daemonHeight > xmrMaxWalletLag || daemonHeight-h > xmrMaxWalletLag {
		return errXMRWalletLag
	}
	return nil
}

// checkNetwork runs checkDaemon and, when the wallet-rpc has a wallet open, checkWallet. walletOpen is false when
// it has none (-13): a fresh install has no escrow wallet yet, and the watcher checks each wallet it opens.
func (r *xmrEscrowRPC) checkNetwork(ctx context.Context) (walletOpen bool, err error) {
	h, err := r.checkDaemon(ctx)
	if err != nil {
		return false, err
	}
	if err = r.checkWallet(ctx, h); xmrNotOpen(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
