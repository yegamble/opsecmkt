package market

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Rates are fiat units per whole coin. Only this scheduled worker contacts a
// source; a vendor's request reads the database and pins an immutable quote.
// Source operators can share underlying exchange data, so agreement is a
// sanity check, not proof that a coordinated market manipulation is impossible.
const (
	fiatRateInterval = 5 * time.Minute
	fiatRateMaxAge   = 15 * time.Minute
	fiatRateBodyMax  = 128 << 10
	fiatRateLock     = 782498
)

var errFiatRatesUnavailable = fail(503, "Fresh, corroborated exchange rates are unavailable for this currency. No bond address was issued; try again after the rate feed recovers.")

type fiatRateService struct {
	client *http.Client
	relay  string
}

type fiatObservation struct {
	Source string    `json:"source"`
	Fiat   string    `json:"fiat"`
	Coin   string    `json:"coin"`
	Rate   string    `json:"rate"`
	AsOf   time.Time `json:"as_of"`
}

type fiatConsensus struct {
	rate    string
	asOf    time.Time
	sources []string
}

func init() { registerBackground(func(ctx context.Context, a *App) { a.watchFiatRates(ctx) }) }

func supportedFiat(s string) bool     { return s == "USD" || s == "GBP" || s == "EUR" }
func supportedFiatCoin(s string) bool { return s == "BTC" || s == "XMR" }
func fiatSource(s string) bool        { return s == "coingecko" || s == "kraken" || s == "coinpaprika" }

// The operator must deliberately configure a proxy or a loopback relay. We
// never inherit HTTP_PROXY, follow redirects, use cookies, or fall back to a
// direct connection if that transport fails. Public endpoint URLs are fixed.
func newFiatRateService(getenv func(string) string) (*fiatRateService, error) {
	proxyText, relay := strings.TrimSpace(getenv("FIAT_RATE_PROXY_URL")), strings.TrimSpace(getenv("FIAT_RATE_RELAY_URL"))
	if proxyText == "" && relay == "" {
		return nil, nil
	}
	if proxyText != "" && relay != "" {
		return nil, errors.New("configure only one of FIAT_RATE_PROXY_URL and FIAT_RATE_RELAY_URL")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, MaxIdleConns: 3, MaxIdleConnsPerHost: 1, IdleConnTimeout: time.Minute, ResponseHeaderTimeout: 15 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	if relay != "" {
		u, err := url.Parse(relay)
		if err != nil || u.Scheme != "http" || !fiatLoopbackURL(u) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("FIAT_RATE_RELAY_URL must be an http://127.0.0.1:port/path or http://[::1]:port/path loopback JSON relay without credentials or query parameters")
		}
	} else {
		u, err := url.Parse(proxyText)
		if err != nil || u.Host == "" || u.Port() == "" || (u.Scheme != "socks5h" && u.Scheme != "http") || (u.Scheme == "http" && !fiatLoopbackURL(u)) || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("FIAT_RATE_PROXY_URL must be a socks5h://host:port proxy with remote DNS, or an http://127.0.0.1:port loopback proxy; public fetching never uses direct egress")
		}
		if _, err := net.LookupPort("tcp", u.Port()); err != nil {
			return nil, errors.New("FIAT_RATE_PROXY_URL must use a valid proxy port")
		}
		tr.Proxy = func(*http.Request) (*url.URL, error) { return u, nil }
	}
	return &fiatRateService{client: &http.Client{Transport: tr, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, relay: relay}, nil
}

func fiatLoopbackURL(u *url.URL) bool {
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback() && u.Port() != ""
}

func (a *App) configureFiatRates() error {
	var err error
	a.fiatRates, err = newFiatRateService(os.Getenv)
	return err
}

func (a *App) watchFiatRates(ctx context.Context) {
	if a.fiatRates == nil {
		return
	}
	// Hold one session lock for the worker's lifetime. Other replicas can read
	// the shared cache but cannot issue another set of public requests.
	var conn *sql.Conn
	defer func() {
		if conn != nil {
			fiatReleaseLeader(conn)
		}
	}()
	for {
		next := time.Now().UTC().Truncate(fiatRateInterval).Add(fiatRateInterval)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		pass, cancel := context.WithTimeout(ctx, 2*time.Minute)
		var err error
		if conn == nil {
			conn, err = fiatAcquireLeader(pass, a.db)
		}
		if err == nil && conn != nil {
			err = a.updateFiatRates(pass, conn)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			// Provider text, response bodies, URLs and proxy credentials never enter logs.
			log.Print("fiat rate cache: refresh unavailable")
		}
		if err != nil && conn != nil {
			fiatReleaseLeader(conn)
			conn = nil
		}
	}
}

func fiatAcquireLeader(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", fiatRateLock).Scan(&locked); err != nil {
		// A lost acknowledgement might mean the session acquired the lock.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		conn.Close()
		return nil, err
	}
	if !locked {
		conn.Close()
		return nil, nil
	}
	return conn, nil
}

func fiatReleaseLeader(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", fiatRateLock); err != nil {
		// A connection with an unreleased session lock must never return to the pool.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	conn.Close()
}

func (s *fiatRateService) read(ctx context.Context, endpoint string, out any) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("rate request failed")
	}
	r.Header.Set("User-Agent", "FiatRateCache/1")
	r.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(r)
	if err != nil {
		return errors.New("rate transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("rate provider refused")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, fiatRateBodyMax+1))
	if err != nil || len(b) > fiatRateBodyMax {
		return errors.New("rate response exceeds limit")
	}
	if err = json.Unmarshal(b, out); err != nil {
		return errors.New("rate response invalid")
	}
	return nil
}

func (s *fiatRateService) observations(ctx context.Context) []fiatObservation {
	if s.relay != "" {
		var response struct {
			Rates []fiatObservation `json:"rates"`
		}
		if s.read(ctx, s.relay, &response) != nil || len(response.Rates) > 18 {
			return nil
		}
		return response.Rates
	}
	// Three independently operated providers; Kraken is an exchange, the other
	// two aggregate market data. Each emits its actual observation timestamp.
	results := make(chan []fiatObservation, 3)
	for _, fetch := range []func(context.Context) []fiatObservation{s.coinGecko, s.kraken, s.coinPaprika} {
		go func() { results <- fetch(ctx) }()
	}
	var all []fiatObservation
	for range 3 {
		all = append(all, (<-results)...)
	}
	return all
}

// Provider references: https://docs.coingecko.com/reference/simple-price and
// https://docs.coinpaprika.com/api-reference/tickers/get-ticker-for-a-specific-coin.
func (s *fiatRateService) coinGecko(ctx context.Context) []fiatObservation {
	var response map[string]struct {
		USD     json.Number `json:"usd"`
		GBP     json.Number `json:"gbp"`
		EUR     json.Number `json:"eur"`
		Updated int64       `json:"last_updated_at"`
	}
	if s.read(ctx, "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin,monero&vs_currencies=usd,gbp,eur&include_last_updated_at=true&precision=full", &response) != nil {
		return nil
	}
	var out []fiatObservation
	for _, pair := range []struct{ id, coin string }{{"bitcoin", "BTC"}, {"monero", "XMR"}} {
		r := response[pair.id]
		for _, v := range []struct {
			fiat  string
			price json.Number
		}{{"USD", r.USD}, {"GBP", r.GBP}, {"EUR", r.EUR}} {
			out = append(out, fiatObservation{"coingecko", v.fiat, pair.coin, v.price.String(), time.Unix(r.Updated, 0).UTC()})
		}
	}
	return out
}

func (s *fiatRateService) coinPaprika(ctx context.Context) []fiatObservation {
	var out []fiatObservation
	for _, pair := range []struct{ id, coin string }{{"btc-bitcoin", "BTC"}, {"xmr-monero", "XMR"}} {
		var r struct {
			ID      string    `json:"id"`
			Updated time.Time `json:"last_updated"`
			Quotes  map[string]struct {
				Price json.Number `json:"price"`
			} `json:"quotes"`
		}
		if s.read(ctx, "https://api.coinpaprika.com/v1/tickers/"+pair.id+"?quotes=USD,GBP,EUR", &r) != nil || r.ID != pair.id {
			continue
		}
		for _, fiat := range []string{"USD", "GBP", "EUR"} {
			out = append(out, fiatObservation{"coinpaprika", fiat, pair.coin, r.Quotes[fiat].Price.String(), r.Updated})
		}
	}
	return out
}

// Kraken Ticker has no source timestamp. Recent Trades gives the price and
// timestamp of the latest trade: https://docs.kraken.com/api-reference/market-data/get-recent-trades.
// XMR/GBP is derived from this provider's XMR/USD and BTC/GBP, BTC/USD trades;
// it retains the oldest of the three timestamps and counts as one source.
func (s *fiatRateService) kraken(ctx context.Context) []fiatObservation {
	var out []fiatObservation
	for _, pair := range []struct{ pair, fiat, coin string }{{"XBTUSD", "USD", "BTC"}, {"XBTGBP", "GBP", "BTC"}, {"XBTEUR", "EUR", "BTC"}, {"XMRUSD", "USD", "XMR"}, {"XMREUR", "EUR", "XMR"}} {
		var response struct {
			Error  []string                   `json:"error"`
			Result map[string]json.RawMessage `json:"result"`
		}
		if s.read(ctx, "https://api.kraken.com/0/public/Trades?pair="+pair.pair+"&count=1", &response) != nil || len(response.Error) != 0 {
			continue
		}
		if len(response.Result) != 2 {
			continue
		}
		for key, raw := range response.Result {
			if key == "last" {
				continue
			}
			var trades [][]json.RawMessage
			if json.Unmarshal(raw, &trades) != nil || len(trades) != 1 || len(trades[0]) < 3 {
				continue
			}
			var price string
			var timestamp json.Number
			if json.Unmarshal(trades[0][0], &price) != nil || json.Unmarshal(trades[0][2], &timestamp) != nil {
				continue
			}
			whole, _, _ := strings.Cut(timestamp.String(), ".")
			seconds, err := strconv.ParseInt(whole, 10, 64)
			if err != nil {
				continue
			}
			out = append(out, fiatObservation{"kraken", pair.fiat, pair.coin, price, time.Unix(seconds, 0).UTC()})
		}
	}
	var usdBTC, gbpBTC, usdXMR *fiatObservation
	for i := range out {
		o := &out[i]
		switch {
		case o.Coin == "BTC" && o.Fiat == "USD":
			usdBTC = o
		case o.Coin == "BTC" && o.Fiat == "GBP":
			gbpBTC = o
		case o.Coin == "XMR" && o.Fiat == "USD":
			usdXMR = o
		}
	}
	if usdBTC != nil && gbpBTC != nil && usdXMR != nil {
		a, ea := parseFiatRate(usdBTC.Rate)
		b, eb := parseFiatRate(gbpBTC.Rate)
		c, ec := parseFiatRate(usdXMR.Rate)
		if ea == nil && eb == nil && ec == nil {
			rate := fiatRateDecimal(new(big.Rat).Quo(new(big.Rat).Mul(c, b), a))
			asOf := usdBTC.AsOf
			if gbpBTC.AsOf.Before(asOf) {
				asOf = gbpBTC.AsOf
			}
			if usdXMR.AsOf.Before(asOf) {
				asOf = usdXMR.AsOf
			}
			out = append(out, fiatObservation{"kraken", "GBP", "XMR", rate, asOf})
		}
	}
	return out
}

func parseFiatRate(s string) (*big.Rat, error) {
	if s == "" || len(s) > 40 {
		return nil, errors.New("invalid rate")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && (len(parts[1]) == 0 || len(parts[1]) > 18)) {
		return nil, errors.New("invalid rate")
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return nil, errors.New("invalid rate")
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 || r.Cmp(big.NewRat(1, 1_000_000)) < 0 || r.Cmp(big.NewRat(1_000_000_000_000, 1)) > 0 {
		return nil, errors.New("rate out of range")
	}
	return r, nil
}

// Truncate down at 18 places: the derived rate must never understate the coin
// amount owed because a rounded-up fiat-per-coin denominator was used.
func fiatRateDecimal(r *big.Rat) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	n := new(big.Int).Quo(new(big.Int).Mul(r.Num(), scale), r.Denom())
	s := n.String()
	if len(s) <= 18 {
		s = strings.Repeat("0", 19-len(s)) + s
	}
	return strings.TrimRight(strings.TrimRight(s[:len(s)-18]+"."+s[len(s)-18:], "0"), ".")
}

func fiatRateClose(a, b *big.Rat, percent int64) bool {
	diff := new(big.Rat).Sub(a, b)
	diff.Abs(diff)
	return diff.Cmp(new(big.Rat).Mul(b, big.NewRat(percent, 100))) <= 0
}

func consensusFiatRate(all []fiatObservation, fiat, coin string, now time.Time, bootstrap bool) (fiatConsensus, error) {
	var rows []fiatObservation
	seen := map[string]bool{}
	for _, o := range all {
		if o.Fiat != fiat || o.Coin != coin || !fiatSource(o.Source) {
			continue
		}
		// A relay cannot masquerade duplicated observations as independent feeds.
		if seen[o.Source] {
			return fiatConsensus{}, errors.New("duplicate rate source")
		}
		seen[o.Source] = true
		if _, err := parseFiatRate(o.Rate); err != nil || o.AsOf.IsZero() || now.Sub(o.AsOf) > fiatRateMaxAge || o.AsOf.After(now.Add(time.Minute)) {
			continue
		}
		rows = append(rows, o)
	}
	if len(rows) < 2 || (bootstrap && len(rows) != 3) {
		return fiatConsensus{}, errors.New("insufficient rate sources")
	}
	slices.SortFunc(rows, func(a, b fiatObservation) int {
		ar, _ := parseFiatRate(a.Rate)
		br, _ := parseFiatRate(b.Rate)
		return ar.Cmp(br)
	})
	// Select the unique largest cluster with full max/min spread <=5%. If
	// clusters tie, accepting either would silently privilege an arbitrary feed.
	bestStart, bestN, ties := 0, 0, 0
	for start := range rows {
		lo, _ := parseFiatRate(rows[start].Rate)
		for end := start + 1; end < len(rows); end++ {
			hi, _ := parseFiatRate(rows[end].Rate)
			if !fiatRateClose(hi, lo, 5) {
				break
			}
			n := end - start + 1
			if n > bestN {
				bestStart, bestN, ties = start, n, 1
			} else if n == bestN {
				ties++
			}
		}
	}
	if bestN < 2 || ties != 1 || (bootstrap && bestN != 3) {
		return fiatConsensus{}, errors.New("rate sources disagree")
	}
	rows = rows[bestStart : bestStart+bestN]
	median, _ := parseFiatRate(rows[len(rows)/2].Rate)
	if len(rows)%2 == 0 {
		lower, _ := parseFiatRate(rows[len(rows)/2-1].Rate)
		median.Add(median, lower)
		median.Quo(median, big.NewRat(2, 1))
	}
	c := fiatConsensus{rate: fiatRateDecimal(median), asOf: rows[0].AsOf}
	for _, o := range rows {
		c.sources = append(c.sources, o.Source)
		if o.AsOf.Before(c.asOf) {
			c.asOf = o.AsOf
		}
	}
	slices.Sort(c.sources)
	return c, nil
}

// A jump needs corroboration at a later scheduled poll, including a newer
// observation; replaying the same timestamp cannot satisfy the circuit breaker.
func fiatAcceptCandidate(c fiatConsensus, previous, pending string, pendingAt, pendingAsOf sql.NullTime, now time.Time) bool {
	old, err := parseFiatRate(previous)
	if err != nil {
		return true
	}
	next, _ := parseFiatRate(c.rate)
	if fiatRateClose(next, old, 20) {
		return true
	}
	p, err := parseFiatRate(pending)
	return err == nil && pendingAt.Valid && pendingAsOf.Valid && now.Sub(pendingAt.Time) >= fiatRateInterval && now.Sub(pendingAt.Time) <= 2*fiatRateInterval && c.asOf.After(pendingAsOf.Time) && fiatRateClose(next, p, 5)
}

func (a *App) pollFiatRates(ctx context.Context) error {
	conn, err := fiatAcquireLeader(ctx, a.db)
	if err != nil || conn == nil {
		return err
	}
	defer fiatReleaseLeader(conn)
	return a.updateFiatRates(ctx, conn)
}

func (a *App) updateFiatRates(ctx context.Context, conn *sql.Conn) error {
	// Do not fetch under a session whose leader lock was lost with its connection.
	if err := conn.PingContext(ctx); err != nil {
		return err
	}
	all := a.fiatRates.observations(ctx)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	for _, fiat := range []string{"USD", "GBP", "EUR"} {
		for _, coin := range []string{"BTC", "XMR"} {
			var previous, pending string
			var pendingAt, pendingAsOf sql.NullTime
			err = tx.QueryRowContext(ctx, "SELECT rate,pending_rate,pending_at,pending_as_of FROM fiat_rates WHERE fiat=$1 AND coin=$2 FOR UPDATE", fiat, coin).Scan(&previous, &pending, &pendingAt, &pendingAsOf)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			c, consensusErr := consensusFiatRate(all, fiat, coin, now, previous == "")
			if consensusErr != nil {
				_, err = tx.ExecContext(ctx, "UPDATE fiat_rates SET available=false,checked_at=$3,pending_rate='',pending_at=NULL,pending_as_of=NULL WHERE fiat=$1 AND coin=$2", fiat, coin, now)
			} else if !fiatAcceptCandidate(c, previous, pending, pendingAt, pendingAsOf, now) {
				// Keep a pending candidate only until its next scheduled poll.
				if pending == "" || !pendingAt.Valid || now.Sub(pendingAt.Time) > 2*fiatRateInterval {
					pending, pendingAt = c.rate, sql.NullTime{Time: now.Truncate(fiatRateInterval), Valid: true}
					pendingAsOf = sql.NullTime{Time: c.asOf, Valid: true}
				}
				_, err = tx.ExecContext(ctx, "UPDATE fiat_rates SET available=false,checked_at=$3,pending_rate=$4,pending_at=$5,pending_as_of=$6 WHERE fiat=$1 AND coin=$2", fiat, coin, now, pending, pendingAt.Time, pendingAsOf.Time)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO fiat_rates(fiat,coin,rate,as_of,sources,checked_at,available,pending_rate,pending_at)
					VALUES($1,$2,$3,$4,$5,$6,true,'',NULL) ON CONFLICT(fiat,coin) DO UPDATE SET
					rate=excluded.rate,as_of=excluded.as_of,sources=excluded.sources,checked_at=excluded.checked_at,available=true,pending_rate='',pending_at=NULL,pending_as_of=NULL`, fiat, coin, c.rate, c.asOf, c.sources, now)
			}
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func fiatAtomicQuote(ctx context.Context, q rowQuerier, minor int64, fiat, coin string) (int64, string, time.Time, error) {
	if minor <= 0 || !supportedFiat(fiat) || !supportedFiatCoin(coin) {
		return 0, "", time.Time{}, fail(400, "Choose a positive USD, GBP or EUR amount and BTC or XMR.")
	}
	var rate string
	var asOf, checkedAt time.Time
	var available bool
	// The quote caller uses its write transaction. This share lock pins the
	// accepted row and its source list until the bond quote commits, excluding a
	// concurrent pause or source replacement. A refresher in progress refuses
	// promptly instead of holding the user's transaction behind a network job.
	if err := q.QueryRowContext(ctx, "SELECT rate,as_of,checked_at,available FROM fiat_rates WHERE fiat=$1 AND coin=$2 FOR SHARE NOWAIT", fiat, coin).Scan(&rate, &asOf, &checkedAt, &available); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "55P03" {
			return 0, "", time.Time{}, errFiatRatesUnavailable
		}
		if err != sql.ErrNoRows {
			return 0, "", time.Time{}, err
		}
		return 0, "", time.Time{}, errFiatRatesUnavailable
	}
	now := time.Now()
	if !available || now.Sub(asOf) > fiatRateMaxAge || now.Sub(checkedAt) > fiatRateMaxAge || asOf.After(now.Add(time.Minute)) || checkedAt.After(now.Add(time.Minute)) {
		return 0, "", time.Time{}, errFiatRatesUnavailable
	}
	n, err := fiatAtomicAmount(minor, rate, coin)
	return n, rate, asOf, err
}

func fiatAtomicAmount(minor int64, rate, coin string) (int64, error) {
	r, err := parseFiatRate(rate)
	if err != nil || minor <= 0 || !supportedFiatCoin(coin) {
		return 0, errFiatRatesUnavailable
	}
	scale := int64(100_000_000)
	if coin == "XMR" {
		scale = 1_000_000_000_000
	}
	owed := new(big.Rat).Quo(new(big.Rat).Mul(big.NewRat(minor, 100), big.NewRat(scale, 1)), r)
	n, rem := new(big.Int), new(big.Int)
	n.QuoRem(owed.Num(), owed.Denom(), rem)
	if rem.Sign() != 0 {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() || n.Sign() <= 0 {
		return 0, fail(400, "The converted bond amount is out of range.")
	}
	return n.Int64(), nil
}
