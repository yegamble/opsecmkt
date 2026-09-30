package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Bitcoin escrow (WR-escrow Phase 1, A-246): configuration, the startup checks and the Bitcoin Core RPC adapter
// the escrow code uses. It is apart from the pooled adapter (payments_bitcoin.go) and shares only rpcClient.
//
// ESCROW_BTC=on (off by default) runs only against a regtest node in Phase 1. The app reaches Core as its own RPC
// user, which the operator restricts with rpcwhitelist to the methods below (docs/operator-guide.md, "Bitcoin
// escrow (regtest only)"). listdescriptors is deliberately not among them: rpcwhitelist allows or refuses a
// method, not its arguments, and a user allowed listdescriptors can call it with private=true and export the
// arbiter's private key (probed on Core 31.1 regtest). The arbiter's fingerprint is read with
// getrawchangeaddress and getaddressinfo instead, which export nothing.
//
// Every RPC error is reduced to a fixed class (escrowRPCError) before anyone logs it: Core's error text can quote
// a descriptor or a key (A-170, ES-10).

// escrowRPCMethods is every method the adapter calls: the rpcwhitelist line the operator guide documents.
var escrowRPCMethods = []string{
	"getblockchaininfo", "getrawchangeaddress", "getaddressinfo", "createpsbt", "utxoupdatepsbt", "importdescriptors",
	"getdescriptorinfo", "deriveaddresses", "gettxout", "keypoolrefill", "walletprocesspsbt", "descriptorprocesspsbt",
	"combinepsbt", "finalizepsbt", "testmempoolaccept", "sendrawtransaction", "getwalletinfo", "listreceivedbyaddress",
	"gettransaction",
}

// escrowSighash is the only sighash type the market ever signs with (ES-2): a signature made with SIGHASH_NONE let
// a party redirect a refund and still passed testmempoolaccept on regtest.
const escrowSighash = "ALL"

// escrowKeypoolTimeout bounds keypoolrefill: deriving 20000 keys took 10-15 s on regtest.
const escrowKeypoolTimeout = 2 * time.Minute

type escrowConfig struct {
	on            bool
	rpcURL        string
	arbiterWallet string
	watchWallet   string // the watch-only wallet escrow descriptors are imported into (P1-b)
	feeBps        int64  // the market fee in basis points, snapshotted on each escrow at checkout (ES-5)
	feeAddress    string // where the market fee goes ("" when feeBps is 0); one fixed address, test networks only
	confirmations int64  // PAYMENT_CONFIRMATIONS_BTC: confirmations before an escrow counts as funded
}

// escrowConfigFromEnv reads ESCROW_BTC (on/off, blank = off), ESCROW_BTC_RPC_URL, ESCROW_BTC_ARBITER_WALLET
// (default opsecmkt-arbiter), ESCROW_BTC_WATCH_WALLET (default opsecmkt-escrow-watch), ESCROW_BTC_FEE_BPS (0 to
// 10000, default 0), ESCROW_BTC_FEE_ADDRESS (needed when the fee is above 0) and PAYMENT_CONFIRMATIONS_BTC (1 to
// 1000, default 3). Nothing is contacted.
func escrowConfigFromEnv(getenv func(string) string) (escrowConfig, error) {
	var c escrowConfig
	switch v := strings.TrimSpace(getenv("ESCROW_BTC")); v {
	case "", "off":
		return c, nil
	case "on":
		c.on = true
	default:
		return c, errors.New(`ESCROW_BTC must be "on" or "off" (blank means off); Bitcoin escrow runs on regtest only in this release`)
	}
	c.rpcURL = strings.TrimSpace(getenv("ESCROW_BTC_RPC_URL"))
	if c.rpcURL == "" {
		return c, errors.New("ESCROW_BTC=on needs ESCROW_BTC_RPC_URL: the regtest Bitcoin Core RPC URL of the app's rpcwhitelist user (docs/operator-guide.md, \"Bitcoin escrow (regtest only)\")")
	}
	c.arbiterWallet = strings.TrimSpace(getenv("ESCROW_BTC_ARBITER_WALLET"))
	if c.arbiterWallet == "" {
		c.arbiterWallet = "opsecmkt-arbiter"
	}
	if chain := strings.TrimSpace(getenv("BITCOIN_CHAIN")); chain != "" && chain != "regtest" {
		return c, errors.New("refusing to start: ESCROW_BTC=on runs only on regtest in this release, and BITCOIN_CHAIN names another chain. Set ESCROW_BTC=off, or use regtest for both")
	}
	c.watchWallet = strings.TrimSpace(getenv("ESCROW_BTC_WATCH_WALLET"))
	if c.watchWallet == "" {
		c.watchWallet = "opsecmkt-escrow-watch"
	}
	if c.watchWallet == c.arbiterWallet {
		return c, errors.New("ESCROW_BTC_WATCH_WALLET must name a watch-only wallet of its own, not the arbiter wallet ESCROW_BTC_ARBITER_WALLET")
	}
	if strings.TrimSpace(getenv("BITCOIN_RPC_URL")) != "" {
		pooled := strings.TrimSpace(getenv("BITCOIN_WALLET"))
		if pooled == "" {
			pooled = "opsecmkt"
		}
		if pooled == c.arbiterWallet {
			return c, errors.New("ESCROW_BTC_ARBITER_WALLET must name a wallet that holds only the escrow arbiter key, not the pooled wallet BITCOIN_WALLET")
		}
		if pooled == c.watchWallet {
			return c, errors.New("ESCROW_BTC_WATCH_WALLET must name a watch-only wallet of its own, not the pooled wallet BITCOIN_WALLET")
		}
	}
	if v := strings.TrimSpace(getenv("ESCROW_BTC_FEE_BPS")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > 10000 {
			return c, errors.New("ESCROW_BTC_FEE_BPS must be a whole number of basis points from 0 to 10000 (250 = 2.5%)")
		}
		c.feeBps = n
	}
	if c.feeBps > 0 {
		var ok bool
		if c.feeAddress, ok = escrowAddressForm(getenv("ESCROW_BTC_FEE_ADDRESS")); !ok {
			return c, errors.New("ESCROW_BTC_FEE_BPS above 0 needs ESCROW_BTC_FEE_ADDRESS: the regtest Bitcoin address the market fee is paid to")
		}
	}
	c.confirmations = 3
	if v := strings.TrimSpace(getenv("PAYMENT_CONFIRMATIONS_BTC")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 1000 {
			return c, errors.New("PAYMENT_CONFIRMATIONS_BTC must be a whole number from 1 to 1000")
		}
		c.confirmations = n
	}
	return c, nil
}

// errEscrowNotRegtest is the fixed startup refusal for an escrow node on any chain but regtest.
var errEscrowNotRegtest = errors.New("refusing to start: ESCROW_BTC=on runs only on regtest in this release (escrow is not verified on any public network), and the node at ESCROW_BTC_RPC_URL is not on regtest. Set ESCROW_BTC=off, or point ESCROW_BTC_RPC_URL at a regtest node")

// Fixed operator lines for an escrow that is switched on but not usable: nothing in them comes from the node.
const (
	escrowLogOn           = "escrow: Bitcoin escrow on (regtest only)"
	escrowLogUnchecked    = "escrow: Bitcoin escrow disabled: the escrow node could not be checked at startup"
	escrowLogArbiter      = "escrow: Bitcoin escrow disabled: the arbiter wallet could not be read (is ESCROW_BTC_ARBITER_WALLET loaded, and a descriptor wallet with private keys?)"
	escrowLogFingerprint  = "escrow: Bitcoin escrow disabled: the arbiter wallet's key fingerprint differs from the one pinned in this database; see docs/operator-guide.md, \"Bitcoin escrow (regtest only)\""
	escrowLogWatch        = "escrow: Bitcoin escrow disabled: the watch-only wallet could not be read (is ESCROW_BTC_WATCH_WALLET loaded, and a descriptor wallet with private keys disabled?)"
	escrowLogFixedFee     = "escrow: the market fee is paid to one fixed address (ESCROW_BTC_FEE_ADDRESS); allowed on test networks only"
	escrowLogRefused      = "escrow: Bitcoin escrow disabled until the app is restarted: the escrow node is not on regtest, or it refused ESCROW_BTC_FEE_ADDRESS"
	escrowLogUnrecorded   = "escrow: Bitcoin escrow disabled: its arbiter fingerprint could not be checked in the database"
	escrowFingerprintPin  = "escrow_btc_arbiter_fingerprint" // settings key
	escrowFingerprintSize = 8
)

// escrowBTC is the escrow adapter; App.escrow is nil until ESCROW_BTC=on has passed every check (startEscrow).
type escrowBTC struct {
	rpc           *rpcClient
	arbiterName   string
	arbiterWallet string    // "/wallet/<name>"
	fingerprint   string    // the arbiter wallet's master fingerprint, pinned in settings
	arbiterKey    escrowKey // the arbiter's BIP84 account key; the market's per-escrow child keys derive from it
	watchName     string    // the watch-only wallet escrow descriptors are imported into
	watchWallet   string    // "/wallet/<name>"
	feeBps        int64
	feeAddress    string
	confirmations int64
}

func newEscrowBTC(c escrowConfig) (*escrowBTC, error) {
	rpc, err := newRPCClient("bitcoin escrow", c.rpcURL, "1.0", false)
	if err != nil {
		return nil, errors.New("ESCROW_BTC_RPC_URL must be http(s)://user:password@host:port")
	}
	return &escrowBTC{rpc: rpc, arbiterName: c.arbiterWallet, arbiterWallet: "/wallet/" + url.PathEscape(c.arbiterWallet),
		watchName: c.watchWallet, watchWallet: "/wallet/" + url.PathEscape(c.watchWallet), feeBps: c.feeBps, feeAddress: c.feeAddress,
		confirmations: c.confirmations}, nil
}

// initEscrow runs in New after the payment providers. ESCROW_BTC=off does nothing. On, a node that answers on any
// chain but regtest stops startup (errEscrowNotRegtest); an unreachable node, an unreadable arbiter wallet or a
// fingerprint that differs from the pinned one leaves escrow disabled with one fixed line, and the market runs; the
// escrow watcher then retries (retryEscrow).
func (a *App) initEscrow(ctx context.Context) error {
	cfg, err := escrowConfigFromEnv(os.Getenv)
	if err != nil || !cfg.on {
		return err
	}
	a.escrowConfigured = true
	if a.live {
		return errors.New("refusing to start: ESCROW_BTC=on runs only on regtest in this release, and this market is configured for mainnet")
	}
	if _, err = newEscrowBTC(cfg); err != nil {
		return err
	}
	a.escrowCfg = cfg
	state, class, err := a.startEscrow(ctx)
	if err == nil {
		logEscrowState(state, class)
		a.escrowStartState = state
	}
	return err
}

// errEscrowFeeAddress is the fixed startup refusal for a fee address the escrow node does not accept.
var errEscrowFeeAddress = errors.New("refusing to start: ESCROW_BTC_FEE_ADDRESS is not a valid address on the escrow node's chain (regtest)")

// startEscrow runs every escrow check once and, when all pass, enables escrow (a.escrow). It reports the state as
// one of the fixed lines (escrowLogOn when enabled) with the error class that caused it (""), or an error: a refusal
// (errEscrowNotRegtest, errEscrowFeeAddress) or a database failure. Nothing here comes from the node's text. The
// guards are the same at startup and on a retry: the node must be on regtest, the arbiter fingerprint must match the
// pinned one, and the watch-only wallet must hold no private keys.
func (a *App) startEscrow(ctx context.Context) (state, class string, err error) {
	e, err := newEscrowBTC(a.escrowCfg)
	if err != nil {
		return "", "", err
	}
	chain, err := e.chain(ctx)
	if err != nil {
		return escrowLogUnchecked, errorClassOf(err), nil
	}
	if chain != "regtest" {
		return "", "", errEscrowNotRegtest
	}
	if err = e.readArbiter(ctx); err != nil {
		return escrowLogArbiter, errorClassOf(err), nil
	}
	ok, err := pinArbiterFingerprint(ctx, a.db, e.fingerprint)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return escrowLogFingerprint, "", nil
	}
	if err = e.checkWatchWallet(ctx); err != nil {
		return escrowLogWatch, errorClassOf(err), nil
	}
	if e.feeAddress != "" {
		valid, err := e.validAddress(ctx, e.feeAddress)
		if err != nil {
			return escrowLogUnchecked, errorClassOf(err), nil
		}
		if !valid {
			return "", "", errEscrowFeeAddress
		}
		log.Print(escrowLogFixedFee)
	}
	a.escrow.Store(e)
	return escrowLogOn, "", nil
}

// logEscrowState logs a startEscrow state line, with its error class when there is one.
func logEscrowState(state, class string) {
	if class != "" {
		log.Printf("%s (%s)", state, class)
		return
	}
	log.Print(state)
}

// escrowRetry is the escrow watcher's record of the last escrow state it logged, so a state is logged when it
// changes, not on every pass.
type escrowRetry struct {
	last    string
	refused bool // the node answered on another chain, or refused the fee address: no retry until a restart
}

// retryEscrow runs startEscrow again for the escrow watcher while escrow is switched on but not enabled (A-316), as
// refreshProviders retries a pooled provider. A refusal that would stop startup cannot stop a running market: it logs
// one fixed line and ends the retries until a restart.
func (a *App) retryEscrow(ctx context.Context, r *escrowRetry) {
	if r.refused || a.escrow.Load() != nil {
		return
	}
	pass, cancel := context.WithTimeout(ctx, watcherPassTimeout)
	state, class, err := a.startEscrow(pass)
	cancel()
	switch {
	case ctx.Err() != nil:
		return
	case errors.Is(err, errEscrowNotRegtest), errors.Is(err, errEscrowFeeAddress):
		r.refused, state = true, escrowLogRefused
	case err != nil:
		state, class = escrowLogUnrecorded, errorCause(err)
	}
	if state != r.last {
		logEscrowState(state, class)
		r.last = state
	}
}

// pinArbiterFingerprint records fp as the arbiter fingerprint the first time and reports whether fp equals the
// recorded one. Concurrent first startups agree on whichever row was written first.
func pinArbiterFingerprint(ctx context.Context, db *sql.DB, fp string) (bool, error) {
	if _, err := db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO NOTHING", escrowFingerprintPin, fp); err != nil {
		return false, err
	}
	var pinned string
	if err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", escrowFingerprintPin).Scan(&pinned); err != nil {
		return false, err
	}
	return pinned == fp, nil
}

// escrowRPCError is a failed escrow RPC reduced to a fixed class: rpc:<code>, forbidden (HTTP 403: the method is
// not on the user's rpcwhitelist), credentials (HTTP 401), timeout, unreachable or bad-reply. It never carries
// Core's message.
type escrowRPCError struct {
	Method, Class string
	Code          int64 // the JSON-RPC error code for class rpc:<code>
}

func (e *escrowRPCError) Error() string { return "bitcoin escrow RPC " + e.Method + ": " + e.Class }

// errorClassOf is the class of an escrow error for a log line: an escrowRPCError's method and class, or the
// generic errorCause.
func errorClassOf(err error) string {
	var re *escrowRPCError
	if errors.As(err, &re) {
		return re.Method + " " + re.Class
	}
	return errorCause(err)
}

// classifyEscrowRPC maps an rpcClient error to escrowRPCError. rpcClient's own messages name the class in fixed
// words (payments_rpc.go); anything else is bad-reply.
func classifyEscrowRPC(method string, err error) error {
	if err == nil {
		return nil
	}
	e := &escrowRPCError{Method: method, Class: "bad-reply"}
	var re *rpcError
	switch msg := err.Error(); {
	case errors.As(err, &re):
		e.Code, e.Class = re.Code, fmt.Sprintf("rpc:%d", re.Code)
	case strings.Contains(msg, "(HTTP 403)"):
		e.Class = "forbidden"
	case strings.Contains(msg, "(HTTP 401)"):
		e.Class = "credentials"
	case strings.Contains(msg, "timed out"):
		e.Class = "timeout"
	case strings.Contains(msg, "unreachable"):
		e.Class = "unreachable"
	}
	return e
}

func (e *escrowBTC) call(ctx context.Context, path, method string, params, out any) error {
	return classifyEscrowRPC(method, e.rpc.call(ctx, path, method, params, out))
}

// chain is getblockchaininfo's chain.
func (e *escrowBTC) chain(ctx context.Context) (string, error) {
	var info struct {
		Chain string `json:"chain"`
	}
	err := e.call(ctx, "", "getblockchaininfo", []any{}, &info)
	return info.Chain, err
}

// readArbiter reads the arbiter wallet's master fingerprint and BIP84 account key without any key export: a
// fresh change address (getrawchangeaddress bech32) and getaddressinfo's hdmasterfingerprint and parent_desc,
// wpkh([fp/84h/1h/0h]tpub…/1/*). The account key must parse as an escrow key with that fingerprint.
func (e *escrowBTC) readArbiter(ctx context.Context) error {
	var addr string
	if err := e.call(ctx, e.arbiterWallet, "getrawchangeaddress", []any{"bech32"}, &addr); err != nil {
		return err
	}
	var info struct {
		IsMine      bool   `json:"ismine"`
		Fingerprint string `json:"hdmasterfingerprint"`
		ParentDesc  string `json:"parent_desc"`
	}
	if err := e.call(ctx, e.arbiterWallet, "getaddressinfo", []any{addr}, &info); err != nil {
		return err
	}
	inner, ok := strings.CutPrefix(info.ParentDesc, "wpkh(")
	if i := strings.Index(inner, "/1/*)"); ok && i > 0 {
		inner = inner[:i]
	} else {
		ok = false
	}
	k, err := parseEscrowKey(inner)
	if !info.IsMine || !ok || err != nil || len(info.Fingerprint) != escrowFingerprintSize || k.Fingerprint != info.Fingerprint {
		return &escrowRPCError{Method: "getaddressinfo", Class: "bad-reply"}
	}
	e.fingerprint, e.arbiterKey = k.Fingerprint, k
	return nil
}

// checkWatchWallet requires the watch-only wallet (getwalletinfo) to be a loaded descriptor wallet with private keys
// disabled: escrow descriptors hold public keys only, and the market's one private key stays in the arbiter wallet.
func (e *escrowBTC) checkWatchWallet(ctx context.Context) error {
	var info struct {
		Descriptors bool `json:"descriptors"`
		PrivateKeys bool `json:"private_keys_enabled"`
	}
	if err := e.call(ctx, e.watchWallet, "getwalletinfo", []any{}, &info); err != nil {
		return err
	}
	if !info.Descriptors || info.PrivateKeys {
		return &escrowRPCError{Method: "getwalletinfo", Class: "not-watch-only"}
	}
	return nil
}

// validAddress asks the node whether addr (already checked by escrowAddressForm, so it cannot close the descriptor)
// is an address on its chain: getdescriptorinfo of addr(<addr>). Core refuses an address of another network.
func (e *escrowBTC) validAddress(ctx context.Context, addr string) (bool, error) {
	_, err := e.descriptorInfo(ctx, "addr("+addr+")")
	var re *escrowRPCError
	if errors.As(err, &re) && re.Class == "rpc:-5" {
		return false, nil
	}
	return err == nil, err
}

// incoming lists what the watch-only wallet received at addresses, one entry per output: listreceivedbyaddress
// (minconf 0, watch-only included) for the transaction IDs and gettransaction for each output's amount and the
// transaction's confirmations (negative when it conflicts), as the pooled adapter reads its wallet.
func (e *escrowBTC) incoming(ctx context.Context, addresses []string) ([]Incoming, error) {
	want := map[string]bool{}
	for _, a := range addresses {
		want[a] = true
	}
	var received []struct {
		Address string   `json:"address"`
		TxIDs   []string `json:"txids"`
	}
	if err := e.call(ctx, e.watchWallet, "listreceivedbyaddress", []any{0, false, true}, &received); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Incoming
	for _, r := range received {
		if !want[r.Address] {
			continue
		}
		for _, txid := range r.TxIDs {
			if seen[txid] {
				continue
			}
			seen[txid] = true
			var tx struct {
				Confirmations json.Number `json:"confirmations"`
				Details       []struct {
					Address  string      `json:"address"`
					Category string      `json:"category"`
					Amount   json.Number `json:"amount"`
					Vout     json.Number `json:"vout"`
				} `json:"details"`
			}
			if err := e.call(ctx, e.watchWallet, "gettransaction", []any{txid, true}, &tx); err != nil {
				return nil, err
			}
			confs, err := tx.Confirmations.Int64()
			if err != nil {
				return nil, &escrowRPCError{Method: "gettransaction", Class: "bad-reply"}
			}
			for _, d := range tx.Details {
				if d.Category != "receive" || !want[d.Address] {
					continue
				}
				amt, err := parseAmount(string(d.Amount), 8)
				if err != nil {
					continue // zero or malformed outputs are not payments
				}
				vout, err := d.Vout.Int64()
				if err != nil {
					return nil, &escrowRPCError{Method: "gettransaction", Class: "bad-reply"}
				}
				out = append(out, Incoming{Address: d.Address, TxID: txid, Index: vout, Amount: amt, Confirmations: confs})
			}
		}
	}
	return out, nil
}

// derivesChild reports whether key derives pub at suffix: deriveaddresses of wpkh(<key><suffix>) and of wpkh(<pub>)
// give the same address only if the child key is pub. It confirms a key registered again after an APP_SECRET_KEY
// rotation is the key an escrow holds, without storing anything that identifies the key outside the HMAC.
func (e *escrowBTC) derivesChild(ctx context.Context, key escrowKey, suffix, pub string) (bool, error) {
	if !validPubKeyHex(pub) {
		return false, nil
	}
	child, err := e.deriveAddresses(ctx, withChecksum("wpkh("+key.Expression()+suffix+")"), -1)
	if err != nil {
		return false, err
	}
	bare, err := e.deriveAddresses(ctx, withChecksum("wpkh("+pub+")"), -1)
	if err != nil {
		return false, err
	}
	return len(child) == 1 && slices.Equal(child, bare), nil
}

// validPubKeyHex is a compressed public key in lower-case hex, as escrow_pubkeys stores it.
func validPubKeyHex(s string) bool {
	return len(s) == 66 && (strings.HasPrefix(s, "02") || strings.HasPrefix(s, "03")) && isLowerHexFold(s) && strings.ToLower(s) == s
}

// escrowOutpoint and escrowPayment are createpsbt's inputs and outputs.
type escrowOutpoint struct {
	TxID string
	Vout uint32
}

type escrowPayment struct {
	Address string
	Sats    int64
}

// createPSBT builds an unsigned PSBT with exactly these inputs and outputs, in this order: createpsbt with lock
// time 0 and replaceable true. Core does no coin selection here.
func (e *escrowBTC) createPSBT(ctx context.Context, ins []escrowOutpoint, outs []escrowPayment) (string, error) {
	inputs := make([]map[string]any, 0, len(ins))
	for _, in := range ins {
		inputs = append(inputs, map[string]any{"txid": in.TxID, "vout": in.Vout})
	}
	outputs := make([]map[string]any, 0, len(outs))
	for _, o := range outs {
		outputs = append(outputs, map[string]any{o.Address: json.Number(amount(o.Sats, 8))})
	}
	var psbt string
	err := e.call(ctx, "", "createpsbt", []any{inputs, outputs, 0, true}, &psbt)
	return psbt, err
}

// utxoUpdatePSBT adds UTXO data and, from the public descriptors, the witness script and BIP32 derivations.
func (e *escrowBTC) utxoUpdatePSBT(ctx context.Context, psbt string, descriptors []string) (string, error) {
	var out string
	err := e.call(ctx, "", "utxoupdatepsbt", []any{psbt, descriptors}, &out)
	return out, err
}

// escrowImport is one importdescriptors request: a watch-only, inactive descriptor.
type escrowImport struct {
	Desc      string
	Timestamp any // "now" or a UNIX time
	Label     string
}

// importDescriptors imports watch-only descriptors into wallet (P1-b: the escrow watch-only wallet). Any request
// that Core reports unsuccessful fails the call.
func (e *escrowBTC) importDescriptors(ctx context.Context, wallet string, reqs []escrowImport) error {
	body := make([]map[string]any, 0, len(reqs))
	for _, r := range reqs {
		body = append(body, map[string]any{"desc": r.Desc, "timestamp": r.Timestamp, "active": false, "label": r.Label})
	}
	var res []struct {
		Success bool `json:"success"`
	}
	if err := e.call(ctx, "/wallet/"+url.PathEscape(wallet), "importdescriptors", []any{body}, &res); err != nil {
		return err
	}
	if len(res) != len(reqs) {
		return &escrowRPCError{Method: "importdescriptors", Class: "bad-reply"}
	}
	for _, r := range res {
		if !r.Success {
			return &escrowRPCError{Method: "importdescriptors", Class: "import-failed"}
		}
	}
	return nil
}

// descriptorInfo is getdescriptorinfo: the descriptor in Core's canonical public form with its checksum (private
// keys replaced by their public keys).
func (e *escrowBTC) descriptorInfo(ctx context.Context, desc string) (public string, err error) {
	var info struct {
		Descriptor string `json:"descriptor"`
	}
	if err = e.call(ctx, "", "getdescriptorinfo", []any{desc}, &info); err == nil && info.Descriptor == "" {
		err = &escrowRPCError{Method: "getdescriptorinfo", Class: "bad-reply"}
	}
	return info.Descriptor, err
}

// deriveAddresses is deriveaddresses for a descriptor with no wildcard (index < 0), or for one index of a ranged
// descriptor (range [index, index]).
func (e *escrowBTC) deriveAddresses(ctx context.Context, desc string, index int64) ([]string, error) {
	params := []any{desc}
	if index >= 0 {
		params = append(params, []int64{index, index})
	}
	var addrs []string
	err := e.call(ctx, "", "deriveaddresses", params, &addrs)
	return addrs, err
}

// txOut is gettxout including the mempool: whether the outpoint is unspent, its confirmations and value.
func (e *escrowBTC) txOut(ctx context.Context, txid string, vout uint32) (found bool, confirmations, sats int64, err error) {
	var out *struct {
		Confirmations int64       `json:"confirmations"`
		Value         json.Number `json:"value"`
	}
	if err = e.call(ctx, "", "gettxout", []any{txid, vout, true}, &out); err != nil || out == nil {
		return false, 0, 0, err
	}
	if sats, err = parseAmount(string(out.Value), 8); err != nil {
		return false, 0, 0, &escrowRPCError{Method: "gettxout", Class: "bad-reply"}
	}
	return true, out.Confirmations, sats, nil
}

// keypoolRefill extends the arbiter wallet's derived key range to size keys per chain, so the arbiter can sign
// for market child indices below it (probed to index 19000 on regtest).
func (e *escrowBTC) keypoolRefill(ctx context.Context, size int) error {
	return classifyEscrowRPC("keypoolrefill", e.rpc.callWithin(ctx, escrowKeypoolTimeout, e.arbiterWallet, "keypoolrefill", []any{size}, nil))
}

// escrowSigned is walletprocesspsbt's and descriptorprocesspsbt's reply.
type escrowSigned struct {
	PSBT     string `json:"psbt"`
	Complete bool   `json:"complete"`
}

// arbiterSign has the arbiter wallet sign psbt: walletprocesspsbt sign=true, sighash "ALL" (never another type),
// BIP32 derivations kept, not finalized. Only the watcher calls it, on the market's own stored copy (P1-c, ES-2).
func (e *escrowBTC) arbiterSign(ctx context.Context, psbt string) (escrowSigned, error) {
	var out escrowSigned
	err := e.call(ctx, e.arbiterWallet, "walletprocesspsbt", []any{psbt, true, escrowSighash, true, false}, &out)
	return out, err
}

// throwawaySign signs psbt with the keys of a private descriptor that holds only throwaway practice keys (ES-9),
// never a wallet key: descriptorprocesspsbt, sighash "ALL", BIP32 derivations kept, not finalized.
func (e *escrowBTC) throwawaySign(ctx context.Context, psbt, privateDescriptor string) (escrowSigned, error) {
	var out escrowSigned
	err := e.call(ctx, "", "descriptorprocesspsbt", []any{psbt, []string{privateDescriptor}, escrowSighash, true, false}, &out)
	return out, err
}

// combinePSBT merges PSBTs of one transaction, in the order given (the market's copy first, ES-2).
func (e *escrowBTC) combinePSBT(ctx context.Context, psbts []string) (string, error) {
	var out string
	err := e.call(ctx, "", "combinepsbt", []any{psbts}, &out)
	return out, err
}

// finalizePSBT finalizes and extracts: the raw transaction hex when complete. Core verifies every signature
// while finalizing, so complete=false also means a signature did not verify.
func (e *escrowBTC) finalizePSBT(ctx context.Context, psbt string) (rawHex string, complete bool, err error) {
	var out struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	err = e.call(ctx, "", "finalizepsbt", []any{psbt, true}, &out)
	return out.Hex, out.Complete, err
}

// feeRateParam is a fee rate in sat/vB as the BTC/kvB number testmempoolaccept and sendrawtransaction take.
func feeRateParam(satPerVByte int64) json.Number { return json.Number(amount(satPerVByte*1000, 8)) }

// testMempoolAccept runs testmempoolaccept on one raw transaction with an explicit maxfeerate (sat/vB).
func (e *escrowBTC) testMempoolAccept(ctx context.Context, rawHex string, maxSatPerVByte int64) (allowed bool, reject string, err error) {
	var out []struct {
		Allowed bool   `json:"allowed"`
		Reject  string `json:"reject-reason"`
	}
	if err = e.call(ctx, "", "testmempoolaccept", []any{[]string{rawHex}, feeRateParam(maxSatPerVByte)}, &out); err != nil {
		return false, "", err
	}
	if len(out) != 1 {
		return false, "", &escrowRPCError{Method: "testmempoolaccept", Class: "bad-reply"}
	}
	return out[0].Allowed, out[0].Reject, nil
}

// sendRawTransaction broadcasts a stored raw transaction with an explicit maxfeerate (sat/vB). Core answers the
// txid again for a transaction it already has; -27 (already in the UTXO set: confirmed) is class rpc:-27, which
// the settlement code treats as sent (ES-2).
func (e *escrowBTC) sendRawTransaction(ctx context.Context, rawHex string, maxSatPerVByte int64) (string, error) {
	var txid string
	err := e.call(ctx, "", "sendrawtransaction", []any{rawHex, feeRateParam(maxSatPerVByte)}, &txid)
	return txid, err
}
