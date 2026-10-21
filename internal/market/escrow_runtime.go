package market

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

func (a *App) netTag() string {
	if a.live {
		return "MAINNET"
	}
	return "TESTNET"
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

// marketLiveFromEnv reads only Monero's explicit mainnet opt-in. Retired Bitcoin
// configuration is refused, so it cannot change the meaning of Monero funds.

func marketLiveFromEnv(getenv func(string) string) (bool, error) {
	if err := refuseBitcoinEnv(getenv); err != nil {
		return false, err
	}
	return strings.TrimSpace(getenv("MONERO_NETWORK")) == "mainnet", nil
}

// checkNetworkClass prevents relabeling an existing Monero database between
// mainnet and test networks. An old unpinned escrow used the local fakechain.
func (a *App) checkNetworkClass(ctx context.Context) error {
	var pin string
	var held bool
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key=$1),''),
		EXISTS(SELECT 1 FROM escrows WHERE coin='XMR') OR EXISTS(SELECT 1 FROM xmr_escrow_wallets)`,
		xmrNetworkPin).Scan(&pin, &held); err != nil {
		return err
	}
	if !held {
		return nil
	}
	if pin == "" {
		pin = "fakechain"
	}
	if isLiveNetwork(pin) != a.live {
		if a.live {
			return errors.New("refusing to start: this market is configured for mainnet (MONERO_NETWORK=mainnet), and this database holds escrows on a test network. Use a new database for a mainnet market")
		}
		return errors.New("refusing to start: this database holds mainnet escrows, and this market is not configured for mainnet. Set MONERO_NETWORK=mainnet as before")
	}
	return nil
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
	if a.servingDB != nil {
		a.background.Add(1)
		go func() { defer a.background.Done(); a.keepServingLock(ctx) }()
	}
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
