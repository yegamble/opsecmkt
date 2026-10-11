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
