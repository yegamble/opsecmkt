package market

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const vendorBondSetting = "vendor_bond"
const sellerBondRequired = "The seller's required vendor bond is not currently confirmed. New orders are paused; existing orders continue."
const bondBalanceMaxAge = 2 * time.Minute
const bondPollInterval = 30 * time.Second

type bondProvider interface {
	Currency() string
	Allocate(context.Context, string) (address, reference string, err error)
	Observe(context.Context, string, string) (confirmed, pending int64, err error)
}

type bondPolicy struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type BondRateView struct {
	Fiat, Coin, Rate, Sources, AsOf, Checked string
	Available                                bool
}
type BondQuoteView struct{ ID, Handle, Currency, Face, Coin, Target, Address, Confirmed, Pending, Expires, Checked, Status, Rate, RateAsOf string }
type VendorBondView struct {
	Amount, Currency           string
	Currencies                 []string
	Required, Satisfied, Admin bool
	Coins                      []string
	Quotes                     []BondQuoteView
	Rates                      []BondRateView
}

func init() {
	registerAction("/admin/vendor-bond", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: vendorBondPolicyAction})
	registerAction("/vendor/bond", actionSpec{Roles: []string{"vendor", "admin"}, FormNonce: true, OneAtATime: true, Run: vendorBondQuoteAction})
	registerLoader("admin", loadVendorBond)
	registerLoader("vendor-dashboard", loadVendorBond)
	registerPreview("admin", previewVendorBond)
	registerPreview("vendor-dashboard", previewVendorBond)
	registerBackground(func(ctx context.Context, a *App) {
		if len(a.bondProviders) == 0 {
			return
		}
		ticker := time.NewTicker(bondPollInterval)
		defer ticker.Stop()
		for {
			pass, cancel := context.WithTimeout(ctx, 25*time.Second)
			_ = a.pollVendorBonds(pass) // fixed, bounded status errors; never print wallet data
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func bondDecimals(cur string) int {
	switch cur {
	case "USD", "GBP", "EUR":
		return 2
	case "BTC":
		return 8
	case "XMR":
		return 12
	}
	return -1
}

func vendorBondPolicy(ctx context.Context, q rowQuerier, lock bool) (bondPolicy, error) {
	suffix := ""
	if lock {
		suffix = " FOR SHARE NOWAIT"
	}
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1"+suffix, vendorBondSetting).Scan(&raw); err != nil {
		return bondPolicy{}, err
	}
	var p bondPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Amount < 0 || bondDecimals(p.Currency) < 0 {
		return p, fail(503, "The vendor bond setting is unreadable. Contact the administrator.")
	}
	return p, nil
}

// The caller holds the vendor's account FOR SHARE before this query in any write path.
// The watcher takes FOR NO KEY UPDATE before changing an observation, so a checkout
// cannot race a withdrawal/reorg observation. Policy updates conflict with the share lock.
func bondCredit(ctx context.Context, q rowQuerier, id string, p bondPolicy) (int64, error) {
	var credit int64
	err := q.QueryRowContext(ctx, `SELECT LEAST(COALESCE(sum(amount),0),$3::numeric)::bigint FROM vendor_bond_quotes
		WHERE vendor_id=$1 AND reference_currency=$2 AND committed_at IS NOT NULL AND observation_ok
		AND confirmed>=crypto_amount AND checked_at>=now()-interval '2 minutes' AND checked_at<=now()
		AND NOT (`+restoreGateSQL+`)`, id, p.Currency, p.Amount).Scan(&credit)
	return credit, err
}

func vendorBondSatisfied(ctx context.Context, q rowQuerier, id string, lock bool) (bool, error) {
	p, err := vendorBondPolicy(ctx, q, lock)
	if err != nil {
		return false, err
	}
	if p.Amount == 0 {
		return true, nil
	}
	credit, err := bondCredit(ctx, q, id, p)
	return credit >= p.Amount, err
}

func lockVendorBond(c *actionCtx, id string) error {
	ok, err := vendorBondSatisfied(c.Ctx(), c.Tx, id, true)
	if err != nil {
		return err
	}
	if !ok {
		return fail(409, sellerBondRequired+" Fund the bond from the vendor desk.")
	}
	return nil
}

func vendorBondOrderGate(ctx context.Context, a *App, d *PageData) error {
	if d.Order == nil || d.Order.State != stateDraft || d.OrderViewer != roleBuyer {
		return nil
	}
	ok, err := vendorBondSatisfied(ctx, a.db, d.Order.VendorID, false)
	if err != nil || ok {
		return err
	}
	d.VendorBondReason = sellerBondRequired
	if d.EscrowCheckout != nil {
		d.EscrowCheckout.Reason = sellerBondRequired
	}
	if d.XMREscrowCheckout != nil {
		d.XMREscrowCheckout.Reason = sellerBondRequired
	}
	for i := range d.Transitions {
		if d.Transitions[i].To == stateAwaitingPayment {
			d.Transitions[i].Action = ""
			d.Transitions[i].Label = sellerBondRequired
		}
	}
	return nil
}

func vendorBondPolicyAction(c *actionCtx) (actionResult, error) {
	cur, raw := c.Form.Get("currency"), strings.TrimSpace(c.Form.Get("amount"))
	dec := bondDecimals(cur)
	if dec < 0 {
		return actionResult{}, fail(400, "Choose USD, GBP, EUR, BTC or XMR for the vendor bond.")
	}
	n := int64(0)
	var err error
	n, err = parseAmount(raw, dec)
	if err != nil {
		parts := strings.Split(raw, ".")
		if len(parts) <= 2 && parts[0] != "" && strings.Trim(strings.Join(parts, ""), "0") == "" && (len(parts) == 1 || len(parts[1]) <= dec) {
			n, err = 0, nil
		}
	}
	if raw == "" || err != nil || n < 0 {
		return actionResult{}, fail(400, "Enter a non-negative vendor bond amount in the selected currency; use 0 to disable the requirement.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		if err := lockSetting(c.Ctx(), c.Tx, vendorBondSetting); err != nil {
			return actionResult{}, err
		}
		old, err := vendorBondPolicy(c.Ctx(), c.Tx, false)
		if err != nil {
			return actionResult{}, err
		}
		if old.Amount == n && old.Currency == cur {
			return actionResult{}, fail(409, "The vendor bond requirement is already set to that amount and currency.")
		}
		encoded, _ := json.Marshal(bondPolicy{Amount: n, Currency: cur})
		if err := setSetting(c, vendorBondSetting, string(encoded)); err != nil {
			return actionResult{}, err
		}
		audit := fmt.Sprintf("Changed vendor bond requirement: %s %s → %s %s", amount(old.Amount, bondDecimals(old.Currency)), old.Currency, amount(n, dec), cur)
		return actionResult{Redirect: "/admin", Notice: "vendor-bond-policy", Audit: audit}, nil
	})
}

func vendorBondQuoteAction(c *actionCtx) (actionResult, error) {
	coin := c.Form.Get("currency")
	if coin != "BTC" && coin != "XMR" {
		return actionResult{}, fail(400, "Choose BTC or XMR to fund the vendor bond.")
	}
	provider := c.A.bondProviders[coin]
	if provider == nil {
		return actionResult{}, fail(409, "Automatic "+coin+" bond deposits are not configured. Contact the administrator.")
	}
	var role string
	if err := c.Tx.QueryRowContext(c.Ctx(), "SELECT role FROM users WHERE id=$1 FOR NO KEY UPDATE NOWAIT", c.User.ID).Scan(&role); err != nil {
		return actionResult{}, err
	}
	if role != "vendor" && role != "admin" {
		return actionResult{}, fail(403, "Vendor access required")
	}
	p, err := vendorBondPolicy(c.Ctx(), c.Tx, true)
	if err != nil {
		return actionResult{}, err
	}
	if p.Amount == 0 {
		return actionResult{}, fail(409, "No vendor bond is required.")
	}
	var restoring bool
	if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT "+restoreGateSQL).Scan(&restoring); err != nil {
		return actionResult{}, err
	}
	if restoring {
		return actionResult{}, fail(409, "Bond quotes are paused during restore reconciliation. Contact the administrator.")
	}
	credit, err := bondCredit(c.Ctx(), c.Tx, c.User.ID, p)
	if err != nil {
		return actionResult{}, err
	}
	if credit >= p.Amount {
		return actionResult{}, fail(409, "Your required vendor bond is already confirmed.")
	}
	face := p.Amount - credit
	var target int64
	var rate string
	var asOf time.Time
	if p.Currency == "BTC" || p.Currency == "XMR" {
		if p.Currency != coin {
			return actionResult{}, fail(400, "This bond is denominated in "+p.Currency+"; fund it in that coin.")
		}
		target = face
	} else {
		if c.A.fiatRates == nil {
			return actionResult{}, errFiatRatesUnavailable
		}
		target, rate, asOf, err = fiatAtomicQuote(c.Ctx(), c.Tx, face, p.Currency, coin)
		if err != nil {
			return actionResult{}, err
		}
	}
	// Bond deposits do not settle an order payout. Keep small top-ups payable.
	// A receiving minimum keeps Bitcoin quotes above ordinary wallet dust limits.
	if coin == "BTC" && target < 1000 {
		target = 1000
	}
	var id string
	var expires time.Time
	err = c.Tx.QueryRowContext(c.Ctx(), `SELECT id,expires_at FROM vendor_bond_quotes WHERE vendor_id=$1 AND coin=$2 AND reference_currency=$3 AND committed_at IS NULL FOR UPDATE`, c.User.ID, coin, p.Currency).Scan(&id, &expires)
	if err != nil && err != sql.ErrNoRows {
		return actionResult{}, err
	}
	existing := err == nil
	var rateTime any
	rateSources := []string{}
	if !asOf.IsZero() {
		rateTime = asOf
		if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT sources FROM fiat_rates WHERE fiat=$1 AND coin=$2", p.Currency, coin).Scan(&rateSources); err != nil {
			return actionResult{}, err
		}
	}
	if existing {
		if expires.After(time.Now()) {
			return actionResult{}, fail(409, "You already have a current bond quote. Reload the vendor desk to see its address and amount.")
		}
		_, err = c.Tx.ExecContext(c.Ctx(), `UPDATE vendor_bond_quotes SET amount=$2,crypto_amount=$3,rate=$4,rate_as_of=$5,rate_sources=$6,quoted_at=now(),expires_at=now()+interval '30 minutes',observation_ok=false WHERE id=$1`, id, face, target, rate, rateTime, rateSources)
	} else {
		id = randomToken()
		address, ref, e := provider.Allocate(c.Ctx(), id)
		if e != nil {
			return actionResult{}, fail(503, "The "+coin+" bond wallet is unavailable. No deposit instructions were issued; try again later.")
		}
		if address == "" || ref == "" {
			return actionResult{}, fail(503, "The bond wallet returned an invalid destination. Nothing was changed.")
		}
		var collision bool
		if e = c.Tx.QueryRowContext(c.Ctx(), `SELECT EXISTS(SELECT 1 FROM escrows WHERE address=$1 OR refund_address=$1 OR fee_destination=$1)
			OR EXISTS(SELECT 1 FROM vendor_bond_quotes WHERE address=$1) OR EXISTS(SELECT 1 FROM escrow_siblings WHERE address=$1)
			OR EXISTS(SELECT 1 FROM settlements WHERE destination=$1) OR EXISTS(SELECT 1 FROM xmr_escrow_slots WHERE address=$1)
			OR EXISTS(SELECT 1 FROM xmr_escrow_wallets WHERE primary_address=$1) OR EXISTS(SELECT 1 FROM xmr_settlements WHERE destination=$1)
			OR EXISTS(SELECT 1 FROM settings WHERE key IN ('escrow_fee_address','escrow_xmr_fee_address') AND value=$1)`, address).Scan(&collision); e != nil {
			return actionResult{}, e
		}
		if collision {
			return actionResult{}, fail(409, "The bond wallet reused a destination. Contact the administrator; send nothing.")
		}
		_, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO vendor_bond_quotes(id,vendor_id,reference_currency,amount,coin,crypto_amount,address,provider_reference,rate,rate_as_of,rate_sources) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, c.User.ID, p.Currency, face, coin, target, address, ref, rate, rateTime, rateSources)
	}
	return actionResult{Redirect: "/vendor-dashboard", Notice: "vendor-bond-quote", Audit: "Requested " + coin + " vendor bond quote " + shortID(id)}, err
}

// One process observes bonds at a time, independently of page views and quote requests.
// Failed observations revoke trust immediately; age also fails closed if this process stops.
func (a *App) pollVendorBonds(ctx context.Context) error {
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var leader bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(782499)").Scan(&leader); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return err
	}
	if !leader {
		return err
	}
	defer func() {
		unlock, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, e := conn.ExecContext(unlock, "SELECT pg_advisory_unlock(782499)"); e != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	rows, err := conn.QueryContext(ctx, `SELECT id,vendor_id,coin,address,provider_reference,crypto_amount,quoted_at FROM vendor_bond_quotes ORDER BY checked_at NULLS FIRST,id LIMIT 100`)
	if err != nil {
		return err
	}
	type observation struct {
		id, vendor, coin, address, ref string
		target                         int64
		quoted                         time.Time
	}
	var batch []observation
	for rows.Next() {
		var v observation
		if err = rows.Scan(&v.id, &v.vendor, &v.coin, &v.address, &v.ref, &v.target, &v.quoted); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range batch {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var confirmed, pending int64
		good := false
		if p := a.bondProviders[v.coin]; p != nil {
			pass, cancel := context.WithTimeout(ctx, 5*time.Second)
			confirmed, pending, err = p.Observe(pass, v.address, v.ref)
			cancel()
			good = err == nil && confirmed >= 0 && pending >= 0 && pending <= int64(^uint64(0)>>1)-confirmed
		}
		if !good {
			confirmed, pending = 0, 0
		}
		tx, e := a.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		var one int
		if e = tx.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id=$1 FOR NO KEY UPDATE NOWAIT", v.vendor).Scan(&one); e != nil {
			tx.Rollback()
			if dbBusy(e) {
				continue
			}
			return e
		}
		_, e = tx.ExecContext(ctx, `UPDATE vendor_bond_quotes SET confirmed=$2,pending=$3,observation_ok=$4,checked_at=now(),
			committed_at=CASE WHEN committed_at IS NULL AND $4 AND $2::bigint+$3::bigint>=crypto_amount AND expires_at>=now() THEN now() ELSE committed_at END
			WHERE id=$1 AND crypto_amount=$5 AND quoted_at=$6`, v.id, confirmed, pending, good, v.target, v.quoted)
		if e != nil {
			tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	return nil
}

func previewVendorBond(d *PageData) {
	d.VendorBond = &VendorBondView{Amount: "0", Currency: "USD", Currencies: []string{"USD", "GBP", "EUR", "BTC", "XMR"}, Satisfied: true, Admin: d.Page == "admin"}
}

func loadVendorBond(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	p, err := vendorBondPolicy(ctx, a.db, false)
	if err != nil {
		return err
	}
	v := &VendorBondView{Amount: amount(p.Amount, bondDecimals(p.Currency)), Currency: p.Currency, Currencies: []string{"USD", "GBP", "EUR", "BTC", "XMR"}, Required: p.Amount > 0, Admin: d.Page == "admin"}
	v.Satisfied, err = vendorBondSatisfied(ctx, a.db, d.User.ID, false)
	if err != nil {
		return err
	}
	for _, coin := range []string{"BTC", "XMR"} {
		if a.bondProviders[coin] != nil && (bondDecimals(p.Currency) == 2 || p.Currency == coin) {
			v.Coins = append(v.Coins, coin)
		}
	}
	where := "WHERE q.vendor_id=$1"
	args := []any{d.User.ID}
	if v.Admin {
		where = ""
		args = nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT q.id,u.handle,q.reference_currency,q.amount,q.coin,q.crypto_amount,q.address,q.confirmed,q.pending,q.expires_at,q.checked_at,q.observation_ok,q.committed_at,q.rate,q.rate_as_of FROM vendor_bond_quotes q JOIN users u ON u.id=q.vendor_id `+where+` ORDER BY q.quoted_at DESC,q.id LIMIT 100`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var b BondQuoteView
		var face, target, confirmed, pending int64
		var expires time.Time
		var checked, committed, rateTime sql.NullTime
		var good bool
		if err = rows.Scan(&b.ID, &b.Handle, &b.Currency, &face, &b.Coin, &target, &b.Address, &confirmed, &pending, &expires, &checked, &good, &committed, &b.Rate, &rateTime); err != nil {
			rows.Close()
			return err
		}
		b.Face = amount(face, bondDecimals(b.Currency))
		b.Target = amount(target, currencyDecimals(b.Coin))
		b.Confirmed = amount(confirmed, currencyDecimals(b.Coin))
		b.Pending = amount(pending, currencyDecimals(b.Coin))
		b.Expires = expires.UTC().Format(time.RFC3339)
		if checked.Valid {
			b.Checked = checked.Time.UTC().Format(time.RFC3339)
		}
		if rateTime.Valid {
			b.RateAsOf = rateTime.Time.UTC().Format(time.RFC3339)
		}
		switch {
		case !good || !checked.Valid || time.Since(checked.Time) > bondBalanceMaxAge:
			b.Status = "Waiting for a fresh wallet check; selling access is paused"
		case committed.Valid && confirmed >= target:
			b.Status = "Confirmed and held"
		case committed.Valid:
			b.Status = "Awaiting confirmations or a top-up to restore the held bond"
		case time.Now().After(expires):
			b.Status = "Quote expired; renew it before sending more. Existing deposits remain at this address"
		default:
			b.Status = "Awaiting deposit and confirmations"
		}
		v.Quotes = append(v.Quotes, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if v.Admin {
		rates, err := a.db.QueryContext(ctx, "SELECT fiat,coin,rate,array_to_string(sources,', '),as_of,checked_at,available FROM fiat_rates ORDER BY fiat,coin")
		if err != nil {
			return err
		}
		for rates.Next() {
			var r BondRateView
			var asOf, checked time.Time
			if err = rates.Scan(&r.Fiat, &r.Coin, &r.Rate, &r.Sources, &asOf, &checked, &r.Available); err != nil {
				rates.Close()
				return err
			}
			r.AsOf = asOf.UTC().Format(time.RFC3339)
			r.Checked = checked.UTC().Format(time.RFC3339)
			now := time.Now()
			r.Available = a.fiatRates != nil && r.Available && now.Sub(asOf) <= fiatRateMaxAge && now.Sub(checked) <= fiatRateMaxAge && !asOf.After(now.Add(time.Minute)) && !checked.After(now.Add(time.Minute))
			r.Checked = checked.UTC().Format(time.RFC3339)
			v.Rates = append(v.Rates, r)
		}
		err = rates.Err()
		rates.Close()
		if err != nil {
			return err
		}
	}
	d.VendorBond = v
	return nil
}

// Bonds also pin the network class even if all order escrow adapters are off.
func (a *App) checkBondNetworkClass(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, "SELECT value FROM settings WHERE key IN ('vendor_bond_btc_network','vendor_bond_xmr_network') AND EXISTS(SELECT 1 FROM vendor_bond_quotes)")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var network string
		if err = rows.Scan(&network); err != nil {
			return err
		}
		if isLiveNetwork(network) != a.live {
			return errors.New("refusing to start: stored vendor bonds belong to a different network class; keep the original networks or use a new database")
		}
	}
	return rows.Err()
}
