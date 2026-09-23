package market

import (
	"context"
	"errors"
	"fmt"
	"log"
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

// providerChecker is implemented by node-backed providers. Check (re)connects, runs the test-network guard,
// loads or opens the wallet when needed and reports whether the node is still syncing. It runs at startup
// and at the start of every watcher pass, so a restarted node or wallet service is picked up again.
// An error for which isRefusal is true means the node is not (or no longer) on an allowed test network.
type providerChecker interface {
	Check(ctx context.Context) (syncing bool, err error)
}

// refusalError marks a node or wallet that is not on an allowed test network. At startup it is fatal; once
// the site is running it permanently disables that provider (never retried until a restart).
type refusalError struct{ msg string }

func (e *refusalError) Error() string { return e.msg }

func refusef(format string, args ...any) error { return &refusalError{fmt.Sprintf(format, args...)} }

func isRefusal(err error) bool {
	var r *refusalError
	return errors.As(err, &r)
}

// providerFactory returns (nil, nil) when its currency is not configured and an error for invalid
// configuration (which aborts startup). It does not contact the node: initPayments runs Check.
type providerFactory func(ctx context.Context) (PaymentProvider, error)

var providerFactories []providerFactory

func registerProvider(f providerFactory) { providerFactories = append(providerFactories, f) }

// unavailableProvider is a configured provider that has not passed its checks: payment requests for its
// currency are refused as "payment unavailable" and the watcher retries it at the start of every pass.
type unavailableProvider struct {
	p        PaymentProvider
	err      string // last check error, shown on the admin page
	disabled bool   // refused (not a test network): never retried
}

// checkProvider runs the provider's own Check (when it has one) and then the generic network guard.
func checkProvider(ctx context.Context, p PaymentProvider) (bool, error) {
	var syncing bool
	if c, ok := p.(providerChecker); ok {
		var err error
		if syncing, err = c.Check(ctx); err != nil {
			return false, err
		}
	}
	if n := strings.ToLower(p.Network()); n == "main" || n == "mainnet" || n == "" {
		return false, refusef("%s provider network %q is not a test network", p.Currency(), p.Network())
	}
	return syncing, nil
}

// initPayments builds the providers from registered factories during New() (not in preview). A node that
// refuses the test-network guard stops startup; an unreachable node or a wallet that is not loaded leaves
// that currency unavailable (not fatal) until a watcher pass finds it working.
func (a *App) initPayments(ctx context.Context) error {
	for _, f := range providerFactories {
		p, err := f(ctx)
		if err != nil {
			return err
		}
		if p == nil {
			continue
		}
		cur := p.Currency()
		if a.payments[cur] != nil || a.unavailable[cur] != nil {
			return fmt.Errorf("two payment providers configured for %s", cur)
		}
		if _, err = checkProvider(ctx, p); err != nil {
			if isRefusal(err) {
				return fmt.Errorf("refusing to start: %w", err)
			}
			log.Printf("payments: %s unavailable at startup; payment requests are refused and the check is retried every watcher pass: %v", cur, err)
			a.unavailable[cur] = &unavailableProvider{p: p, err: truncate(err.Error(), 300)}
			continue
		}
		a.payments[cur] = p
	}
	return nil
}

// provider returns the working provider for currency, or nil when it is not configured or unavailable.
func (a *App) provider(currency string) PaymentProvider {
	a.payMu.RLock()
	defer a.payMu.RUnlock()
	return a.payments[currency]
}

// providers returns a snapshot of the working providers.
func (a *App) providers() map[string]PaymentProvider {
	a.payMu.RLock()
	defer a.payMu.RUnlock()
	out := make(map[string]PaymentProvider, len(a.payments))
	for c, p := range a.payments {
		out[c] = p
	}
	return out
}

// unavailableError returns the last check error for a configured currency that is not working ("" otherwise).
func (a *App) unavailableError(currency string) (msg string, disabled bool) {
	a.payMu.RLock()
	defer a.payMu.RUnlock()
	if u := a.unavailable[currency]; u != nil {
		return u.err, u.disabled
	}
	return "", false
}

func (a *App) paymentsConfigured() bool {
	a.payMu.RLock()
	defer a.payMu.RUnlock()
	return len(a.payments) > 0 || len(a.unavailable) > 0
}

// refreshProviders runs at the start of every watcher pass, in every process. Unavailable providers are
// checked again and promoted once they pass the test-network guard; working providers are checked so a
// restarted wallet is loaded or opened again. The result maps each currency that must not be polled this
// pass to the reason (check failed, node syncing, provider disabled).
func (a *App) refreshProviders(ctx context.Context) map[string]string {
	a.checkMu.Lock() // a provider's first successful Check records its network; never run two at once
	defer a.checkMu.Unlock()
	a.payMu.RLock()
	ready := make(map[string]PaymentProvider, len(a.payments))
	for c, p := range a.payments {
		ready[c] = p
	}
	waiting := make(map[string]*unavailableProvider, len(a.unavailable))
	for c, u := range a.unavailable {
		waiting[c] = u
	}
	a.payMu.RUnlock()
	skip := map[string]string{}
	for cur, u := range waiting {
		a.payMu.RLock()
		disabled, last := u.disabled, u.err
		a.payMu.RUnlock()
		if disabled {
			skip[cur] = last
			continue
		}
		syncing, err := checkProvider(ctx, u.p)
		if ctx.Err() != nil {
			return skip
		}
		a.payMu.Lock()
		switch {
		case isRefusal(err):
			u.disabled, u.err = true, "Disabled until the node is fixed and the app restarted: "+truncate(err.Error(), 300)
			log.Printf("payments: %s provider disabled: %v", cur, err)
		case err != nil:
			u.err = truncate(err.Error(), 300)
		default:
			delete(a.unavailable, cur)
			a.payments[cur] = u.p
			log.Printf("payments: %s provider available (TESTNET %s)", cur, u.p.Network())
		}
		if err != nil {
			skip[cur] = u.err
		} else if syncing {
			skip[cur] = syncingReason
		}
		a.payMu.Unlock()
	}
	for cur, p := range ready {
		if _, ok := p.(providerChecker); !ok {
			continue
		}
		syncing, err := checkProvider(ctx, p)
		if ctx.Err() != nil {
			return skip
		}
		switch {
		case isRefusal(err):
			msg := "Disabled until the node is fixed and the app restarted: " + truncate(err.Error(), 300)
			a.payMu.Lock()
			delete(a.payments, cur)
			a.unavailable[cur] = &unavailableProvider{p: p, err: msg, disabled: true}
			a.payMu.Unlock()
			log.Printf("payments: %s provider disabled: %v", cur, err)
			skip[cur] = msg
		case err != nil:
			skip[cur] = "Wallet check failed; watcher pass skipped (no deposits read, no expiry, no payouts): " + truncate(err.Error(), 300)
		case syncing:
			skip[cur] = syncingReason
		}
	}
	return skip
}

const syncingReason = "Node is still syncing; watcher pass skipped (no deposits read, no expiry, no payouts) until it catches up."

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

// Close stops background work, waits for it to finish (an in-flight payout completes on its own bounded
// context) and then closes the database.
func (a *App) Close() {
	if a.stop != nil {
		a.stop()
		a.background.Wait()
	}
	if a.db != nil {
		a.db.Close()
	}
}
