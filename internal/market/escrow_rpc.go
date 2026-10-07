package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Bitcoin escrow (WR-escrow Phase 1, A-246): configuration, the startup checks and the Bitcoin Core RPC adapter
// the escrow code uses. It is apart from the pooled adapter (payments_bitcoin.go) and shares only rpcClient.
//
// ESCROW_BTC=on (off by default) runs only against a node on regtest, signet or testnet4 (the chain table, escrowChains;
// A-443 opened signet and testnet4 once the sync guard, the chain pin, the market key self-check and the Tor-mode gate
// were in place). The app reaches Core as its own RPC user, which the operator restricts with rpcwhitelist to the
// methods below (docs/operator-guide.md, "Bitcoin escrow (test networks only)"). listdescriptors is deliberately not
// among them: rpcwhitelist allows or refuses a method, not its arguments, and a user allowed listdescriptors can call it
// with private=true and export the arbiter's private key (probed on Core 31.1 regtest). The arbiter's fingerprint is
// read with getrawchangeaddress and getaddressinfo instead, which export nothing.
//
// Every RPC error is reduced to a fixed class (escrowRPCError) before anyone logs it: Core's error text can quote
// a descriptor or a key (A-170, ES-10).

// escrowRPCMethods is every method the adapter calls: the rpcwhitelist line the operator guide documents.
// getnetworkinfo (read-only: the peer count for the sync guard, A-439, and the proxies and version for the Tor-mode
// gate, A-441) is called only on a public chain or in APP_MODE=tor, so a clearnet regtest whitelist written before it
// keeps working. listsinceblock (read-only) reads the arbiter wallet's entries for the start check (QA4W4A-1,
// checkArbiterWallet).
var escrowRPCMethods = []string{
	"getblockchaininfo", "getnetworkinfo", "getrawchangeaddress", "getaddressinfo", "createpsbt", "utxoupdatepsbt",
	"importdescriptors", "getdescriptorinfo", "deriveaddresses", "gettxout", "keypoolrefill", "walletprocesspsbt",
	"descriptorprocesspsbt", "combinepsbt", "finalizepsbt", "testmempoolaccept", "sendrawtransaction", "getwalletinfo",
	"listreceivedbyaddress", "gettransaction", "listtransactions", "listsinceblock",
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
	feeRate       int64  // ESCROW_BTC_FEE_RATE: the network fee rate of every settlement, in sat/vB (P1-c)
	signerSocket  string // ESCROW_SIGNER_SOCKET (A-454): the escrow signer's Unix socket; "" = no signer
	pooledURL     string // BITCOIN_RPC_URL: the pooled client the market key self-check goes through (A-440); "" = none
}

// escrowConfigFromEnv reads ESCROW_BTC (on/off, blank = off), ESCROW_BTC_RPC_URL, ESCROW_BTC_ARBITER_WALLET
// (default opsecmkt-arbiter), ESCROW_BTC_WATCH_WALLET (default opsecmkt-escrow-watch), ESCROW_BTC_FEE_BPS (0 to
// 10000, default 0), ESCROW_BTC_FEE_ADDRESS (needed when the fee is above 0), ESCROW_BTC_FEE_RATE (the settlement
// network fee rate in sat/vB, 1 to escrowMaxFeeRate, default escrowDefaultFeeRate), PAYMENT_CONFIRMATIONS_BTC (1
// to 1000, default 3) and ESCROW_SIGNER_SOCKET (the escrow signer's socket, an absolute path; blank = none, A-454).
// Nothing is contacted.
func escrowConfigFromEnv(getenv func(string) string) (escrowConfig, error) {
	var c escrowConfig
	switch v := strings.TrimSpace(getenv("ESCROW_BTC")); v {
	case "", "off":
		return c, nil
	case "on":
		c.on = true
	default:
		return c, errors.New(`ESCROW_BTC must be "on" or "off" (blank means off); Bitcoin escrow runs on test networks only (regtest, signet or testnet4)`)
	}
	c.rpcURL = strings.TrimSpace(getenv("ESCROW_BTC_RPC_URL"))
	if c.rpcURL == "" {
		return c, errors.New("ESCROW_BTC=on needs ESCROW_BTC_RPC_URL: the Bitcoin Core RPC URL (regtest, signet or testnet4) of the app's rpcwhitelist user (docs/operator-guide.md, \"Bitcoin escrow (test networks only)\")")
	}
	c.arbiterWallet = strings.TrimSpace(getenv("ESCROW_BTC_ARBITER_WALLET"))
	if c.arbiterWallet == "" {
		c.arbiterWallet = "opsecmkt-arbiter"
	}
	if chain := strings.TrimSpace(getenv("BITCOIN_CHAIN")); chain != "" && escrowChains[chain] == "" {
		return c, errors.New("refusing to start: ESCROW_BTC=on runs only on regtest, signet or testnet4, and BITCOIN_CHAIN names mainnet or testnet3. Set ESCROW_BTC=off, or use one of those chains")
	}
	c.watchWallet = strings.TrimSpace(getenv("ESCROW_BTC_WATCH_WALLET"))
	if c.watchWallet == "" {
		c.watchWallet = "opsecmkt-escrow-watch"
	}
	if c.watchWallet == c.arbiterWallet {
		return c, errors.New("ESCROW_BTC_WATCH_WALLET must name a watch-only wallet of its own, not the arbiter wallet ESCROW_BTC_ARBITER_WALLET")
	}
	if c.pooledURL = strings.TrimSpace(getenv("BITCOIN_RPC_URL")); c.pooledURL != "" {
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
		if c.feeAddress, ok = escrowAddressForm("", getenv("ESCROW_BTC_FEE_ADDRESS")); !ok {
			return c, errors.New("ESCROW_BTC_FEE_BPS above 0 needs ESCROW_BTC_FEE_ADDRESS: the Bitcoin address, on the escrow node's chain, the market fee is paid to")
		}
	}
	c.feeRate = escrowDefaultFeeRate
	if v := strings.TrimSpace(getenv("ESCROW_BTC_FEE_RATE")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > escrowMaxFeeRate {
			return c, fmt.Errorf("ESCROW_BTC_FEE_RATE must be a whole number of sat/vB from 1 to %d: the network fee rate of escrow settlements", escrowMaxFeeRate)
		}
		c.feeRate = n
	}
	c.confirmations = 3
	if v := strings.TrimSpace(getenv("PAYMENT_CONFIRMATIONS_BTC")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 1000 {
			return c, errors.New("PAYMENT_CONFIRMATIONS_BTC must be a whole number from 1 to 1000")
		}
		c.confirmations = n
	}
	if _, err := parseMaxTipAge(getenv("BITCOIN_MAX_TIP_AGE")); err != nil {
		return c, err
	}
	if v := strings.TrimSpace(getenv("ESCROW_SIGNER_SOCKET")); v != "" {
		if !filepath.IsAbs(v) {
			return c, errors.New("ESCROW_SIGNER_SOCKET must be the absolute path of the escrow signer's Unix socket, as mounted in the app's container (docs/operator-guide.md, \"Escrow signer\")")
		}
		c.signerSocket = v
	}
	return c, nil
}

// errEscrowChain is the fixed startup refusal for an escrow node on a chain that is not in the chain table (escrowChains):
// mainnet, testnet3 or any other.
var errEscrowChain = errors.New("refusing to start: ESCROW_BTC=on runs only on regtest, signet or testnet4, and the node at ESCROW_BTC_RPC_URL reports another chain (mainnet and testnet3 are refused). Set ESCROW_BTC=off, or point ESCROW_BTC_RPC_URL at a node on one of those chains")

// Fixed operator lines for an escrow that is switched on but not usable: nothing in them comes from the node.
const (
	escrowLogOn           = "escrow: Bitcoin escrow on (test networks only)"
	escrowLogUnchecked    = "escrow: Bitcoin escrow disabled: the escrow node could not be checked at startup"
	escrowLogArbiter      = "escrow: Bitcoin escrow disabled: the arbiter wallet could not be read (is ESCROW_BTC_ARBITER_WALLET loaded, and a descriptor wallet with private keys?)"
	escrowLogFingerprint  = "escrow: Bitcoin escrow disabled: the arbiter wallet's key fingerprint differs from the one pinned in this database; see docs/operator-guide.md, \"Bitcoin escrow (test networks only)\""
	escrowLogWatch        = "escrow: Bitcoin escrow disabled: the watch-only wallet could not be read (is ESCROW_BTC_WATCH_WALLET loaded, and a descriptor wallet with private keys disabled?)"
	escrowLogArbiterUse   = "escrow: Bitcoin escrow disabled: the arbiter wallet must be used for the escrow key only (an unencrypted descriptor wallet with private keys that has never spent); see docs/operator-guide.md, \"Bitcoin escrow (test networks only)\""
	escrowLogArbiterRead  = "escrow: Bitcoin escrow disabled: the arbiter wallet's transactions could not be read to check that it has never spent (is listsinceblock on the escrow user's rpcwhitelist, and the node answering?); the read is retried every poll, or after a wait when it took 5 s or more or timed out (1 minute, doubling up to 1 hour; restarting the app reads at once); see docs/operator-guide.md, \"Bitcoin escrow (test networks only)\""
	escrowLogArbiterWait  = "escrow: Bitcoin escrow off while the arbiter wallet is checked: reading its transactions takes longer than startup allows; the read goes on, and the escrow watcher starts escrow once it has passed"
	escrowLogFixedFee     = "escrow: the market fee is paid to one fixed address (ESCROW_BTC_FEE_ADDRESS); allowed on test networks only"
	escrowLogRefused      = "escrow: Bitcoin escrow disabled until the app is restarted: the escrow node is on a chain escrow does not run on (mainnet, testnet3 or another), or it refused ESCROW_BTC_FEE_ADDRESS"
	escrowLogUnrecorded   = "escrow: Bitcoin escrow disabled: its arbiter fingerprint could not be checked in the database"
	escrowLogChain        = "escrow: Bitcoin escrow disabled: the escrow node is not on the chain this database's escrows are on (escrow_btc_chain); the admin page names both; see docs/operator-guide.md, \"Bitcoin escrow (test networks only)\""
	escrowLogPooledKey    = "escrow: Bitcoin escrow disabled: the pooled Bitcoin RPC user (BITCOIN_RPC_URL) was not refused listdescriptors, gethdkeys or walletprocesspsbt on the arbiter wallet, so it may be able to export or sign with the market's escrow key; give that user the pooled rpcwhitelist line, or run escrow on a node of its own; see docs/operator-guide.md, \"The pooled RPC user\""
	escrowFingerprintPin  = "escrow_btc_arbiter_fingerprint" // settings key
	escrowChainPin        = "escrow_btc_chain"               // settings key: escrowChainID of the node the database's escrows are on (A-439)
	escrowArbiterChecked  = "escrow_btc_arbiter_checked"     // settings key: the arbiter wallet check's checkpoint (QA4W4A-1, arbiterCheckpoint)
	escrowFingerprintSize = 8
	escrowChainPinLock    = 782497 // the transaction advisory lock pinEscrowChain takes (the watchers' are 782494 to 782496)
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
	feeRate       int64  // sat/vB, for every settlement this process builds
	network       string // the node's chain (getblockchaininfo) as startEscrow read it; names the network in notes
	chainID       string // escrowChainID of the node as startEscrow read it, equal to the pin; each watcher pass compares it
}

func newEscrowBTC(c escrowConfig) (*escrowBTC, error) {
	rpc, err := newRPCClient("bitcoin escrow", c.rpcURL, "1.0", false)
	if err != nil {
		return nil, errors.New("ESCROW_BTC_RPC_URL must be http(s)://user:password@host:port")
	}
	return &escrowBTC{rpc: rpc, arbiterName: c.arbiterWallet, arbiterWallet: "/wallet/" + url.PathEscape(c.arbiterWallet),
		watchName: c.watchWallet, watchWallet: "/wallet/" + url.PathEscape(c.watchWallet), feeBps: c.feeBps, feeAddress: c.feeAddress,
		confirmations: c.confirmations, feeRate: c.feeRate}, nil
}

// initEscrow runs in New after the payment providers. ESCROW_BTC=off does nothing. On, a node that answers on a chain
// escrow does not run on stops startup (errEscrowChain); an unreachable node, an unreadable arbiter wallet or a
// fingerprint that differs from the pinned one leaves escrow disabled with one fixed line, and the market runs; the
// escrow watcher then retries (retryEscrow).
func (a *App) initEscrow(ctx context.Context) error {
	cfg, err := escrowConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if !cfg.on {
		a.notePinnedNet(ctx) // escrow orders' pages are shown with ESCROW_BTC off too, naming their chain (A-443)
		return nil
	}
	a.escrowConfigured = true
	if a.live {
		return errors.New("refusing to start: ESCROW_BTC=on runs only on test networks (regtest, signet or testnet4), and this market is configured for mainnet")
	}
	if _, err = newEscrowBTC(cfg); err != nil {
		return err
	}
	a.escrowCfg = cfg
	state, class, err := a.startEscrow(ctx)
	if err == nil {
		logEscrowState(state, class)
		a.escrowStartState = state
		if cfg.signerSocket != "" {
			a.escrowSigner = &escrowSignerClient{socket: cfg.signerSocket}
			a.checkEscrowSigner(ctx) // a signer that does not answer is "waiting": startup goes on
		}
	}
	return err
}

// errEscrowFeeAddress is the fixed startup refusal for a fee address the escrow node does not accept.
var errEscrowFeeAddress = errors.New("refusing to start: ESCROW_BTC_FEE_ADDRESS is not a valid address on the escrow node's chain")

// startEscrow runs every escrow check once and, when all pass, enables escrow (a.escrow). It reports the state as
// one of the fixed lines (escrowLogOn when enabled) with the error class that caused it (""), or an error: a refusal
// (errEscrowChain, errEscrowFeeAddress) or a database failure. Nothing here comes from the node's text. The
// guards are the same at startup and on a retry: the node must be on a chain of the chain table (regtest, signet or
// testnet4) and on the chain pinned for the database's escrows (pinEscrowChain, before anything else is written), the
// arbiter fingerprint must match the pinned one, and the watch-only wallet must hold no private keys. The configured fee
// address is lowered by the node's chain (escrowAddressForm) before the node checks it.
func (a *App) startEscrow(ctx context.Context) (state, class string, err error) {
	e, err := newEscrowBTC(a.escrowCfg)
	if err != nil {
		return "", "", err
	}
	a.escrowMismatch.Store(nil)    // shown on /admin only while the latest attempt found the node on another chain
	a.escrowPooledKey.Store(nil)   // likewise, while the latest attempt found the pooled user not refused (A-440)
	a.escrowArbiterRead.Store(nil) // likewise, while the latest attempt could not finish reading the arbiter wallet (QA4W4A-1)
	node, err := e.blockchainInfo(ctx)
	if err != nil {
		a.notePinnedNet(ctx)
		return escrowLogUnchecked, errorClassOf(err), nil
	}
	if escrowChains[node.Chain] == "" {
		return "", "", errEscrowChain
	}
	e.network, e.chainID = node.Chain, escrowChainID(node.Chain, node.SignetChallenge)
	pinned, ok, err := pinEscrowChain(ctx, a.db, e.chainID)
	if err != nil {
		return "", "", err
	}
	// The chain escrowNetwork names while escrow is not enabled: the node's, unless the database's escrows are on another,
	// the pinned one (none pinned: escrows from before the pin, which ran on regtest only).
	net := node.Chain
	if !ok {
		net = "regtest"
		if p, _, _ := strings.Cut(pinned, ":"); escrowChains[p] != "" {
			net = p
		}
	}
	a.escrowNet.Store(&net)
	if !ok {
		a.escrowMismatch.Store(&EscrowChainMismatch{Pinned: pinned, Reported: e.chainID})
		return escrowLogChain, "", nil
	}
	if class := e.pooledKeyCheck(ctx, a.escrowCfg.pooledURL); class != "" {
		a.escrowPooledKey.Store(&class)
		return escrowLogPooledKey, class, nil
	}
	if err = e.readArbiter(ctx); err != nil {
		return escrowLogArbiter, errorClassOf(err), nil
	}
	if ok, err = pinArbiterFingerprint(ctx, a.db, e.fingerprint); err != nil {
		return "", "", err
	}
	if !ok {
		return escrowLogFingerprint, "", nil
	}
	if state, class, err := a.checkArbiterWallet(ctx, e); state != "" || err != nil {
		return state, class, err
	}
	if err = e.checkWatchWallet(ctx); err != nil {
		return escrowLogWatch, errorClassOf(err), nil
	}
	if e.feeAddress != "" {
		e.feeAddress, _ = escrowAddressForm(e.network, e.feeAddress)
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

// pooledKeyCheck is the market key protection self-check (P4-R6, A-440). rpcwhitelist is per user, not per wallet,
// and Core selects the wallet by the URL path, so a pooled user allowed listdescriptors could call it with
// private=true on the arbiter wallet's path and print the market's escrow key (probed on Core 31.1 regtest); Core 31.1
// also exports it with gethdkeys {"private":true} and signs with it through walletprocesspsbt (QA4W3-1). Through the
// pooled client (pooledURL, BITCOIN_RPC_URL), on that path, it calls each of pooledKeyProbes in turn with arguments
// that export and sign nothing (Core checks the whitelist, and then the wallet, before it reads them: an allowed
// walletprocesspsbt answers -22 for the empty PSBT). It reports "" when every one is refused: HTTP 403 (the method is
// not on the pooled user's rpcwhitelist) or RPC -18 (no such wallet: the pooled node is another node). Otherwise it
// reports the first method that was not refused and the class escrow stays disabled with, as "<method> <class>":
// "answered" for a success, otherwise the escrowRPCError class (credentials, timeout, rpc:<code>, ...), never Core's
// text. With no pooled client configured there is nothing to check.
func (e *escrowBTC) pooledKeyCheck(ctx context.Context, pooledURL string) string {
	if pooledURL == "" {
		return ""
	}
	rpc, err := newRPCClient("bitcoin", pooledURL, "1.0", false)
	if err != nil {
		return "bad-url"
	}
	for _, p := range pooledKeyProbes {
		err = classifyEscrowRPC(p.method, rpc.call(ctx, e.arbiterWallet, p.method, p.params, nil))
		var re *escrowRPCError
		switch {
		case err == nil:
			return p.method + " answered"
		case !errors.As(err, &re):
			return p.method + " " + errorCause(err)
		case re.Class != "forbidden" && re.Class != "rpc:-18":
			return p.method + " " + re.Class
		}
	}
	return ""
}

// pooledKeyProbes are the calls pooledKeyCheck makes, in order: none exports a private key or signs anything.
var pooledKeyProbes = []struct {
	method string
	params []any
}{
	{"listdescriptors", []any{false}},
	{"gethdkeys", []any{map[string]any{"private": false}}},
	{"walletprocesspsbt", []any{"", false}},
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
	last     string
	refused  bool      // the node answered on another chain, or refused the fee address: no retry until a restart
	pooledAt time.Time // when the pooled key self-check last ran while escrow was on (zero: not yet seen on)
}

// escrowPooledRecheck is how often the watcher runs the pooled key self-check again while escrow is on (QA4W3-8).
const escrowPooledRecheck = 24 * time.Hour

// retryEscrow runs startEscrow again for the escrow watcher while escrow is switched on but not enabled (A-316), as
// refreshProviders retries a pooled provider. A refusal that would stop startup cannot stop a running market: it logs
// one fixed line and ends the retries until a restart. While escrow is on it runs the pooled key self-check again
// (pooledKeyCheck) once escrowPooledRecheck has passed since the last one (QA4W3-8), so a pooled whitelist widened
// after startup is found within a day: then escrow is disabled, as startEscrow would leave it (the key may be
// exportable; disabling is the safer default), with the same line and admin alert, and retried like any other check.
// Its read of the arbiter wallet's entries is paced after a slow one (A-479, checkArbiterWallet).
func (a *App) retryEscrow(ctx context.Context, r *escrowRetry) {
	if r.refused {
		return
	}
	if e := a.escrow.Load(); e != nil {
		switch {
		case r.pooledAt.IsZero(): // escrow came on at startup, which ran the self-check
			r.pooledAt = time.Now()
		case time.Since(r.pooledAt) >= escrowPooledRecheck:
			pass, cancel := context.WithTimeout(ctx, watcherPassTimeout)
			class := e.pooledKeyCheck(pass, a.escrowCfg.pooledURL)
			cancel()
			if ctx.Err() != nil {
				return
			}
			r.pooledAt = time.Now()
			if class != "" {
				a.escrowPooledKey.Store(&class)
				a.escrow.CompareAndSwap(e, nil)
				logEscrowState(escrowLogPooledKey, class)
				r.last = escrowLogPooledKey
			}
		}
		return
	}
	pass, cancel := context.WithTimeout(ctx, watcherPassTimeout)
	state, class, err := a.startEscrow(pass)
	cancel()
	switch {
	case ctx.Err() != nil:
		return
	case errors.Is(err, errEscrowChain), errors.Is(err, errEscrowFeeAddress):
		r.refused, state = true, escrowLogRefused
	case err != nil:
		state, class = escrowLogUnrecorded, errorCause(err)
	}
	if state == escrowLogOn {
		r.pooledAt = time.Now() // startEscrow has just run the self-check
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

// escrowChainID is the identity escrow pins for a node (P4-R4): its chain name, plus its signet_challenge on signet, so
// a private signet (which shares public signet's genesis block) is not taken for another signet.
func escrowChainID(chain, signetChallenge string) string {
	if chain == "signet" {
		return chain + ":" + signetChallenge
	}
	return chain
}

// notePinnedNet sets the chain escrowNetwork names while escrow is not enabled (ESCROW_BTC off, or no escrow node has
// answered yet in this process): the chain this database's escrows are pinned to (escrowChainPin), if any, else regtest
// when it holds Bitcoin escrows (from before the pin, when escrow ran on regtest only). A database with neither leaves
// it unset, so pages say the chain is not verified yet (escrowNetKnown, QA4W6-8). A failed read sets nothing.
func (a *App) notePinnedNet(ctx context.Context) {
	if a.db == nil || a.escrowNet.Load() != nil {
		return
	}
	var pinned string
	var held bool
	if a.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key=$1),''),EXISTS (SELECT 1 FROM escrows WHERE coin='BTC')`,
		escrowChainPin).Scan(&pinned, &held) != nil {
		return
	}
	if pinned == "" && held {
		pinned = "regtest"
	}
	if chain, _, _ := strings.Cut(pinned, ":"); escrowChains[chain] != "" {
		a.escrowNet.Store(&chain)
	}
}

// pinEscrowChain checks id (escrowChainID of the escrow node) against the chain pinned in settings (escrowChainPin)
// and reports the pinned value and whether escrow may use the node. While the database holds no Bitcoin escrow the pin
// is written, or rewritten, to id: a database may change chain until it holds an escrow, never after (a new chain
// needs a new database). A database that holds escrows and no pin (one from before A-439) is pinned when the node
// reports regtest, the only chain escrow ran on before; on any other chain it is refused. A refusal writes nothing. One
// transaction under escrowChainPinLock, so concurrent startups agree.
func pinEscrowChain(ctx context.Context, db *sql.DB, id string) (pinned string, ok bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", escrowChainPinLock); err != nil {
		return "", false, err
	}
	var held bool
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key=$1),''),EXISTS (SELECT 1 FROM escrows WHERE coin='BTC')`,
		escrowChainPin).Scan(&pinned, &held); err != nil {
		return "", false, err
	}
	switch {
	case pinned == id:
		return pinned, true, nil
	case held && (pinned != "" || id != "regtest"):
		return pinned, false, nil
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=excluded.value", escrowChainPin, id); err != nil {
		return "", false, err
	}
	return id, true, tx.Commit()
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

// blockchainInfo is getblockchaininfo: the chain, signet_challenge and what the sync guard reads (bitcoinNode;
// Connections is not set).
func (e *escrowBTC) blockchainInfo(ctx context.Context) (bitcoinNode, error) {
	var info bitcoinNode
	err := e.call(ctx, "", "getblockchaininfo", []any{}, &info)
	return info, err
}

// networkInfo is getnetworkinfo: the node's peer count, for the sync guard on a public chain, and its version and
// proxies, for the Tor-mode gate (A-441, escrow_stall.go).
func (e *escrowBTC) networkInfo(ctx context.Context) (escrowNetInfo, error) {
	var info escrowNetInfo
	err := e.call(ctx, "", "getnetworkinfo", []any{}, &info)
	return info, err
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
	u, err := e.utxo(ctx, txid, vout)
	return u.Found, u.Confirmations, u.Sats, err
}

// escrowUTXO is one gettxout answer; Found is false for an outpoint that is spent (in a block or in the mempool)
// or never existed.
type escrowUTXO struct {
	Found         bool
	Confirmations int64
	Sats          int64
	Address       string // scriptPubKey.address ("" for a script without one)
}

// utxo is gettxout including the mempool: the outpoint's confirmations, value and address, as a settlement build
// checks each input before it asks for a PSBT (ES-2).
func (e *escrowBTC) utxo(ctx context.Context, txid string, vout uint32) (escrowUTXO, error) {
	var out *struct {
		Confirmations int64       `json:"confirmations"`
		Value         json.Number `json:"value"`
		ScriptPubKey  struct {
			Address string `json:"address"`
		} `json:"scriptPubKey"`
	}
	if err := e.call(ctx, "", "gettxout", []any{txid, vout, true}, &out); err != nil || out == nil {
		return escrowUTXO{}, err
	}
	sats, err := parseAmount(string(out.Value), 8)
	if err != nil || out.Confirmations < 0 {
		return escrowUTXO{}, &escrowRPCError{Method: "gettxout", Class: "bad-reply"}
	}
	return escrowUTXO{Found: true, Confirmations: out.Confirmations, Sats: sats, Address: out.ScriptPubKey.Address}, nil
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

// escrowWalletTx is what the watch-only wallet knows of a settlement transaction (gettransaction): whether it knows
// it, its confirmations (negative when a transaction in the chain conflicts with it) and the conflicting txids.
type escrowWalletTx struct {
	Found         bool
	Confirmations int64
	Conflicts     []string
	Hex           string // the transaction as the wallet holds it (with its witnesses)
}

// walletTx is gettransaction of txid in the watch-only wallet, which records every transaction spending an escrow
// output it watches. A transaction the wallet does not know (-5) is Found false.
func (e *escrowBTC) walletTx(ctx context.Context, txid string) (escrowWalletTx, error) {
	var tx struct {
		Confirmations json.Number `json:"confirmations"`
		Conflicts     []string    `json:"walletconflicts"`
		Hex           string      `json:"hex"`
	}
	err := e.call(ctx, e.watchWallet, "gettransaction", []any{txid, true}, &tx)
	var re *escrowRPCError
	if errors.As(err, &re) && re.Class == "rpc:-5" {
		return escrowWalletTx{}, nil
	}
	if err != nil {
		return escrowWalletTx{}, err
	}
	confs, err := tx.Confirmations.Int64()
	if err != nil {
		return escrowWalletTx{}, &escrowRPCError{Method: "gettransaction", Class: "bad-reply"}
	}
	return escrowWalletTx{Found: true, Confirmations: confs, Conflicts: tx.Conflicts, Hex: tx.Hex}, nil
}

// escrowSpenderScan is how many of the watch-only wallet's latest entries walletSpenders reads.
const escrowSpenderScan = 100

// walletSpenders names, for each outpoint ("txid:vout") in want, the transaction in the watch-only wallet that spends
// it. The wallet records every transaction spending an output it watches; listtransactions lists them as category
// "send" (its latest escrowSpenderScan entries are read), and each such transaction is decoded from gettransaction's
// hex. An outpoint no listed transaction spends is not in the answer. Both methods read the watch-only wallet only.
func (e *escrowBTC) walletSpenders(ctx context.Context, want map[string]bool) (map[string]string, error) {
	var list []struct {
		TxID     string `json:"txid"`
		Category string `json:"category"`
	}
	if err := e.call(ctx, e.watchWallet, "listtransactions", []any{"*", escrowSpenderScan, 0, true}, &list); err != nil {
		return nil, err
	}
	out := map[string]string{}
	seen := map[string]bool{}
	for _, l := range list {
		if l.Category != "send" || seen[l.TxID] || firstTxID([]string{l.TxID}) == "" || len(out) == len(want) {
			continue
		}
		seen[l.TxID] = true
		w, err := e.walletTx(ctx, l.TxID)
		if err != nil {
			return nil, err
		}
		raw, err := hex.DecodeString(w.Hex)
		if err != nil {
			continue
		}
		t, err := parseSignedTx(raw)
		if err != nil {
			continue // not a segwit spend: not one of an escrow output
		}
		for _, in := range t.In {
			id := in.TxID
			slices.Reverse(id[:])
			if k := hex.EncodeToString(id[:]) + ":" + strconv.FormatUint(uint64(in.Vout), 10); want[k] {
				out[k] = l.TxID
			}
		}
	}
	return out, nil
}

// escrowWatched is one address in the watch-only wallet's address book: its label and what the wallet says it
// received in total.
type escrowWatched struct {
	Address, Label string
	Received       int64
}

// watchedAddresses lists every address the watch-only wallet holds, received anything or not: listreceivedbyaddress
// with minconf 0, include_empty and include_watchonly. Each escrow descriptor is imported with the label
// "escrow:<order ID>" (escrow_create.go), which puts its address in the wallet's address book, so this lists every
// escrow the wallet watches without listdescriptors, which can export private keys (probed on Core 31.1 regtest: a
// labelled watch-only import is listed with amount 0 before it is funded).
func (e *escrowBTC) watchedAddresses(ctx context.Context) ([]escrowWatched, error) {
	var list []struct {
		Address string      `json:"address"`
		Label   string      `json:"label"`
		Amount  json.Number `json:"amount"`
	}
	if err := e.call(ctx, e.watchWallet, "listreceivedbyaddress", []any{0, true, true}, &list); err != nil {
		return nil, err
	}
	out := make([]escrowWatched, 0, len(list))
	for _, l := range list {
		sats, err := parseAmount(string(l.Amount), 8)
		if err != nil {
			sats = 0 // nothing received ("0.00000000"), which parseAmount refuses
		}
		out = append(out, escrowWatched{Address: l.Address, Label: l.Label, Received: sats})
	}
	return out, nil
}

// watchKnows reports whether the watch-only wallet already holds addr (getaddressinfo on that wallet): an escrow
// address it watches was handed out before, by an escrow a restored database no longer has (A-359). Bitcoin Core 31.1
// reports an imported watch-only escrow address as ismine, with the "escrow:<order ID>" label it was imported with
// (iswatchonly is always false in a descriptor wallet), and an address it does not hold as ismine false with no label
// (probed on regtest). Either ismine or a label counts as known; a reply without ismine is a bad-reply.
func (e *escrowBTC) watchKnows(ctx context.Context, addr string) (bool, error) {
	var info struct {
		IsMine *bool    `json:"ismine"`
		Labels []string `json:"labels"`
	}
	if err := e.call(ctx, e.watchWallet, "getaddressinfo", []any{addr}, &info); err != nil {
		return false, err
	}
	if info.IsMine == nil {
		return false, &escrowRPCError{Method: "getaddressinfo", Class: "bad-reply"}
	}
	return *info.IsMine || len(info.Labels) > 0, nil
}

// watchKeys reads the three public keys of an escrow address the watch-only wallet holds (getaddressinfo on that
// wallet), for the orphan sweep (P2-R10, A-366). Bitcoin Core 31.1 reports an imported bare-key wsh(sortedmulti(2,…))
// with pubkeys (the keys in script order), desc (wsh(multi(2,[fingerprint]key,…)), in script order) and its
// scriptPubKey (probed on regtest). The keys come from pubkeys, or else from desc, and are returned only when each is a
// compressed key on the curve, they are distinct, and their 2-of-3 witness script hashes to the scriptPubKey, which
// must be the asked address's own: so they are the address's own keys. An address the wallet does not hold (ismine false, labelled or not: the wallet has no
// script to read keys from) and one that is not P2WSH (never an escrow) have none (nil, nil). Any other reply is a
// bad-reply class, never read as "no keys".
func (e *escrowBTC) watchKeys(ctx context.Context, addr string) ([][]byte, error) {
	var info struct {
		IsMine       *bool    `json:"ismine"`
		ScriptPubKey string   `json:"scriptPubKey"`
		PubKeys      []string `json:"pubkeys"`
		Desc         string   `json:"desc"`
	}
	if err := e.call(ctx, e.watchWallet, "getaddressinfo", []any{addr}, &info); err != nil {
		return nil, err
	}
	bad := &escrowRPCError{Method: "getaddressinfo", Class: "bad-reply"}
	if info.IsMine == nil {
		return nil, bad
	}
	if !*info.IsMine {
		return nil, nil
	}
	spk, err := hex.DecodeString(info.ScriptPubKey)
	if err != nil || len(spk) == 0 {
		return nil, bad
	}
	// The reply must be about the address asked: keys that hash to another address's script (a misrouted or lying
	// reply) are never reserved in its place (QA-P2e-1).
	if want, err := escrowAddressScript(e.network, addr); err != nil || !bytes.Equal(spk, want) {
		return nil, bad
	}
	if len(spk) != 34 || spk[0] != 0 || spk[1] != 32 {
		return nil, nil
	}
	texts, sorted := info.PubKeys, false
	if texts == nil {
		var ok bool
		if texts, sorted, ok = multisigKeys(info.Desc); !ok {
			return nil, bad
		}
	}
	if len(texts) != 3 {
		return nil, bad
	}
	keys := make([][]byte, 0, 3)
	for _, s := range texts {
		k, err := hex.DecodeString(s)
		if err != nil || len(k) != 33 || !onCurve(k) || slices.ContainsFunc(keys, func(have []byte) bool { return bytes.Equal(have, k) }) {
			return nil, bad
		}
		keys = append(keys, k)
	}
	if sorted {
		slices.SortFunc(keys, bytes.Compare)
	}
	ws := []byte{0x52}
	for _, k := range keys {
		ws = append(append(ws, 33), k...)
	}
	sum := sha256.Sum256(append(ws, 0x53, 0xae))
	if !bytes.Equal(spk[2:], sum[:]) {
		return nil, bad
	}
	return keys, nil
}

// multisigKeys reads the three key texts of a descriptor wsh(multi(2,a,b,c)) or wsh(sortedmulti(2,a,b,c)), with or
// without its checksum, each key's origin "[…]" dropped; sorted reports sortedmulti (the script sorts the keys).
func multisigKeys(desc string) (keys []string, sorted, ok bool) {
	desc, _, _ = strings.Cut(desc, "#")
	inner, isMulti := strings.CutPrefix(desc, "wsh(multi(2,")
	if !isMulti {
		inner, sorted = strings.CutPrefix(desc, "wsh(sortedmulti(2,")
	}
	inner, closed := strings.CutSuffix(inner, "))")
	if !isMulti && !sorted || !closed {
		return nil, false, false
	}
	keys = strings.Split(inner, ",")
	for i, k := range keys {
		if strings.HasPrefix(k, "[") {
			_, k, _ = strings.Cut(k, "]")
		}
		keys[i] = k
	}
	return keys, sorted, len(keys) == 3
}

// arbiterOwns reports whether the arbiter wallet holds the key of addr (getaddressinfo ismine): for the P2WPKH address
// of a market child key, whether the wallet's derived key range covers that child, so it can sign for it.
func (e *escrowBTC) arbiterOwns(ctx context.Context, addr string) (bool, error) {
	var info struct {
		IsMine bool `json:"ismine"`
	}
	err := e.call(ctx, e.arbiterWallet, "getaddressinfo", []any{addr}, &info)
	return info.IsMine, err
}

// escrowArbiterReadTimeout bounds one read of the arbiter wallet's entries (arbiterSends). Core 31.1 answered
// listsinceblock over 100,000 entries in about 5 s (71 MB) on a loaded regtest host. A variable only so tests can
// shorten it.
var escrowArbiterReadTimeout = 5 * time.Minute

// The pacing of the arbiter wallet reads (A-479; paceArbiter). While a payer holds N fee bumps of a payment to the
// market child's address unconfirmed, Core lists each replacement with all the others in its walletconflicts, so one
// read carries about N² bytes and Core builds it in memory (QA4W6-1, Core 31.1 regtest: 1100 bumps 82 MB in 2.1 s; 3000
// bumps 605 MB in 16 s, the node growing by about 1.5 GB per read). A read that takes escrowArbiterHeavy or longer, or
// times out, makes the next read wait, whatever it found: escrowArbiterWaitMin, doubled after each such read up to
// escrowArbiterWaitMax. A quicker read, pass or failure, costs the node little and paces nothing (QA4W7-2).
const (
	escrowArbiterHeavy   = 5 * time.Second
	escrowArbiterWaitMin = time.Minute
	escrowArbiterWaitMax = time.Hour
)

// arbiterPaceNow is the pacing's clock (when a read started and ended, when the next may start). A variable only so
// tests can move it.
var arbiterPaceNow = time.Now

// checkArbiterWallet requires the arbiter wallet (getwalletinfo) to be a descriptor wallet with private keys, not
// encrypted (the watcher co-signs without a passphrase) and used for nothing else: it has never spent. Escrow outputs
// never belong to it (the escrow descriptors are in the watch-only wallet) and the market never sends from it (it only
// signs PSBTs it built), so a spend (an entry of category "send") means the wallet is also used as an ordinary wallet.
// Coins paid to it prove nothing (A-471): anyone can pay the wpkh address of the market child key an order page's
// descriptor shows, which the wallet owns. With txcount 0 there is nothing to read.
//
// QA4W4A-1: reading every entry on every start let mass dust to that address keep escrow off (100,000 entries took
// listtransactions paging past startup's budget). So a passing read leaves a checkpoint (arbiterCheckpoint): the block
// listsinceblock reported, for the wallet it read (its name and getwalletinfo birthtime). A later check of the same
// wallet reads only what is newer than that block (and anything unconfirmed); with no checkpoint, one for another
// wallet (a different name or birthtime: another wallet, or one rebuilt with importdescriptors), a wallet that reports
// no birthtime, a block the node does not know (-5: another node or chain) or one that is not a block hash (QA4W5-8;
// Core refuses it, -8), it reads everything, in one call. From a block no longer in the active chain Core itself lists
// every entry from the fork point on. The read goes on in the background (arbiterScan): when the caller's context ends
// first (startup's budget) escrow stays off with escrowLogArbiterWait, and the next check of the same wallet from the
// same checkpoint (the escrow watcher's retry) takes that read's answer instead of starting again. It reports the state
// line and class startEscrow returns ("" when the wallet passes): escrowLogArbiterUse for a wallet that is not an
// arbiter's or has spent, escrowLogArbiterRead when its entries could not be read (a timeout, an unreachable node, an
// RPC error: never taken as a pass, and never reported as a wallet used for something else), or a database error.
// A-479: a read that takes escrowArbiterHeavy or longer, or times out, makes the next one wait (paceArbiter); a check
// while that wait runs starts no read and takes what that read found (arbiterHeld): a pass as a pass, so the other start
// checks go on (QA4W7-1). Startup's read is never held back: the wait is in memory, so a restart reads at once.
func (a *App) checkArbiterWallet(ctx context.Context, e *escrowBTC) (state, class string, err error) {
	var info struct {
		WalletName    string           `json:"walletname"`
		Descriptors   bool             `json:"descriptors"`
		PrivateKeys   bool             `json:"private_keys_enabled"`
		TxCount       *int64           `json:"txcount"`
		BirthTime     *int64           `json:"birthtime"`
		UnlockedUntil *json.RawMessage `json:"unlocked_until"`
	}
	if err := e.call(ctx, e.arbiterWallet, "getwalletinfo", []any{}, &info); err != nil {
		return a.arbiterUnread(errorClassOf(err))
	}
	switch {
	case !info.Descriptors || !info.PrivateKeys || info.TxCount == nil:
		return escrowLogArbiterUse, "getwalletinfo not-arbiter", nil
	case info.UnlockedUntil != nil:
		return escrowLogArbiterUse, "getwalletinfo encrypted", nil
	case *info.TxCount == 0:
		return "", "", nil
	}
	var mark arbiterCheckpoint
	if info.BirthTime != nil {
		mark = arbiterCheckpoint{Wallet: info.WalletName, BirthTime: *info.BirthTime}
	}
	from := ""
	var raw string
	switch err := a.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", escrowArbiterChecked).Scan(&raw); {
	case err == sql.ErrNoRows:
	case err != nil:
		return "", "", err
	default:
		var pinned arbiterCheckpoint
		// QA4W5-8: a block that is not a block hash (64 hex characters) is no checkpoint; Core would refuse it (-8) on
		// every retry.
		if info.BirthTime != nil && json.Unmarshal([]byte(raw), &pinned) == nil && pinned.Wallet == mark.Wallet && pinned.BirthTime == mark.BirthTime &&
			xmrHex64(pinned.Block) {
			from = pinned.Block
		}
	}
	scan := a.arbiterScan(e, fmt.Sprintf("%q %v %d %s", mark.Wallet, info.BirthTime != nil, mark.BirthTime, from), from)
	if scan == nil {
		return a.arbiterHeld()
	}
	select {
	case <-scan.done:
	case <-ctx.Done():
		wait := "waiting"
		a.escrowArbiterRead.Store(&wait)
		return escrowLogArbiterWait, "", nil
	}
	a.escrowScanMu.Lock()
	if a.escrowScan == scan {
		a.escrowScan = nil // taken: the next check reads again
	}
	a.paceArbiter(scan)
	a.escrowScanMu.Unlock()
	switch {
	case scan.err != nil:
		return a.arbiterUnread(errorClassOf(scan.err))
	case scan.sent:
		return escrowLogArbiterUse, "listsinceblock not-dedicated", nil
	case info.BirthTime == nil:
		return "", "", nil
	}
	mark.Block = scan.block
	b, err := json.Marshal(mark)
	if err != nil {
		return "", "", err
	}
	// The read may have finished just as the caller's budget ran out: the checkpoint is still written.
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err = a.db.ExecContext(write, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=excluded.value", escrowArbiterChecked, string(b))
	return "", "", err
}

// arbiterCheckpoint is the settings value (escrowArbiterChecked, JSON) a passing arbiter wallet check leaves: the
// wallet it read, by name and getwalletinfo birthtime (a wallet restored from its backup file keeps both; one rebuilt
// with importdescriptors gets another birthtime, probed on Core 31.1), and the block listsinceblock reported.
type arbiterCheckpoint struct {
	Wallet    string `json:"wallet"`
	BirthTime int64  `json:"birthtime"`
	Block     string `json:"block"`
}

// arbiterUnread records why the arbiter wallet's entries could not be read, for /admin, and returns its state line.
func (a *App) arbiterUnread(class string) (string, string, error) {
	a.escrowArbiterRead.Store(&class)
	return escrowLogArbiterRead, class, nil
}

// escrowArbiterScan is one read of the arbiter wallet's entries that outlives the check that started it (arbiterScan).
type escrowArbiterScan struct {
	key   string        // the wallet and the checkpoint block it reads from
	done  chan struct{} // closed once block, sent and err are set
	block string        // listsinceblock's lastblock
	sent  bool          // an entry of category "send" was listed
	err   error
	took  time.Duration // how long the read took, on arbiterPaceNow (A-479)
	end   time.Time     // when it ended, on arbiterPaceNow
}

// arbiterScan returns the read in progress (or finished and not yet taken) for key, or starts one: arbiterSends from
// from, and when the node does not know that block (-5) or refuses it as malformed (-8, QA4W5-8) from the start. It
// runs on its own context, bounded only by escrowArbiterReadTimeout per call, so a check whose context ends first
// leaves it running for the next check. A read in progress is always adopted; a new one does not start while a
// previous slow read's wait runs (A-479, paceArbiter): then it returns nil.
func (a *App) arbiterScan(e *escrowBTC, key, from string) *escrowArbiterScan {
	a.escrowScanMu.Lock()
	defer a.escrowScanMu.Unlock()
	if s := a.escrowScan; s != nil && s.key == key {
		return s
	}
	now := arbiterPaceNow
	if now().Before(a.escrowPace.next) {
		return nil
	}
	s := &escrowArbiterScan{key: key, done: make(chan struct{})}
	a.escrowScan = s
	go func() {
		defer close(s.done)
		start := now()
		s.block, s.sent, s.err = e.arbiterSends(context.Background(), from)
		var re *escrowRPCError
		if from != "" && errors.As(s.err, &re) && (re.Class == "rpc:-5" || re.Class == "rpc:-8") {
			s.block, s.sent, s.err = e.arbiterSends(context.Background(), "")
		}
		s.end = now()
		s.took = s.end.Sub(start)
	}()
	return s
}

// arbiterSends reads the arbiter wallet's entries with listsinceblock: with from "" every entry; otherwise those in
// blocks after from, and every unconfirmed or conflicted one. include_change is set, so a spend whose outputs all go to
// the wallet's own change addresses is listed as a "send" too (listtransactions lists nothing for it; probed on Core
// 31.1). It reports the lastblock Core answers (the tip as the wallet saw it: the next read's from) and whether any
// entry is a "send". The answer is read as it arrives (callStream), never held whole: 100,000 entries are 71 MB.
func (e *escrowBTC) arbiterSends(ctx context.Context, from string) (block string, sent bool, err error) {
	err = e.rpc.callStream(ctx, escrowArbiterReadTimeout, e.arbiterWallet, "listsinceblock", []any{from, 1, true, false, true}, func(dec *json.Decoder) error {
		if t, err := dec.Token(); err != nil || t == nil {
			return err // null: Core's answer to an error
		} else if t != json.Delim('{') {
			return errors.New("not an object")
		}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			switch key {
			case "lastblock":
				err = dec.Decode(&block)
			case "transactions":
				err = arbiterEntries(dec, &sent)
			default:
				err = skipValue(dec)
			}
			if err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	})
	if err == nil && block == "" {
		err = errors.New("bitcoin escrow RPC listsinceblock: unexpected result shape")
	}
	return block, sent, classifyEscrowRPC("listsinceblock", err)
}

// arbiterEntries reads listsinceblock's transactions list one entry at a time, and each entry one key at a time,
// setting *sent at an entry of category "send". Only category is decoded; every other value is skipped token by token
// (skipValue), so an entry's walletconflicts, which names every other replacement of its transaction (a payment its
// payer fee-bumped 1000 times lists about 67 KB of them, QA4W6-1), streams and rpcStreamMaxValue bounds each token, not
// the entry.
func arbiterEntries(dec *json.Decoder, sent *bool) error {
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return errors.New("not a list")
	}
	for dec.More() {
		if t, err := dec.Token(); err != nil || t != json.Delim('{') {
			return errors.New("not an object")
		}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			if key == "category" {
				var category string
				err = dec.Decode(&category)
				*sent = *sent || category == "send"
			} else {
				err = skipValue(dec)
			}
			if err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// escrowArbiterPace is when the next arbiter wallet read may start (A-479, paceArbiter).
type escrowArbiterPace struct {
	next time.Time          // no read starts before this; zero = none waits
	wait time.Duration      // the latest wait; 0 = none since the last quick answer
	last *escrowArbiterScan // the slow read that set it, whose answer a check takes while it waits
}

// paceArbiter sets when the next arbiter wallet read may start, from the read s a check has just taken (A-479). Only a
// slow read is paced (QA4W7-2): one that took escrowArbiterHeavy or longer, or timed out (escrowArbiterReadTimeout),
// whatever it found. It makes the next read wait from when s ended: escrowArbiterWaitMin, doubled after each slow read,
// up to escrowArbiterWaitMax; s is kept, so a check while the wait runs takes what it found (arbiterHeld). One log line
// names the wait. A quicker read, pass or failure, clears any wait: a failure that answers at once (a refused method, an
// RPC error) costs the node nothing and is read again every poll. The caller holds escrowScanMu.
func (a *App) paceArbiter(s *escrowArbiterScan) {
	p := &a.escrowPace
	var re *escrowRPCError
	timedOut := errors.As(s.err, &re) && re.Class == "timeout"
	if s.took < escrowArbiterHeavy && !timedOut {
		*p = escrowArbiterPace{}
		return
	}
	p.wait = min(max(2*p.wait, escrowArbiterWaitMin), escrowArbiterWaitMax)
	p.next, p.last = s.end.Add(p.wait), s
	what := "passed"
	switch {
	case s.err != nil:
		what = "failed (" + errorClassOf(s.err) + ")"
	case s.sent:
		what = "found a spend"
	}
	log.Printf("escrow: the arbiter wallet's read took %s and %s; the next read waits about %s", s.took.Round(time.Second), what, aboutMinutes(p.wait))
}

// arbiterHeld is checkArbiterWallet's answer while a slow read's wait runs (A-479): what that read found. A read that
// failed keeps the read-failure line and class; a spend keeps "used for the escrow key only"; a pass counts as a pass
// (QA4W7-1), so startEscrow goes on to its other checks. That is no weaker than escrow running: a running escrow never
// reads the arbiter wallet again, and the pass held is at most escrowArbiterWaitMax old.
func (a *App) arbiterHeld() (state, class string, err error) {
	a.escrowScanMu.Lock()
	s := a.escrowPace.last
	a.escrowScanMu.Unlock()
	switch {
	case s.err != nil:
		return a.arbiterUnread(errorClassOf(s.err))
	case s.sent:
		return escrowLogArbiterUse, "listsinceblock not-dedicated", nil
	}
	return "", "", nil
}

// arbiterNextCheck is when the next arbiter wallet read may start, for /admin: aboutMinutes of the wait left, or ""
// when none is left (the next poll reads).
func (a *App) arbiterNextCheck() string {
	a.escrowScanMu.Lock()
	next := a.escrowPace.next
	a.escrowScanMu.Unlock()
	if left := next.Sub(arbiterPaceNow()); left > 0 {
		return aboutMinutes(left)
	}
	return ""
}

// aboutMinutes is d in whole minutes, rounded up, at least one: "1 minute", "12 minutes".
func aboutMinutes(d time.Duration) string {
	n := max(int64((d+time.Minute-1)/time.Minute), 1)
	if n == 1 {
		return "1 minute"
	}
	return strconv.FormatInt(n, 10) + " minutes"
}
