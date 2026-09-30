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
	"combinepsbt", "finalizepsbt", "testmempoolaccept", "sendrawtransaction",
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
}

// escrowConfigFromEnv reads ESCROW_BTC (on/off, blank = off), ESCROW_BTC_RPC_URL and ESCROW_BTC_ARBITER_WALLET
// (default opsecmkt-arbiter). Nothing is contacted.
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
	if strings.TrimSpace(getenv("BITCOIN_RPC_URL")) != "" {
		pooled := strings.TrimSpace(getenv("BITCOIN_WALLET"))
		if pooled == "" {
			pooled = "opsecmkt"
		}
		if pooled == c.arbiterWallet {
			return c, errors.New("ESCROW_BTC_ARBITER_WALLET must name a wallet that holds only the escrow arbiter key, not the pooled wallet BITCOIN_WALLET")
		}
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
	escrowFingerprintPin  = "escrow_btc_arbiter_fingerprint" // settings key
	escrowFingerprintSize = 8
)

// escrowBTC is the escrow adapter; App.escrow is nil unless ESCROW_BTC=on passed every startup check.
type escrowBTC struct {
	rpc           *rpcClient
	arbiterName   string
	arbiterWallet string    // "/wallet/<name>"
	fingerprint   string    // the arbiter wallet's master fingerprint, pinned in settings
	arbiterKey    escrowKey // the arbiter's BIP84 account key; P1-b derives the market's per-escrow child keys from it
}

func newEscrowBTC(c escrowConfig) (*escrowBTC, error) {
	rpc, err := newRPCClient("bitcoin escrow", c.rpcURL, "1.0", false)
	if err != nil {
		return nil, errors.New("ESCROW_BTC_RPC_URL must be http(s)://user:password@host:port")
	}
	return &escrowBTC{rpc: rpc, arbiterName: c.arbiterWallet, arbiterWallet: "/wallet/" + url.PathEscape(c.arbiterWallet)}, nil
}

// initEscrow runs in New after the payment providers. ESCROW_BTC=off does nothing. On, a node that answers on any
// chain but regtest stops startup (errEscrowNotRegtest); an unreachable node, an unreadable arbiter wallet or a
// fingerprint that differs from the pinned one leaves escrow disabled with one fixed line, and the market runs.
func (a *App) initEscrow(ctx context.Context) error {
	cfg, err := escrowConfigFromEnv(os.Getenv)
	if err != nil || !cfg.on {
		return err
	}
	a.escrowConfigured = true
	if a.live {
		return errors.New("refusing to start: ESCROW_BTC=on runs only on regtest in this release, and this market is configured for mainnet")
	}
	e, err := newEscrowBTC(cfg)
	if err != nil {
		return err
	}
	chain, err := e.chain(ctx)
	if err != nil {
		log.Printf("%s (%s)", escrowLogUnchecked, errorClassOf(err))
		return nil
	}
	if chain != "regtest" {
		return errEscrowNotRegtest
	}
	if err = e.readArbiter(ctx); err != nil {
		log.Printf("%s (%s)", escrowLogArbiter, errorClassOf(err))
		return nil
	}
	ok, err := pinArbiterFingerprint(ctx, a.db, e.fingerprint)
	if err != nil {
		return err
	}
	if !ok {
		log.Print(escrowLogFingerprint)
		return nil
	}
	a.escrow = e
	log.Print(escrowLogOn)
	return nil
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
