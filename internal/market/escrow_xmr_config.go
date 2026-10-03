package market

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Monero escrow configuration and startup (WR-escrow Phase 3, P3-R2, P3-R4, P3-R13). ESCROW_XMR=on (off by
// default) runs only against a local fakechain (monerod --regtest) and its own monero-wallet-rpc, never the pooled
// one. With ESCROW_XMR off nothing here reads another setting, opens a file, writes a setting or contacts a node.

// xmrDefaultMaxFee caps a settlement's network fee (P3-R6) when ESCROW_XMR_MAX_FEE is blank: 0.01 XMR. A young
// fakechain charges about 0.0018744 XMR for a one-input, two-output, ring-16 transfer (chair probe on monerod
// 0.18.5.1, 2026-10-03), so this leaves headroom; xmrMaxMaxFee (0.1 XMR) bounds what an operator may set.
const (
	xmrDefaultMaxFee = 10_000_000_000  // piconero
	xmrMaxMaxFee     = 100_000_000_000 // piconero
)

// Startup refusals for escrow nodes that answer but fail the network check (P3-R2), as errEscrowNotRegtest does for
// Bitcoin escrow. startupRefusal appends the fixed reason (an xmrRefusal); nothing in them comes from a node.
var (
	errXMRStartDaemon = errors.New("refusing to start: ESCROW_XMR=on runs only on a local Monero fakechain in this release, and the daemon at ESCROW_XMR_DAEMON_URL is not one. Set ESCROW_XMR=off, or point it at a fakechain daemon (monerod --regtest)")
	errXMRStartWallet = errors.New("refusing to start: the escrow wallet-rpc at ESCROW_XMR_WALLET_RPC_URL is not attached to the fakechain daemon at ESCROW_XMR_DAEMON_URL. Set ESCROW_XMR=off, or start monero-wallet-rpc with --daemon-address pointing at that daemon")
	// errXMRRefusedUntilRestart answers recheckEscrowXMRNetwork once a re-check has refused the nodes.
	errXMRRefusedUntilRestart = errors.New("monero escrow: disabled until the app is restarted: the escrow nodes failed the network check")
)

var errXMRMainnetMarket = errors.New("refusing to start: ESCROW_XMR=on runs only on a local fakechain in this release, and this market is configured for mainnet (MONERO_NETWORK=mainnet or BITCOIN_CHAIN=main). Set ESCROW_XMR=off")

// Fixed operator lines for Monero escrow's state at startup: nothing in them comes from a node.
const (
	xmrLogOn        = "escrow: Monero escrow on (local fakechain only)"
	xmrLogUnchecked = "escrow: Monero escrow disabled: the escrow daemon or wallet-rpc could not be checked at startup"
	xmrLogRefused   = "escrow: Monero escrow disabled until the app is restarted: " // + an xmrRefusal
	xmrLogSecret    = "escrow: Monero escrow disabled: MONERO_WALLET_SECRET differs from the secret this database was first started with, so no escrow wallet can be opened or created with it; restore the original secret (docs/operator-guide.md, \"Monero escrow\")"
	xmrLogSeedKey   = "escrow: Monero escrow wallet creation stopped: ESCROW_XMR_SEED_KEY is not the seed key pinned in this database; seeds are never re-sealed automatically. Restore the pinned key, or re-pin the new one as docs/operator-guide.md, \"Monero escrow\" describes"
)

type escrowXMRConfig struct {
	on                   bool
	walletURL, daemonURL string
	feeBps               int64  // the market fee in basis points (ESCROW_BTC_FEE_BPS's rule)
	feeAddress           string // where the market fee goes ("" when feeBps is 0)
	slots                int    // free slots kept on the active wallet (P3-R3)
	maxFee               int64  // the highest network fee a settlement may pay, in piconero (P3-R6)
	secret               string // MONERO_WALLET_SECRET; never logged
	seedKeyPath          string
	confirmations        int64 // a deposit counts from max(PAYMENT_CONFIRMATIONS_XMR, xmrMinConfirmations) (P3-R5)
}

// escrowXMRConfigFromEnv reads ESCROW_XMR (on/off, blank = off), ESCROW_XMR_WALLET_RPC_URL and ESCROW_XMR_DAEMON_URL
// (both required; the wallet-rpc must not be MONERO_WALLET_RPC_URL's), ESCROW_XMR_FEE_BPS (0 to 10000, default 0)
// and ESCROW_XMR_FEE_ADDRESS (needed above 0: a fakechain standard address or subaddress), ESCROW_XMR_SLOTS (5 to
// 200, default 20), ESCROW_XMR_MAX_FEE (piconero, 1 to xmrMaxMaxFee, default xmrDefaultMaxFee),
// PAYMENT_CONFIRMATIONS_XMR (1 to 1000; a deposit counts from at least xmrMinConfirmations), MONERO_WALLET_SECRET
// (checkXMRWalletSecret) and ESCROW_XMR_SEED_KEY (a path, read by initEscrowXMR). A market
// configured for mainnet (MONERO_NETWORK=mainnet, BITCOIN_CHAIN=main) is refused. Nothing is opened or contacted,
// and with ESCROW_XMR off nothing else is read.
func escrowXMRConfigFromEnv(getenv func(string) string) (escrowXMRConfig, error) {
	var c escrowXMRConfig
	switch strings.TrimSpace(getenv("ESCROW_XMR")) {
	case "", "off":
		return c, nil
	case "on":
		c.on = true
	default:
		return c, errors.New(`ESCROW_XMR must be "on" or "off" (blank means off); Monero escrow runs on a local fakechain only in this release`)
	}
	if strings.TrimSpace(getenv("MONERO_NETWORK")) == "mainnet" || strings.TrimSpace(getenv("BITCOIN_CHAIN")) == "main" {
		return c, errXMRMainnetMarket
	}
	if c.walletURL = strings.TrimSpace(getenv("ESCROW_XMR_WALLET_RPC_URL")); c.walletURL == "" {
		return c, errors.New("ESCROW_XMR=on needs ESCROW_XMR_WALLET_RPC_URL: the RPC URL of the monero-wallet-rpc that holds only the escrow wallets (docs/operator-guide.md, \"Monero escrow\")")
	}
	if c.daemonURL = strings.TrimSpace(getenv("ESCROW_XMR_DAEMON_URL")); c.daemonURL == "" {
		return c, errors.New("ESCROW_XMR=on needs ESCROW_XMR_DAEMON_URL: the RPC URL of the fakechain monerod (monerod --regtest) the escrow wallet-rpc uses")
	}
	if sameRPCEndpoint(c.walletURL, getenv("MONERO_WALLET_RPC_URL")) {
		return c, errors.New("ESCROW_XMR_WALLET_RPC_URL must be a monero-wallet-rpc of its own, not the pooled wallet's MONERO_WALLET_RPC_URL: monero-wallet-rpc holds one wallet open at a time")
	}
	if v := strings.TrimSpace(getenv("ESCROW_XMR_FEE_BPS")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > 10000 {
			return c, errors.New("ESCROW_XMR_FEE_BPS must be a whole number of basis points from 0 to 10000 (250 = 2.5%)")
		}
		c.feeBps = n
	}
	if c.feeBps > 0 {
		c.feeAddress = strings.TrimSpace(getenv("ESCROW_XMR_FEE_ADDRESS"))
		if _, err := parseXMRAddress(c.feeAddress); err != nil {
			return c, errors.New("ESCROW_XMR_FEE_BPS above 0 needs ESCROW_XMR_FEE_ADDRESS: the fakechain Monero standard address or subaddress (starting with 4 or 8; not integrated) the market fee is paid to")
		}
	}
	c.slots = 20
	if v := strings.TrimSpace(getenv("ESCROW_XMR_SLOTS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 5 || n > 200 {
			return c, errors.New("ESCROW_XMR_SLOTS must be a whole number from 5 to 200: the free escrow accounts kept ready for checkouts")
		}
		c.slots = n
	}
	c.maxFee = xmrDefaultMaxFee
	if v := strings.TrimSpace(getenv("ESCROW_XMR_MAX_FEE")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > xmrMaxMaxFee {
			return c, errors.New("ESCROW_XMR_MAX_FEE must be a whole number of piconero from 1 to 100000000000 (0.1 XMR): the highest network fee a Monero escrow settlement may pay")
		}
		c.maxFee = n
	}
	c.confirmations = xmrMinConfirmations
	if v := strings.TrimSpace(getenv("PAYMENT_CONFIRMATIONS_XMR")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 1000 {
			return c, errors.New("PAYMENT_CONFIRMATIONS_XMR must be a whole number from 1 to 1000")
		}
		c.confirmations = max(n, xmrMinConfirmations)
	}
	c.secret = getenv("MONERO_WALLET_SECRET")
	if err := checkXMRWalletSecret(c.secret, getenv("SETUP_TOKEN"), getenv("APP_SECRET_KEY")); err != nil {
		return c, err
	}
	if c.seedKeyPath = strings.TrimSpace(getenv("ESCROW_XMR_SEED_KEY")); c.seedKeyPath == "" {
		return c, errors.New("ESCROW_XMR=on needs ESCROW_XMR_SEED_KEY: the path of the armored OpenPGP public key every escrow wallet's seed is sealed to (keep its private key offline)")
	}
	return c, nil
}

// sameRPCEndpoint reports whether two RPC URLs name the same endpoint, ignoring credentials, case in the scheme and
// host, and a trailing slash. It compares spellings (localhost and 127.0.0.1 differ); the wallet height check backs it
// up, since the pooled wallet-rpc follows a stagenet or testnet daemon, never the escrow's fakechain.
func sameRPCEndpoint(a, b string) bool {
	norm := func(raw string) string {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			return ""
		}
		return strings.ToLower(u.Scheme+"://"+u.Host) + strings.TrimRight(u.Path, "/")
	}
	n := norm(a)
	return n != "" && n == norm(b)
}

// escrowXMR is Monero escrow as configured at startup: App.escrowXMR, nil while ESCROW_XMR is off. The Monero escrow
// watcher (X-c) uses it: it re-runs startEscrowXMR while escrow is not ready (a node unreachable at startup) and
// calls recheckEscrowXMRNetwork after each open_wallet.
type escrowXMR struct {
	rpc           *xmrEscrowRPC
	feeBps        int64
	feeAddress    string
	slots         int
	maxFee        int64
	confirmations int64  // a deposit counts from this many confirmations (P3-R5)
	secret        string // MONERO_WALLET_SECRET: wallet passwords derive from it (xmrWalletPassword); never logged
	seedKey       *xmrSeedKey
	startState    string           // the state line logged at startup
	now           func() time.Time // the watcher's clock (escrow_xmr_watch.go); nil is time.Now. Tests move it

	ready     atomic.Bool // the secret verifier matched and the network check passed: escrow wallets may be used
	canCreate atomic.Bool // ESCROW_XMR_SEED_KEY is the pinned seed key: new wallets may be created and sealed
	refused   atomic.Bool // the nodes failed the network check after startup (P3-R2): disabled until a restart
}

// initEscrowXMR runs in New after Bitcoin escrow. ESCROW_XMR=off does nothing. On, a configuration error, an
// unusable seed key file, a mainnet market, or an escrow node that answers but fails the network check
// (errXMRStartDaemon, errXMRStartWallet) stops startup. A changed secret, a changed seed key (wallet creation only),
// an unreachable node or a timeout leaves Monero escrow configured but unavailable with one fixed line, and the market
// runs; the watcher retries. A wallet-rpc with no wallet open (-13) is not a failure: the daemon is checked and escrow
// is ready; each wallet is checked by recheckEscrowXMRNetwork right after open_wallet, and no wallet call is made until
// it returns nil.
func (a *App) initEscrowXMR(ctx context.Context) error {
	cfg, err := escrowXMRConfigFromEnv(os.Getenv)
	if err != nil || !cfg.on {
		return err
	}
	if a.live {
		return errXMRMainnetMarket
	}
	key, err := loadXMRSeedKey(cfg.seedKeyPath)
	if err != nil {
		return err
	}
	rpc, err := newXMREscrowRPC(cfg.walletURL, cfg.daemonURL)
	if err != nil {
		return err
	}
	a.escrowXMR = &escrowXMR{rpc: rpc, feeBps: cfg.feeBps, feeAddress: cfg.feeAddress, slots: cfg.slots, maxFee: cfg.maxFee,
		confirmations: cfg.confirmations, secret: cfg.secret, seedKey: key}
	state, class, err := a.startEscrowXMR(ctx)
	var refusal *xmrRefusal
	if errors.As(err, &refusal) {
		return startupRefusal(refusal)
	}
	if err != nil {
		return err
	}
	logEscrowState(state, class)
	if state != xmrLogSecret && !a.escrowXMR.canCreate.Load() {
		log.Print(xmrLogSeedKey)
	}
	a.escrowXMR.startState = state
	return nil
}

// startEscrowXMR runs Monero escrow's checks once: the secret verifier (a mismatch stops here, before any node is
// contacted), the seed key pin (a mismatch clears canCreate: wallet creation only) and the network check. It reports
// the state as one of the fixed lines (xmrLogOn when ready) with the error class that caused it, or an error: an
// *xmrRefusal when a node answered but failed the network check (initEscrowXMR refuses to start; a watcher retry
// passes it to refuseEscrowXMR), or a database error. It logs nothing, so the watcher can retry it and log a state
// only when it changes (retryEscrow's pattern). Once refused it answers errXMRRefusedUntilRestart and checks nothing.
func (a *App) startEscrowXMR(ctx context.Context) (state, class string, err error) {
	x := a.escrowXMR
	if x.refused.Load() {
		return "", "", errXMRRefusedUntilRestart
	}
	ok, err := pinXMRSetting(ctx, a.db, xmrVerifierSetting, xmrSecretVerifier(x.secret))
	if err != nil {
		return "", "", err
	}
	if !ok {
		x.ready.Store(false)
		return xmrLogSecret, "", nil
	}
	ok, err = pinXMRSetting(ctx, a.db, xmrSeedKeySetting, x.seedKey.fpr)
	if err != nil {
		return "", "", err
	}
	x.canCreate.Store(ok)
	_, err = x.rpc.checkNetwork(ctx)
	var refusal *xmrRefusal
	switch {
	case errors.As(err, &refusal):
		return "", "", refusal
	case err != nil:
		return xmrLogUnchecked, xmrErrorClass(err), nil
	}
	x.ready.Store(true)
	return xmrLogOn, "", nil
}

// startupRefusal is the error New returns for an escrow node that answered but failed the network check: the fixed
// line for the daemon or the wallet-rpc, with the refusal's fixed reason.
func startupRefusal(r *xmrRefusal) error {
	if r.wallet {
		return fmt.Errorf("%w (%s)", errXMRStartWallet, r.msg)
	}
	return fmt.Errorf("%w (%s)", errXMRStartDaemon, r.msg)
}

// recheckEscrowXMRNetwork is the Monero escrow watcher's network check after each open_wallet (X-c): the daemon
// (checkDaemon), then the open wallet after a refresh (checkWallet). A node that answers but fails it disables Monero
// escrow until the app is restarted (refuseEscrowXMR: sticky, one fixed line) and the refusal is returned; from then
// on it answers errXMRRefusedUntilRestart without contacting either node. Any other error (unreachable, timeout, no
// wallet open) is returned unchanged and changes nothing, so the next pass retries. The caller uses the open wallet
// only when it returns nil.
func (a *App) recheckEscrowXMRNetwork(ctx context.Context) error {
	x := a.escrowXMR
	if x == nil || x.refused.Load() {
		return errXMRRefusedUntilRestart
	}
	h, err := x.rpc.checkDaemon(ctx)
	if err == nil {
		err = x.rpc.checkWallet(ctx, h)
	}
	var refusal *xmrRefusal
	if errors.As(err, &refusal) {
		x.refuseEscrowXMR(refusal)
	}
	return err
}

// refuseEscrowXMR disables Monero escrow until a restart after a node failed the network check, logging xmrLogRefused
// with the fixed reason once.
func (x *escrowXMR) refuseEscrowXMR(r *xmrRefusal) {
	x.ready.Store(false)
	if !x.refused.Swap(true) {
		log.Print(xmrLogRefused + r.msg)
	}
}
