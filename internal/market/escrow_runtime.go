package market

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func (a *App) netTag() string {
	if a.live {
		return "MAINNET"
	}
	return "TESTNET"
}
func (a *App) netName() string {
	if a.live {
		return "mainnet"
	}
	return "test network"
}
func (a *App) bitcoinPriceNetwork() string {
	if !a.escrowConfigured {
		return "not configured"
	}
	if !a.escrowNetKnown() {
		return "not yet known"
	}
	if a.escrowNetwork() == "main" {
		return "main network"
	}
	return "test network"
}
func (a *App) bitcoinPriceLabel() string { return "Price (BTC, " + a.bitcoinPriceNetwork() + ")" }
func isLiveNetwork(network string) bool {
	n := strings.ToLower(network)
	return n == "main" || n == "mainnet"
}

// errMixedNetworks refuses a market configured for mainnet in one currency and a test network in the other.
var errMixedNetworks = errors.New("refusing to start: a market runs on mainnet or on test networks, never both. BITCOIN_CHAIN=main needs MONERO_NETWORK=mainnet or blank, and MONERO_NETWORK=mainnet needs BITCOIN_CHAIN=main or blank")

// marketLiveFromEnv reads the market's network class (A-460). BITCOIN_CHAIN=main or MONERO_NETWORK=mainnet is the
// operator's mainnet opt-in: escrow then holds real funds, and every page says MAINNET. Anything else is a
// test-network market. A market is all mainnet or all test networks (errMixedNetworks).
func marketLiveFromEnv(getenv func(string) string) (bool, error) {
	chain, xmr := strings.TrimSpace(getenv("BITCOIN_CHAIN")), strings.TrimSpace(getenv("MONERO_NETWORK"))
	btcLive, xmrLive := chain == "main", xmr == "mainnet"
	if btcLive && xmr != "" && !xmrLive || xmrLive && chain != "" && !btcLive {
		return false, errMixedNetworks
	}
	return btcLive || xmrLive, nil
}

// checkNetworkClass refuses a database whose escrows are on the other class of network than the market (A-460): a
// mainnet market never shows test-network escrows as real funds, and a test-network market never handles mainnet
// ones. It reads the escrow pins (escrowChainPin, xmrNetworkPin) of a database that holds escrows; escrows from before a
// pin ran on regtest or a local fakechain only.
func (a *App) checkNetworkClass(ctx context.Context) error {
	var btcPin, xmrPin string
	var btcHeld, xmrHeld bool
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key=$1),''),
		COALESCE((SELECT value FROM settings WHERE key=$2),''),
		EXISTS (SELECT 1 FROM escrows WHERE coin='BTC'),
		EXISTS (SELECT 1 FROM escrows WHERE coin='XMR') OR EXISTS (SELECT 1 FROM xmr_escrow_wallets)`,
		escrowChainPin, xmrNetworkPin).Scan(&btcPin, &xmrPin, &btcHeld, &xmrHeld); err != nil {
		return err
	}
	// A pin without escrows is rewritten by the next escrow startup (pinEscrowChain, pinXMRNetwork): only held escrows
	// count.
	switch {
	case !btcHeld:
		btcPin = ""
	case btcPin == "":
		btcPin = "regtest"
	}
	switch {
	case !xmrHeld:
		xmrPin = ""
	case xmrPin == "":
		xmrPin = "fakechain"
	}
	chain, _, _ := strings.Cut(btcPin, ":")
	for _, pin := range []string{chain, xmrPin} {
		if pin != "" && isLiveNetwork(pin) != a.live {
			if a.live {
				return errors.New("refusing to start: this market is configured for mainnet (BITCOIN_CHAIN=main or MONERO_NETWORK=mainnet), and this database holds escrows on a test network. Use a new database for a mainnet market")
			}
			return errors.New("refusing to start: this database holds mainnet escrows, and this market is not configured for mainnet. Set BITCOIN_CHAIN=main or MONERO_NETWORK=mainnet as before")
		}
	}
	return nil
}

type nodeStatus struct {
	Syncing     bool
	Why, Remedy string
	Tip         int64
}
type refusalError struct{ msg string }

func (e *refusalError) Error() string          { return e.msg }
func refusef(format string, args ...any) error { return &refusalError{fmt.Sprintf(format, args...)} }
func isRefusal(err error) bool                 { var r *refusalError; return errors.As(err, &r) }
func envConfirmations(key string, def int) (int, error) {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 1000 {
		return 0, fmt.Errorf("%s must be a whole number from 1 to 1000", key)
	}
	return n, nil
}
func paymentExpiry() (time.Duration, error) {
	s := strings.TrimSpace(os.Getenv("PAYMENT_EXPIRY"))
	if s == "" {
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 10*time.Minute || d > 720*time.Hour {
		return 0, errors.New("PAYMENT_EXPIRY must be a duration from 10m to 720h, e.g. 24h")
	}
	return d, nil
}
func expiryWindow() time.Duration {
	d, err := paymentExpiry()
	if err != nil {
		return 24 * time.Hour
	}
	return d
}
func pollInterval() (time.Duration, error) {
	s := strings.TrimSpace(os.Getenv("PAYMENT_POLL_INTERVAL"))
	if s == "" {
		return 30 * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Second || d > time.Hour {
		return 0, errors.New("PAYMENT_POLL_INTERVAL must be a duration from 1s to 1h, e.g. 30s")
	}
	return d, nil
}

const payoutSendTimeout = 30 * time.Second

var backgroundTasks []func(context.Context, *App)

func registerBackground(f func(context.Context, *App)) { backgroundTasks = append(backgroundTasks, f) }
func (a *App) Start(ctx context.Context) {
	if a.preview || a.db == nil || a.stop != nil {
		return
	}
	ctx, a.stop = context.WithCancel(ctx)
	a.startRetention(ctx)
	for _, f := range backgroundTasks {
		a.background.Add(1)
		go func() { defer a.background.Done(); f(ctx, a) }()
	}
}
func (a *App) Close() {
	if a.stop != nil {
		a.stop()
		a.background.Wait()
	}
	a.closeDB()
}
