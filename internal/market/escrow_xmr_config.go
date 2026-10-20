package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Monero escrow configuration and startup (WR-escrow Phase 3, P3-R2, P3-R4, P3-R13; A-449, P4-R15; A-460). ESCROW_XMR=on
// (off by default) runs against its own monero-wallet-rpc, with an independent escrow wallet-rpc, and a monerod on a
// local fakechain (monerod --regtest) or stagenet on a test-network market, or on mainnet only on a market configured
// with MONERO_NETWORK=mainnet (the operator's opt-in: real funds). The daemon's network is pinned in settings
// (escrow_xmr_network, pinXMRNetwork): a database whose Monero escrows are on one network refuses a daemon on another.
// With ESCROW_XMR off nothing here reads another setting, opens a file, writes a setting or contacts a node.

// xmrDefaultMaxFee caps a settlement's network fee (P3-R6) when ESCROW_XMR_MAX_FEE is blank: 0.01 XMR. A young
// fakechain charges about 0.0018744 XMR for a one-input, two-output, ring-16 transfer (chair probe on monerod
// 0.18.5.1, 2026-10-03), so this leaves headroom; xmrMaxMaxFee (0.1 XMR) bounds what an operator may set.
const (
	xmrDefaultMaxFee = 10_000_000_000  // piconero
	xmrMaxMaxFee     = 100_000_000_000 // piconero
)

// Startup refusals for escrow nodes that answer but fail the network check (P3-R2), as errEscrowChain does for
// Bitcoin escrow. startupRefusal appends the fixed reason (an xmrRefusal); nothing in them comes from a node.
var (
	errXMRStartDaemon = errors.New("refusing to start: the Monero escrow daemon at ESCROW_XMR_DAEMON_URL failed the network check. ESCROW_XMR=on runs on a local Monero fakechain (monerod --regtest) or on stagenet (monerod --stagenet), or on mainnet only when MONERO_NETWORK=mainnet. Set ESCROW_XMR=off, or point it at a daemon on the network this market is configured for")
	errXMRStartWallet = errors.New("refusing to start: the escrow wallet-rpc at ESCROW_XMR_WALLET_RPC_URL is not attached to the escrow daemon at ESCROW_XMR_DAEMON_URL. Set ESCROW_XMR=off, or start monero-wallet-rpc with --daemon-address pointing at that daemon")
	errXMRStartFee    = errors.New("refusing to start: ESCROW_XMR_FEE_ADDRESS is not an address of the escrow daemon's network. Set the fee wallet's address on that network")
	// errXMRRefusedUntilRestart answers recheckEscrowXMRNetwork once a re-check has refused the nodes.
	errXMRRefusedUntilRestart = errors.New("monero escrow: disabled until the app is restarted: the escrow nodes failed the network check")
)

// errXMRFeeAddressKeys (A-417): ESCROW_XMR_FEE_ADDRESS passes its checksum but its keys are not valid (xmrPointValid),
// so the wallet could build no settlement paying it.
var errXMRFeeAddressKeys = errors.New("ESCROW_XMR_FEE_ADDRESS is not a valid Monero address: its checksum matches but its keys are not valid, so no Monero escrow settlement could pay it. Copy the fee wallet's address again")

// errXMRMainnetOptIn refuses an inconsistent live App that lacks Monero's explicit mainnet opt-in.
var errXMRMainnetOptIn = errors.New("refusing to start: this market is configured for mainnet, and Monero escrow on mainnet holds real funds: it needs its own opt-in. Set MONERO_NETWORK=mainnet to run Monero escrow on mainnet, or set ESCROW_XMR=off")

// Fixed operator lines for Monero escrow's state at startup: nothing in them comes from a node. xmrLogOn is logged
// with the verified network type as its class: "escrow: Monero escrow on (stagenet)".
const (
	xmrLogOn        = "escrow: Monero escrow on"
	xmrLogNetwork   = "escrow: Monero escrow disabled: the escrow daemon is not on the network this database's Monero escrows are on (escrow_xmr_network); the admin page names both; see docs/operator-guide.md, \"Monero escrow\""
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
	torRelayOK           bool  // relay permission from the legacy node overlay or an explicit relay declaration
	mainnet              bool  // MONERO_NETWORK=mainnet: the operator's opt-in to Monero escrow on mainnet (A-460)
}

// xmrNodeTorOverlay is the Compose overlay that puts the local monerod behind its own Tor client (A-442), and
// xmrTorRelayAccepted is the direct-exposure choice: the operator accepts that Monero escrow settlements are
// relayed from this host's IP address in APP_MODE=tor (A-515).
const (
	xmrNodeTorOverlay   = "compose.nodes-tor.yaml"
	xmrTorRelayAccepted = "clearnet-accepted"
	// The app checks the production endpoint contract, not the remote proxy or firewall.
	xmrTorRelayRemote = "remote-tor"
)

// xmrLogTorHold is logged once at startup with xmrRelayHold's reason (A-515); nothing in it comes from a node.
const xmrLogTorHold = "escrow: APP_MODE=tor: Monero escrow relays held (%s): settlements are built, checked and stored but not relayed, because the escrow wallet-rpc relays through its daemon from this host's IP address. Add " +
	xmrNodeTorOverlay + " to COMPOSE_FILE (Monero over Tor is unverified), declare ESCROW_XMR_TOR_RELAY=" + xmrTorRelayRemote + " after configuring separate production payment-host Tor egress, or set ESCROW_XMR_TOR_RELAY=" + xmrTorRelayAccepted + ", then restart; the stored transactions are then relayed unchanged (docs/operator-guide.md, \"Monero escrow\")"

// escrowXMRConfigFromEnv reads ESCROW_XMR (on/off, blank = off), ESCROW_XMR_WALLET_RPC_URL and ESCROW_XMR_DAEMON_URL
// (both required), ESCROW_XMR_FEE_BPS (0 to 10000, default 0)
// and ESCROW_XMR_FEE_ADDRESS (needed above 0: a standard address or subaddress with valid keys, a mainnet one with
// MONERO_NETWORK=mainnet, else a fakechain or stagenet one; startEscrowXMR requires the daemon's network), ESCROW_XMR_SLOTS (5 to 200, default 20), ESCROW_XMR_MAX_FEE
// (piconero, 1 to xmrMaxMaxFee, default xmrDefaultMaxFee), MONERO_MAX_TIP_AGE (parseMoneroMaxTipAge),
// PAYMENT_CONFIRMATIONS_XMR (1 to 1000; a deposit counts from at least xmrMinConfirmations), MONERO_WALLET_SECRET
// (checkXMRWalletSecret), ESCROW_XMR_SEED_KEY (a path, read by initEscrowXMR) and ESCROW_XMR_TOR_RELAY (blank,
// xmrTorRelayAccepted or production-only xmrTorRelayRemote; blank, COMPOSE_FILE decides whether APP_MODE=tor relays). The market's
// class is read as New reads it (marketLiveFromEnv, A-460): MONERO_NETWORK=mainnet is Monero escrow's mainnet opt-in
// (mainnet). Retired Bitcoin configuration is refused before any node is contacted.
// With ESCROW_XMR off nothing else is read.
func escrowXMRConfigFromEnv(getenv func(string) string) (escrowXMRConfig, error) {
	var c escrowXMRConfig
	switch strings.TrimSpace(getenv("ESCROW_XMR")) {
	case "", "off":
		return c, nil
	case "on":
		c.on = true
	default:
		return c, errors.New(`ESCROW_XMR must be "on" or "off" (blank means off); Monero escrow runs on a local fakechain or on stagenet, or on mainnet only with MONERO_NETWORK=mainnet`)
	}
	live, err := marketLiveFromEnv(getenv)
	if err != nil {
		return c, err
	}
	if c.mainnet = strings.TrimSpace(getenv("MONERO_NETWORK")) == "mainnet"; live && !c.mainnet {
		return c, errXMRMainnetOptIn
	}
	if c.walletURL = strings.TrimSpace(getenv("ESCROW_XMR_WALLET_RPC_URL")); c.walletURL == "" {
		return c, errors.New("ESCROW_XMR=on needs ESCROW_XMR_WALLET_RPC_URL: the RPC URL of the monero-wallet-rpc that holds only the escrow wallets (docs/operator-guide.md, \"Monero escrow\")")
	}
	if c.daemonURL = strings.TrimSpace(getenv("ESCROW_XMR_DAEMON_URL")); c.daemonURL == "" {
		return c, errors.New("ESCROW_XMR=on needs ESCROW_XMR_DAEMON_URL: the RPC URL of the monerod (a local fakechain, monerod --regtest, or stagenet, monerod --stagenet; mainnet with MONERO_NETWORK=mainnet) the escrow wallet-rpc uses")
	}
	if v := strings.TrimSpace(getenv("ESCROW_XMR_FEE_BPS")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > 10000 {
			return c, errors.New("ESCROW_XMR_FEE_BPS must be a whole number of basis points from 0 to 10000 (250 = 2.5%)")
		}
		c.feeBps = n
	}
	if c.feeBps > 0 {
		// Before any node is contacted the fee address is checked by the market's class: with MONERO_NETWORK=mainnet a
		// mainnet address only; otherwise one of either test escrow network (a 4…/8… address is a fakechain's: mainnet's
		// format, never mainnet on such a market). startEscrowXMR refuses it unless it is of the network the daemon is
		// verified on (errXMRFeeNetwork).
		c.feeAddress = strings.TrimSpace(getenv("ESCROW_XMR_FEE_ADDRESS"))
		if c.mainnet {
			_, err = parseXMRAddress(c.feeAddress, "mainnet")
		} else if _, err = parseXMRAddress(c.feeAddress, "fakechain"); errors.Is(err, errXMRAddressNetwork) {
			_, err = parseXMRAddress(c.feeAddress, "stagenet")
		}
		switch {
		case errors.Is(err, errXMRAddressKeys):
			return c, errXMRFeeAddressKeys
		case err != nil && c.mainnet:
			return c, errors.New("ESCROW_XMR_FEE_BPS above 0 needs ESCROW_XMR_FEE_ADDRESS: the Monero mainnet standard address or subaddress (not integrated; starting with 4 or 8) the market fee is paid to, as MONERO_NETWORK=mainnet runs Monero escrow on mainnet")
		case err != nil:
			return c, errors.New("ESCROW_XMR_FEE_BPS above 0 needs ESCROW_XMR_FEE_ADDRESS: the Monero standard address or subaddress (not integrated) the market fee is paid to, on the escrow daemon's network (stagenet: starting with 5 or 7; a local fakechain: 4 or 8)")
		}
	}
	if _, err := parseMoneroMaxTipAge(getenv("MONERO_MAX_TIP_AGE")); err != nil {
		return c, err
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
	switch strings.TrimSpace(getenv("ESCROW_XMR_TOR_RELAY")) {
	case "":
		c.torRelayOK = composeFiles(getenv("COMPOSE_FILE"))[xmrNodeTorOverlay]
	case xmrTorRelayAccepted:
		c.torRelayOK = true
	case xmrTorRelayRemote:
		if err := CheckDeploymentProfile(getenv); err != nil {
			return c, err
		}
		c.torRelayOK = true
	default:
		return c, errors.New(`ESCROW_XMR_TOR_RELAY must be blank, "` + xmrTorRelayRemote + `" (production Tor with separately hosted HTTPS Monero services), or "` + xmrTorRelayAccepted + `" (accept direct relay exposure); see docs/operator-guide.md, "Monero escrow"`)
	}
	return c, nil
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
	torRelayOK    bool             // escrowXMRConfig.torRelayOK: in APP_MODE=tor settlements may be relayed (xmrRelayHold, A-515)

	state     atomic.Pointer[xmrState] // the latest startup check's state (initEscrowXMR, retryEscrowXMR, refuseEscrowXMR), for /admin (notReady)
	ready     atomic.Bool              // the secret verifier matched and the network check passed: escrow wallets may be used
	canCreate atomic.Bool              // ESCROW_XMR_SEED_KEY is the pinned seed key: new wallets may be created and sealed
	refused   atomic.Bool              // the nodes failed the network check after startup (P3-R2): disabled until a restart
	locked    sync.Map                 // wallet names another wallet program has open, logged once while it lasts (escrow_xmr_watch.go)
	belowFee  sync.Map                 // settlement ID -> the refusal xmrRefundBelowFee last found too few (escrow_xmr_settle.go, QA5W1-6)

	// reconciled is the id of the active wallet the watcher last reconciled in this process with nothing paused (0 =
	// none since startEscrowXMR): checkout claims only that wallet's slots, so no account the wallet gave out after a
	// restore point is offered before the reconcile has seen it (A-556, PAY66-9).
	reconciled atomic.Int64

	// The network pin and the sync guard (A-449), in memory only.
	pinned   atomic.Value                        // string: the network pin as initEscrowXMR read it, for display before the daemon is verified
	mismatch atomic.Pointer[EscrowChainMismatch] // set while startEscrowXMR refuses the daemon's network against the pin; nil = none
	sync     atomic.Value                        // string: the reason class the watcher last found the daemon not synced for ("" or unset = synced)
}

// formNet is the network type a refund or claim address typed into a form must be of: the verified one, else the
// network pin as initEscrowXMR read it (written only once a daemon was verified on that network, and fixed while the
// database holds Monero escrow state), so the vendor can mark an order shipped while the daemon is unavailable. ""
// (every address refused) before either exists: a database from before A-449 whose daemon was never reached since.
func (x *escrowXMR) formNet() string {
	if n := x.rpc.nettype(); n != "" {
		return n
	}
	p, _ := x.pinned.Load().(string)
	return p
}

// network names the network Monero escrow's notes, notices and pages speak of: formNet, else "mainnet" on a mainnet
// market (the only network its daemon may be on, A-460), else "fakechain", the only network Monero escrow ran on
// before the pin (A-449).
func (x *escrowXMR) network() string {
	if n := x.formNet(); n != "" {
		return n
	}
	if x.rpc.mainnet {
		return "mainnet"
	}
	return "fakechain"
}

// xmrEscrowNetwork is escrowXMR.network, or while ESCROW_XMR is off (an order page of an escrow made before it was
// switched off) "mainnet" on a mainnet market (checkNetworkClass: its escrows are mainnet ones) and "fakechain" on
// any other.
func (a *App) xmrEscrowNetwork() string {
	if a.escrowXMR != nil {
		return a.escrowXMR.network()
	}
	if a.live {
		return "mainnet"
	}
	return "fakechain"
}

// xmrEscrowNetKnown is the network Monero escrow was verified on or pinned to (escrowXMR.formNet), "" before that and
// while ESCROW_XMR is off: copy that cannot name the network of a particular order says nothing instead of guessing
// (A-472).
func (a *App) xmrEscrowNetKnown() string {
	if a.escrowXMR == nil {
		return ""
	}
	return a.escrowXMR.formNet()
}

// xmrEscrowNetShort is the short name of the network in "(…)" in Go-built copy, the rule of the templates
// payment-status:xmr-net and /admin: "network not verified yet", "Monero mainnet", "Monero stagenet" or "local
// fakechain" (A-472, A-460).
func (a *App) xmrEscrowNetShort() string {
	switch a.xmrEscrowNetKnown() {
	case "":
		return "network not verified yet"
	case "mainnet":
		return "Monero mainnet"
	case "stagenet":
		return "Monero stagenet"
	}
	return "local fakechain"
}

// initEscrowXMR initializes the market's sole payment adapter in New. ESCROW_XMR=off does nothing. On, a configuration error, an
// unusable seed key file, a mainnet market without MONERO_NETWORK=mainnet (errXMRMainnetOptIn), or an escrow node that answers but fails the network check
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
	if a.live && !cfg.mainnet {
		return errXMRMainnetOptIn
	}
	key, err := loadXMRSeedKey(cfg.seedKeyPath)
	if err != nil {
		return err
	}
	rpc, err := newXMREscrowRPC(cfg.walletURL, cfg.daemonURL)
	if err != nil {
		return err
	}
	rpc.mainnet = a.live // the market's class decides which daemon checkDaemon accepts (A-460)
	a.escrowXMR = &escrowXMR{rpc: rpc, feeBps: cfg.feeBps, feeAddress: cfg.feeAddress, slots: cfg.slots, maxFee: cfg.maxFee,
		confirmations: cfg.confirmations, secret: cfg.secret, seedKey: key, torRelayOK: cfg.torRelayOK}
	var pinned string
	if err = a.db.QueryRowContext(ctx, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'')", xmrNetworkPin).Scan(&pinned); err != nil {
		return err
	}
	a.escrowXMR.pinned.Store(pinned)
	state, class, err := a.startEscrowXMR(ctx)
	var refusal *xmrRefusal
	if errors.As(err, &refusal) {
		return startupRefusal(refusal)
	}
	if err != nil {
		return err
	}
	// The fee passed its startup checks: a host change is recorded and announced to administrators (A-550). Its own
	// deadline: a node that timed out above has spent ctx, and the market still starts then.
	feeCtx, cancelFee := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	err = a.recordXMRFee(feeCtx)
	cancelFee()
	if err != nil {
		return err
	}
	logEscrowState(state, class)
	if why := a.xmrRelayHold(); why != "" {
		log.Printf(xmrLogTorHold, why) // once per process: relay configuration is read at startup only
	}
	if state != xmrLogSecret && !a.escrowXMR.canCreate.Load() {
		log.Print(xmrLogSeedKey)
	}
	a.escrowXMR.startState = state
	a.escrowXMR.state.Store(&xmrState{state, class})
	return nil
}

// seedKeyMismatch reports that ESCROW_XMR_SEED_KEY was checked against the seed key pinned in this database and is
// not it: no new escrow wallet is created (xmrLogSeedKey). False before that check ran: with a changed secret
// (xmrLogSecret) or a database error first (xmrLogUnrecorded) canCreate was never set.
func (x *escrowXMR) seedKeyMismatch() bool {
	if x.canCreate.Load() {
		return false
	}
	s := x.state.Load()
	return s != nil && s.line != xmrLogSecret && s.line != xmrLogUnrecorded
}

// xmrState is one of the fixed state lines of Monero escrow's startup checks with its error class ("" for none; for
// xmrLogRefused, the refusal's fixed reason). Nothing in it comes from a node.
type xmrState struct{ line, class string }

// notReady says, while Monero escrow is configured but not ready, which fixed state its latest startup check left it in,
// in plain words with the error class, and what the operator does about it ("" while ready). /admin shows both under
// Payment providers (xmrEscrowNodeLoader) and the state in the escrow-only readiness panel (QA4W4B-1).
func (x *escrowXMR) notReady() (state, remedy string) {
	if x.ready.Load() {
		return "", ""
	}
	s := x.state.Load()
	if s == nil {
		s = &xmrState{}
	}
	const retried = " The check is retried every poll; Monero escrow starts once it passes, without a restart."
	withClass := func(words string) string {
		if s.class != "" {
			return words + " (" + s.class + ")"
		}
		return words
	}
	switch s.line {
	case xmrLogUnchecked:
		return withClass("the escrow daemon or wallet-rpc could not be checked at startup"), "Check ESCROW_XMR_DAEMON_URL and ESCROW_XMR_WALLET_RPC_URL, then restart or wait for the next poll." + retried
	case xmrLogNetwork:
		return "the escrow daemon is not on the network this database's Monero escrows are on",
			"Point ESCROW_XMR_DAEMON_URL and the escrow wallet-rpc back at a daemon on that network; a new network needs a new database." + retried
	case xmrLogSecret:
		return "MONERO_WALLET_SECRET is not the secret this database was first started with",
			"No escrow wallet can be opened or created with it. Restore the original secret and restart the app."
	case xmrLogUnrecorded:
		return withClass("its secret verifier or seed key could not be checked in the database"), "Check the database and its connection." + retried
	case xmrLogRefused:
		return "refused until the app is restarted", "Reason: " + s.class + ". Correct what it describes, then restart the app; nothing is retried until then."
	}
	return "its startup checks have not passed yet", "The server log names the state." + retried
}

// startEscrowXMR runs Monero escrow's checks once: the secret verifier (a mismatch stops here, before any node is
// contacted), the seed key pin (a mismatch clears canCreate: wallet creation only), the daemon (checkDaemon), the
// network pin (pinXMRNetwork: a mismatch leaves Monero escrow disabled with xmrLogNetwork, writes nothing and makes no
// wallet call; retried like an unreachable node), that ESCROW_XMR_FEE_ADDRESS is of the verified network, then the open
// wallet (checkOpenWallet) before the network type is
// adapter parses addresses for set, and escrow ready. It
// reports the state as one of the fixed lines (xmrLogOn, with the network type as its class, when ready) with the
// error class that caused it, or an error: an *xmrRefusal when a node answered but failed the network check
// (initEscrowXMR refuses to start; a watcher retry passes it to refuseEscrowXMR), or a database error. It logs
// nothing, so the watcher can retry it and log a state only when it changes (retryEscrow's pattern). Once refused it
// answers errXMRRefusedUntilRestart and checks nothing.
func (a *App) startEscrowXMR(ctx context.Context) (state, class string, err error) {
	x := a.escrowXMR
	if x.refused.Load() {
		return "", "", errXMRRefusedUntilRestart
	}
	x.mismatch.Store(nil) // shown on /admin only while the latest attempt found the daemon on another network
	x.reconciled.Store(0)
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
	// The daemon first, then the pin, and only then the wallet: a wallet of one network is never refreshed against a
	// daemon of the other.
	info, err := x.rpc.checkDaemon(ctx)
	var refusal *xmrRefusal
	switch {
	case errors.As(err, &refusal):
		return "", "", refusal
	case err != nil:
		return xmrLogUnchecked, xmrErrorClass(err), nil
	}
	pinned, ok, err := pinXMRNetwork(ctx, a.db, info.NetType)
	if err != nil {
		return "", "", err
	}
	if !ok {
		x.mismatch.Store(&EscrowChainMismatch{Pinned: pinned, Reported: info.NetType})
		return xmrLogNetwork, "", nil
	}
	if x.feeAddress != "" {
		if _, err := parseXMRAddress(x.feeAddress, info.NetType); err != nil {
			return "", "", errXMRFeeNetwork
		}
	}
	_, err = x.rpc.checkOpenWallet(ctx, info.Height)
	switch {
	case errors.As(err, &refusal):
		return "", "", refusal
	case err != nil:
		return xmrLogUnchecked, xmrErrorClass(err), nil
	}
	x.rpc.net.Store(info.NetType)
	x.ready.Store(true)
	return xmrLogOn, info.NetType, nil
}

// xmrNetworkPin is the settings key of the network type a database's Monero escrows are on (A-449, P4-R15).
const (
	xmrNetworkPin     = "escrow_xmr_network"
	xmrNetworkPinLock = 782498 // the transaction advisory lock pinXMRNetwork takes (pinEscrowChain's is 782497)
)

// pinXMRNetwork checks nettype (the escrow daemon's verified network type) against the network pinned in settings
// (xmrNetworkPin) and reports the pinned value and whether Monero escrow may use the daemon. While the database holds
// no Monero escrow state (no Monero escrow, no escrow wallet and no vendor bond wallet, A-547: a wallet's accounts are
// addresses of one network) the pin
// is written, or rewritten, to nettype: a database may change network until it holds such state, never after (a new
// network needs a new database). A database that holds state and no pin (one from before A-449) is pinned when the
// daemon reports fakechain, the only network Monero escrow ran on before; on any other it is refused. A refusal writes
// nothing. One transaction under xmrNetworkPinLock, so concurrent startups agree (pinEscrowChain's pattern).
func pinXMRNetwork(ctx context.Context, db *sql.DB, nettype string) (pinned string, ok bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", xmrNetworkPinLock); err != nil {
		return "", false, err
	}
	var held bool
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key=$1),''),
		EXISTS (SELECT 1 FROM escrows WHERE coin='XMR') OR EXISTS (SELECT 1 FROM xmr_escrow_wallets) OR EXISTS (SELECT 1 FROM xmr_bond_wallets)`, xmrNetworkPin).Scan(&pinned, &held); err != nil {
		return "", false, err
	}
	switch {
	case pinned == nettype:
		return pinned, true, nil
	case held && (pinned != "" || nettype != "fakechain"):
		return pinned, false, nil
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=excluded.value", xmrNetworkPin, nettype); err != nil {
		return "", false, err
	}
	return nettype, true, tx.Commit()
}

// startupRefusal is the error New returns for an escrow node that answered but failed the network check: the fixed
// line for the daemon or the wallet-rpc, or for ESCROW_XMR_FEE_ADDRESS, with the refusal's fixed reason.
func startupRefusal(r *xmrRefusal) error {
	if r == errXMRFeeNetwork {
		return fmt.Errorf("%w (%s)", errXMRStartFee, r.msg)
	}
	if r.wallet {
		return fmt.Errorf("%w (%s)", errXMRStartWallet, r.msg)
	}
	return fmt.Errorf("%w (%s)", errXMRStartDaemon, r.msg)
}

// recheckEscrowXMRNetwork is the Monero escrow watcher's network check after each open_wallet (X-c): the daemon
// (checkDaemon, and still on the network Monero escrow started on: errXMRNetworkMoved), then the open wallet after a
// refresh (checkWallet). A node that answers but fails it disables Monero
// escrow until the app is restarted (refuseEscrowXMR: sticky, one fixed line) and the refusal is returned; from then
// on it answers errXMRRefusedUntilRestart without contacting either node. Any other error (unreachable, timeout, no
// wallet open) is returned unchanged and changes nothing, so the next pass retries. The caller uses the open wallet
// only when it returns nil.
func (a *App) recheckEscrowXMRNetwork(ctx context.Context) error {
	x := a.escrowXMR
	if x == nil || x.refused.Load() {
		return errXMRRefusedUntilRestart
	}
	info, err := x.rpc.checkDaemon(ctx)
	switch {
	case err == nil && info.NetType != x.rpc.nettype():
		err = errXMRNetworkMoved
	case err == nil:
		err = x.rpc.checkWallet(ctx, info.Height)
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
	x.state.Store(&xmrState{xmrLogRefused, r.msg})
	x.ready.Store(false)
	if !x.refused.Swap(true) {
		log.Print(xmrLogRefused + r.msg)
	}
}
