package market

import (
	"context"
	"database/sql"
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

// PaymentProvider is a wallet adapter for one currency on one network. A test network is the default; the live
// network (mainnet) is used only when the operator names it (BITCOIN_CHAIN=main, MONERO_NETWORK=mainnet).
type PaymentProvider interface {
	Currency() string // "BTC" or "XMR"
	// Network is the configured network until the first successful check ("" when detected from the node) and
	// the node's network after it. It is a live network ("main"/"mainnet") only when the operator configured one.
	Network() string
	Confirmations() int
	NewAddress(ctx context.Context, orderID string) (string, error)
	Incoming(ctx context.Context, addresses []string) ([]Incoming, error)
	Send(ctx context.Context, to string, amount int64) (txid string, err error)
	ValidAddress(addr string) bool // format for THIS network; other networks' formats false
}

// mainnetOptIn is implemented by providers the operator can configure for mainnet. A provider without it, or
// one whose MainnetConfigured is false, is held to test networks by checkProvider whatever it reports.
type mainnetOptIn interface {
	MainnetConfigured() bool // BITCOIN_CHAIN=main, MONERO_NETWORK=mainnet
}

func mainnetConfigured(p PaymentProvider) bool {
	m, ok := p.(mainnetOptIn)
	return ok && m.MainnetConfigured()
}

// isLiveNetwork reports whether network is a live (mainnet) network rather than a test network.
func isLiveNetwork(network string) bool {
	n := strings.ToLower(network)
	return n == "main" || n == "mainnet"
}

// netTag, netName and netAdj label amounts, notes and copy with the network class of this market: every
// provider is on a test network, or every provider is on mainnet (initPayments refuses a mix).
func (a *App) netTag() string {
	if a.live {
		return "MAINNET"
	}
	return "TESTNET"
}

// netName is the network class as a noun: "(test network)", "(mainnet)".
func (a *App) netName() string {
	if a.live {
		return "mainnet"
	}
	return "test network"
}

// netAdj is the network class as an adjective: "the BTC test-network wallet", "the BTC mainnet wallet".
func (a *App) netAdj() string {
	if a.live {
		return "mainnet"
	}
	return "test-network"
}

// providerChecker is implemented by node-backed providers. Check (re)connects, runs the network guard,
// loads or opens the wallet when needed and reports whether the node is still syncing. It runs at startup
// and at the start of every watcher pass, so a restarted node or wallet service is picked up again.
// An error for which isRefusal is true means the node is not (or no longer) on the configured network.
type providerChecker interface {
	Check(ctx context.Context) (nodeStatus, error)
}

// nodeStatus is what a provider's Check reports about its node and wallet.
type nodeStatus struct {
	Syncing bool
	Tip     int64
}

// refusalError marks a node or wallet that is not on the configured network. At startup it is fatal; once
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
	disabled bool   // refused (not on the configured network): never retried
}

// checkProvider runs the provider's own Check (when it has one) and then the generic network guard: the
// provider must report a live network exactly when live (the operator configured mainnet), and some network.
func checkProvider(ctx context.Context, p PaymentProvider, live bool) (nodeStatus, error) {
	var st nodeStatus
	if c, ok := p.(providerChecker); ok {
		var err error
		if st, err = c.Check(ctx); err != nil {
			return nodeStatus{}, err
		}
	}
	switch n := p.Network(); {
	case live && !isLiveNetwork(n):
		return nodeStatus{}, refusef("%s provider network %q is not mainnet", p.Currency(), n)
	case !live && (isLiveNetwork(n) || n == ""):
		return nodeStatus{}, refusef("%s provider network %q is not a test network", p.Currency(), n)
	}
	return st, nil
}

// initPayments builds the providers from registered factories during New() (not in preview). A node that
// refuses the network guard stops startup; an unreachable node or a wallet that is not loaded leaves that
// currency unavailable (not fatal) until a watcher pass finds it working. A provider configured for mainnet
// makes this a live market (a.live): every other provider must be configured for mainnet too. New() then runs
// checkNetworkHistory.
func (a *App) initPayments(ctx context.Context) error {
	var built []PaymentProvider
	for _, f := range providerFactories {
		p, err := f(ctx)
		if err != nil {
			return err
		}
		if p != nil {
			built = append(built, p)
		}
	}
	for i, p := range built {
		live := mainnetConfigured(p)
		if i == 0 {
			a.live = live
		} else if live != a.live {
			return errors.New("refusing to start: one payment currency is configured for mainnet and another for a test network; configure both for mainnet (BITCOIN_CHAIN=main, MONERO_NETWORK=mainnet) or both for test networks")
		}
	}
	for _, p := range built {
		cur := p.Currency()
		if a.payments[cur] != nil || a.unavailable[cur] != nil {
			return fmt.Errorf("two payment providers configured for %s", cur)
		}
		if _, err := checkProvider(ctx, p, a.live); err != nil {
			if isRefusal(err) {
				return fmt.Errorf("refusing to start: %w", err)
			}
			log.Printf("payments: %s unavailable at startup; payment requests are refused and the check is retried every watcher pass: %v", cur, err)
			a.unavailable[cur] = &unavailableProvider{p: p, err: truncate(err.Error(), 300)}
			continue
		}
		a.payments[cur] = p
	}
	if a.live {
		log.Printf("payments: MAINNET — deposits and payouts move real funds")
	}
	return nil
}

// checkNetworkHistory runs in New() after initPayments. It refuses to start when the database holds deposit
// addresses for a configured currency issued on the other class of network: a mainnet wallet must never pay
// out for test-network deposits, and test coins must never settle mainnet orders. Addresses record their
// network in payment_addresses.provider ("btc-testnet3", "xmr-mainnet"; see orders_actions.go). With no such
// address, a watcher status row from the other class is deleted: its highest tip (a testnet3 tip is far above
// mainnet's) would otherwise hold every pass as "node is behind".
func (a *App) checkNetworkHistory(ctx context.Context) error {
	a.payMu.RLock()
	var currencies []string
	for cur := range a.payments {
		currencies = append(currencies, cur)
	}
	for cur := range a.unavailable {
		currencies = append(currencies, cur)
	}
	a.payMu.RUnlock()
	for _, cur := range currencies {
		var provider string
		err := a.db.QueryRowContext(ctx, "SELECT provider FROM payment_addresses WHERE currency=$1 AND (split_part(provider,'-',2) IN ('main','mainnet'))<>$2 LIMIT 1", cur, a.live).Scan(&provider)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return fmt.Errorf("payments: reading %s payment history: %w", cur, err)
		}
		_, network, _ := strings.Cut(provider, "-")
		return fmt.Errorf("refusing to start: this database holds %s payments made on %s, but the %s wallet is configured for %s; use a fresh database", cur, historyClass(network), cur, a.netName())
	}
	for _, cur := range currencies {
		res, err := a.db.ExecContext(ctx, "DELETE FROM payment_status WHERE currency=$1 AND network<>'' AND (network IN ('main','mainnet'))<>$2", cur, a.live)
		if err != nil {
			return fmt.Errorf("payments: resetting %s watcher status: %w", cur, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("payments: %s watcher status from another network class reset for %s", cur, a.netName())
		}
	}
	return nil
}

func historyClass(network string) string {
	if isLiveNetwork(network) {
		return "mainnet"
	}
	return "a test network (" + network + ")"
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

// confirmationThreshold returns the confirmation threshold of currency's provider, working or configured but
// unavailable in this process (not disabled); 0 when there is none, so only conflicted deposits are below it.
func (a *App) confirmationThreshold(currency string) int64 {
	a.payMu.RLock()
	defer a.payMu.RUnlock()
	if p := a.payments[currency]; p != nil {
		return int64(p.Confirmations())
	}
	if u := a.unavailable[currency]; u != nil && !u.disabled {
		return int64(u.p.Confirmations())
	}
	return 0
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
// checked again and promoted once they pass the network guard; working providers are checked so a
// restarted wallet is loaded or opened again. The result maps each currency that must not be polled this
// pass to the reason (check failed, node syncing, provider disabled), and why maps it to a class safe to log
// (no wallet text, A-170); tips maps each other checked currency whose node reported a height to that tip
// (pollOnce compares it with the highest tip recorded).
func (a *App) refreshProviders(ctx context.Context) (skip map[string]string, tips map[string]int64, why map[string]string) {
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
	skip, tips, why = map[string]string{}, map[string]int64{}, map[string]string{}
	for cur, u := range waiting {
		a.payMu.RLock()
		disabled, last := u.disabled, u.err
		a.payMu.RUnlock()
		if disabled {
			skip[cur], why[cur] = last, refusedClass
			continue
		}
		st, err := checkProvider(ctx, u.p, a.live)
		if ctx.Err() != nil {
			return skip, tips, why
		}
		a.payMu.Lock()
		switch {
		case isRefusal(err):
			u.disabled, u.err = true, "Disabled until the node is fixed and the app restarted: "+truncate(err.Error(), 300)
			why[cur] = refusedClass
			log.Printf("payments: %s provider disabled: %s", cur, refusedClass)
		case err != nil:
			u.err = truncate(err.Error(), 300)
			why[cur] = "wallet check failed: " + watcherCause(err)
		default:
			delete(a.unavailable, cur)
			a.payments[cur] = u.p
			log.Printf("payments: %s provider available (%s %s)", cur, a.netTag(), u.p.Network())
		}
		if err != nil {
			skip[cur] = u.err
		} else if st.Syncing {
			skip[cur] = syncingReason
		} else if st.Tip > 0 {
			tips[cur] = st.Tip
		}
		a.payMu.Unlock()
	}
	for cur, p := range ready {
		if _, ok := p.(providerChecker); !ok {
			continue
		}
		st, err := checkProvider(ctx, p, a.live)
		if ctx.Err() != nil {
			return skip, tips, why
		}
		switch {
		case isRefusal(err):
			msg := "Disabled until the node is fixed and the app restarted: " + truncate(err.Error(), 300)
			a.payMu.Lock()
			delete(a.payments, cur)
			a.unavailable[cur] = &unavailableProvider{p: p, err: msg, disabled: true}
			a.payMu.Unlock()
			log.Printf("payments: %s provider disabled: %s", cur, refusedClass)
			skip[cur], why[cur] = msg, refusedClass
		case err != nil:
			skip[cur] = "Wallet check failed; watcher pass skipped (no deposits read, no expiry, no payouts): " + truncate(err.Error(), 300)
			why[cur] = "wallet check failed: " + watcherCause(err)
		case st.Syncing:
			skip[cur] = syncingReason
		case st.Tip > 0:
			tips[cur] = st.Tip
		}
	}
	return skip, tips, why
}

// refusedClass is the log class of a provider the network guard refused; the node's own answer is only in
// the reason on the admin page (Refused — not a test network, or not mainnet).
const refusedClass = "refused by the network guard (reason on the admin page)"

const syncingReason = "Node is still syncing; watcher pass skipped (no deposits read, no expiry, no payouts) until it catches up."

// tipBehindPrefix starts the reason a pass is skipped because the node's tip is below the highest tip recorded
// for the currency (payment_status.tip_height); like syncing, it is not reported as an error.
const tipBehindPrefix = "Node is behind the highest tip recorded"

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
	a.closeDB()
}
