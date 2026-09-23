package market

import (
	"context"
	"fmt"
	"strings"
)

// Incoming is one received output (Bitcoin vout / Monero subaddress transfer).
type Incoming struct {
	Address, TxID string
	Index         int64 // vout or subaddress minor index
	Amount        int64 // atomic units
	Confirmations int64 // <0 = conflicted/reorged
	Locked        bool  // Monero unlock_time != 0: recorded and reported, never credited
}

// PaymentProvider is a test-network wallet adapter. Implementations never accept mainnet.
type PaymentProvider interface {
	Currency() string // "BTC" or "XMR"
	Network() string  // never "main"/"mainnet"
	Confirmations() int
	NewAddress(ctx context.Context, orderID string) (string, error)
	Incoming(ctx context.Context, addresses []string) ([]Incoming, error)
	Send(ctx context.Context, to string, amount int64) (txid string, err error)
	ValidAddress(addr string) bool // format for THIS network; mainnet formats false
}

// providerFactory returns (nil, nil) when its currency is not configured; an error aborts startup.
type providerFactory func(ctx context.Context) (PaymentProvider, error)

var providerFactories []providerFactory

func registerProvider(f providerFactory) { providerFactories = append(providerFactories, f) }

// initPayments builds a.payments from registered factories during New() (not in preview).
func (a *App) initPayments(ctx context.Context) error {
	for _, f := range providerFactories {
		p, err := f(ctx)
		if err != nil {
			return err
		}
		if p == nil {
			continue
		}
		if n := strings.ToLower(p.Network()); n == "main" || n == "mainnet" || n == "" {
			return fmt.Errorf("refusing to start: %s provider network %q is not a test network", p.Currency(), p.Network())
		}
		if a.payments[p.Currency()] != nil {
			return fmt.Errorf("two payment providers configured for %s", p.Currency())
		}
		a.payments[p.Currency()] = p
	}
	return nil
}

var backgroundTasks []func(ctx context.Context, a *App)

// registerBackground adds a goroutine started by Start and stopped (context cancelled, then awaited) by Close.
func registerBackground(f func(ctx context.Context, a *App)) {
	backgroundTasks = append(backgroundTasks, f)
}

// Start launches registered background work; it does nothing in preview mode.
func (a *App) Start(ctx context.Context) {
	if a.preview || a.db == nil || a.stop != nil {
		return
	}
	ctx, a.stop = context.WithCancel(ctx)
	for _, f := range backgroundTasks {
		a.background.Add(1)
		go func() {
			defer a.background.Done()
			f(ctx, a)
		}()
	}
}

func (a *App) Close() {
	if a.stop != nil {
		a.stop()
		a.background.Wait()
	}
	if a.db != nil {
		a.db.Close()
	}
}
