package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func fiatTestObservations(now time.Time, values ...string) []fiatObservation {
	names := []string{"coingecko", "kraken", "coinpaprika"}
	var out []fiatObservation
	for i, rate := range values {
		out = append(out, fiatObservation{names[i], "USD", "BTC", rate, now})
	}
	return out
}

func TestFiatRateConsensusRejectsOutliersAndUnavailableFeeds(t *testing.T) {
	now := time.Now().UTC()
	for _, tt := range []struct {
		name      string
		rates     []fiatObservation
		bootstrap bool
		want      string
	}{
		{"three feeds median", fiatTestObservations(now, "100", "102", "101"), true, "101"},
		{"outlier excluded", fiatTestObservations(now, "100", "102", "50"), false, "101"},
		{"bootstrap outlier refuses", fiatTestObservations(now, "100", "102", "50"), true, ""},
		{"two feeds agree", fiatTestObservations(now, "100", "105"), false, "102.5"},
		{"two feeds disagree", fiatTestObservations(now, "100", "106"), false, ""},
		{"bootstrap requires three", fiatTestObservations(now, "100", "101"), true, ""},
		{"ambiguous clusters refuse", fiatTestObservations(now, "100", "104", "108"), false, ""},
		{"one feed refuses", fiatTestObservations(now, "100"), false, ""},
		{"old observations refuse", fiatTestObservations(now.Add(-fiatRateMaxAge-time.Second), "100", "101", "102"), false, ""},
		{"future observations refuse", fiatTestObservations(now.Add(2*time.Minute), "100", "101", "102"), false, ""},
		{"zero rates refuse", fiatTestObservations(now, "0", "0", "0"), false, ""},
		{"invalid rates refuse", fiatTestObservations(now, "NaN", "-1", "1e9999"), false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := consensusFiatRate(tt.rates, "USD", "BTC", now, tt.bootstrap)
			if tt.want == "" {
				if err == nil {
					t.Fatalf("accepted unsafe consensus %+v", c)
				}
				return
			}
			if err != nil || c.rate != tt.want {
				t.Fatalf("consensus %+v, %v; want %s", c, err, tt.want)
			}
		})
	}
	duplicate := fiatTestObservations(now, "100", "101", "102")
	duplicate[2].Source = duplicate[0].Source
	if _, err := consensusFiatRate(duplicate, "USD", "BTC", now, false); err == nil {
		t.Fatal("duplicate operator accepted as independent")
	}
	older := fiatTestObservations(now, "100", "101", "102")
	older[2].AsOf = now.Add(-10 * time.Minute)
	c, err := consensusFiatRate(older, "USD", "BTC", now, true)
	if err != nil || !c.asOf.Equal(older[2].AsOf) {
		t.Fatalf("did not retain oldest observation: %+v %v", c, err)
	}
}

func TestFiatRateFlashMoveNeedsLaterFreshCorroboration(t *testing.T) {
	now := time.Now().UTC().Truncate(fiatRateInterval)
	c := fiatConsensus{rate: "70", asOf: now}
	if fiatAcceptCandidate(c, "100", "", sql.NullTime{}, sql.NullTime{}, now) {
		t.Fatal("first flash crash accepted")
	}
	pending := sql.NullTime{Time: now, Valid: true}
	if fiatAcceptCandidate(c, "100", "70", pending, pending, now.Add(fiatRateInterval)) {
		t.Fatal("same source observations replayed to confirm crash")
	}
	c.asOf = now.Add(fiatRateInterval - time.Minute)
	if !fiatAcceptCandidate(c, "100", "70", pending, pending, now.Add(fiatRateInterval)) {
		t.Fatal("later corroborated sustained move refused")
	}
	c.rate = "40"
	if fiatAcceptCandidate(c, "100", "70", pending, pending, now.Add(fiatRateInterval)) {
		t.Fatal("different second crash bypassed circuit breaker")
	}
	c.rate = "101"
	if !fiatAcceptCandidate(c, "100", "70", pending, pending, now.Add(fiatRateInterval)) {
		t.Fatal("recovery to baseline refused")
	}
	c.rate = "70"
	if fiatAcceptCandidate(c, "100", "70", pending, pending, now.Add(3*fiatRateInterval)) {
		t.Fatal("old pending candidate accepted")
	}
}

func TestFiatAtomicQuotesRoundUpAndRejectOverflow(t *testing.T) {
	for _, tt := range []struct {
		minor      int64
		rate, coin string
		want       int64
	}{
		{10_000, "60000", "BTC", 166667},
		{10_000, "200", "XMR", 500_000_000_000},
		{1, "99999", "BTC", 11},
		{1, "0.000001", "BTC", 1_000_000_000_000},
		{math.MaxInt64, "1000000000000", "BTC", 9_223_372_036_855},
	} {
		got, err := fiatAtomicAmount(tt.minor, tt.rate, tt.coin)
		if err != nil || got != tt.want {
			t.Fatalf("%d/%s %s: %d %v, want %d", tt.minor, tt.rate, tt.coin, got, err, tt.want)
		}
	}
	for _, tt := range []struct {
		minor      int64
		rate, coin string
	}{{math.MaxInt64, "0.000001", "XMR"}, {0, "100", "BTC"}, {1, "0", "BTC"}, {1, "-1", "BTC"}, {1, "100", "ETH"}, {1, "1/2", "BTC"}, {1, "1.0000000000000000001", "BTC"}} {
		if _, err := fiatAtomicAmount(tt.minor, tt.rate, tt.coin); err == nil {
			t.Fatalf("unsafe conversion accepted %+v", tt)
		}
	}
}

func TestFiatRateTransportRequiresExplicitSafeConfiguration(t *testing.T) {
	for _, tt := range []struct {
		proxy, relay   string
		enabled, valid bool
	}{
		{"", "", false, true},
		{"socks5h://127.0.0.1:9050", "", true, true},
		{"socks5h://tor:9050", "", true, true},
		{"http://127.0.0.1:8123", "", true, true},
		{"", "http://127.0.0.1:8123/rates", true, true},
		{"", "http://[::1]:8123/rates", true, true},
		{"socks5://127.0.0.1:9050", "", false, false},
		{"http://public.example:80", "", false, false},
		{"socks5h://tor:wrong", "", false, false},
		{"socks5h://tor:9050/path", "", false, false},
		{"", "https://public.example/rates", false, false},
		{"", "http://localhost:8123/rates", false, false},
		{"", "http://127.0.0.1:8123/rates?user=abc", false, false},
		{"socks5h://tor:9050", "http://127.0.0.1:8123/rates", false, false},
	} {
		getenv := func(k string) string {
			if k == "FIAT_RATE_PROXY_URL" {
				return tt.proxy
			}
			if k == "FIAT_RATE_RELAY_URL" {
				return tt.relay
			}
			return ""
		}
		s, err := newFiatRateService(getenv)
		if (err == nil) != tt.valid || (s != nil) != tt.enabled {
			t.Fatalf("config %+v yielded service %v error %v", tt, s != nil, err)
		}
	}
}

func TestFiatRateLocalRelayHasNoCookiesAndRefusesRedirectsAndOversize(t *testing.T) {
	var seen int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++
		if r.Header.Get("Cookie") != "" || r.Header.Get("User-Agent") != "FiatRateCache/1" || r.Header.Get("Referer") != "" {
			t.Errorf("unexpected identifying headers: %v", r.Header)
		}
		w.Header().Set("Set-Cookie", "tracking=bad")
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/target", 302)
		case "/oversize":
			fmt.Fprint(w, strings.Repeat("x", fiatRateBodyMax+1))
		default:
			fmt.Fprint(w, `{"rates":[]}`)
		}
	}))
	defer server.Close()
	s, err := newFiatRateService(func(k string) string {
		if k == "FIAT_RATE_RELAY_URL" {
			return server.URL
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	var response any
	if s.read(context.Background(), server.URL, &response) != nil || s.read(context.Background(), server.URL, &response) != nil {
		t.Fatal("local relay unavailable")
	}
	if s.read(context.Background(), server.URL+"/redirect", &response) == nil {
		t.Fatal("redirect accepted")
	}
	if seen != 3 {
		t.Fatalf("redirect target reached: %d calls", seen)
	}
	if s.read(context.Background(), server.URL+"/oversize", &response) == nil {
		t.Fatal("oversize body accepted")
	}
}

func TestFiatPublicSourcesNeverFallBackAfterProxyRefusal(t *testing.T) {
	var mu sync.Mutex
	var targets []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targets = append(targets, r.Host)
		mu.Unlock()
		if r.Method != "CONNECT" {
			t.Errorf("public HTTPS tunnel expected, got %s", r.Method)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	s, err := newFiatRateService(func(k string) string {
		if k == "FIAT_RATE_PROXY_URL" {
			return proxy.URL
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.observations(context.Background()); len(got) != 0 {
		t.Fatalf("proxy refusal still returned %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(targets) != 8 {
		t.Fatalf("fixed source requests not proxied: %v", targets)
	}
	for _, host := range targets {
		if host != "api.coingecko.com:443" && host != "api.kraken.com:443" && host != "api.coinpaprika.com:443" {
			t.Fatalf("unexpected public target %s", host)
		}
	}
}

func TestFiatSOCKSProxyResolvesPublicNamesRemotely(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var names []string
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				header := make([]byte, 2)
				if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					return
				}
				_, _ = conn.Write([]byte{5, 0})
				request := make([]byte, 5)
				if _, err := io.ReadFull(conn, request); err != nil {
					return
				}
				if request[0] != 5 || request[1] != 1 || request[3] != 3 {
					t.Errorf("SOCKS request resolved hostname locally: %v", request)
					return
				}
				destination := make([]byte, int(request[4])+2)
				if _, err := io.ReadFull(conn, destination); err != nil {
					return
				}
				mu.Lock()
				names = append(names, string(destination[:len(destination)-2]))
				mu.Unlock()
				// Refuse the tunnel. No client may retry a direct connection.
				_, _ = conn.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0})
			}()
		}
	}()
	s, err := newFiatRateService(func(k string) string {
		if k == "FIAT_RATE_PROXY_URL" {
			return "socks5h://" + listener.Addr().String()
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.observations(context.Background()); len(got) != 0 {
		t.Fatalf("refused SOCKS tunnel returned rates %v", got)
	}
	_ = listener.Close()
	<-finished
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(names) != 8 {
		t.Fatalf("public requests did not use remote DNS: %v", names)
	}
	for _, name := range names {
		if name != "api.coingecko.com" && name != "api.kraken.com" && name != "api.coinpaprika.com" {
			t.Fatalf("unexpected proxied hostname %q", name)
		}
	}
}

type fiatTestTransport func(*http.Request) (*http.Response, error)

func (f fiatTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFiatPublicProviderAdaptersKeepSourceTimesAndDeriveXMRGBP(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := &fiatRateService{client: &http.Client{Transport: fiatTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.Header.Get("Cookie") != "" || r.Header.Get("User-Agent") != "FiatRateCache/1" {
			t.Errorf("unsafe request %+v", r)
		}
		var body string
		switch r.URL.Host {
		case "api.coingecko.com":
			if r.URL.Query().Get("ids") != "bitcoin,monero" || r.URL.Query().Get("vs_currencies") != "usd,gbp,eur" {
				t.Errorf("unexpected public query %s", r.URL)
			}
			body = fmt.Sprintf(`{"bitcoin":{"usd":60000,"gbp":48000,"eur":54000,"last_updated_at":%d},"monero":{"usd":200,"gbp":160,"eur":180,"last_updated_at":%d}}`, now.Unix(), now.Unix())
		case "api.coinpaprika.com":
			id := strings.TrimPrefix(r.URL.Path, "/v1/tickers/")
			usd, gbp, eur := 60000, 48000, 54000
			if id == "xmr-monero" {
				usd, gbp, eur = 200, 160, 180
			}
			body = fmt.Sprintf(`{"id":%q,"last_updated":%q,"quotes":{"USD":{"price":%d},"GBP":{"price":%d},"EUR":{"price":%d}}}`, id, now.Format(time.RFC3339), usd, gbp, eur)
		case "api.kraken.com":
			pair := r.URL.Query().Get("pair")
			prices := map[string]string{"XBTUSD": "60000", "XBTGBP": "48000", "XBTEUR": "54000", "XMRUSD": "200", "XMREUR": "180"}
			if r.URL.Query().Get("count") != "1" || prices[pair] == "" {
				t.Errorf("unexpected Kraken pair %s", r.URL)
			}
			body = fmt.Sprintf(`{"error":[],"result":{%q:[[%q,"1",%d,"b","m",""]],"last":"123"}}`, pair, prices[pair], now.Unix())
		default:
			t.Errorf("unexpected source %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	all := s.observations(context.Background())
	if len(all) != 18 {
		t.Fatalf("got %d observations, want 18", len(all))
	}
	for _, coin := range []string{"BTC", "XMR"} {
		for _, fiat := range []string{"USD", "GBP", "EUR"} {
			c, err := consensusFiatRate(all, fiat, coin, now, true)
			if err != nil || len(c.sources) != 3 || !c.asOf.Equal(now) {
				t.Fatalf("%s/%s: %+v %v", coin, fiat, c, err)
			}
			if coin == "XMR" && fiat == "GBP" && c.rate != "160" {
				t.Fatalf("XMR GBP cross rate %s", c.rate)
			}
		}
	}
}

func TestFiatAtomicQuoteDatabaseFreshnessAndPause(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	if _, _, _, err := fiatAtomicQuote(ctx, e.DB, 10_000, "USD", "BTC"); err != errFiatRatesUnavailable {
		t.Fatalf("missing quote error %v", err)
	}
	agExec(e, `INSERT INTO fiat_rates(fiat,coin,rate,as_of,sources,checked_at,available) VALUES('USD','BTC','60000',now(),ARRAY['coingecko','kraken','coinpaprika'],now(),true)`)
	got, rate, asOf, err := fiatAtomicQuote(ctx, e.DB, 10_000, "USD", "BTC")
	if err != nil || got != 166667 || rate != "60000" || asOf.IsZero() {
		t.Fatalf("fresh quote %d %s %v %v", got, rate, asOf, err)
	}
	for _, update := range []string{"UPDATE fiat_rates SET available=false", "UPDATE fiat_rates SET available=true,as_of=now()-interval '16 minutes'", "UPDATE fiat_rates SET as_of=now()+interval '2 minutes'", "UPDATE fiat_rates SET as_of=now(),checked_at=now()-interval '16 minutes'", "UPDATE fiat_rates SET checked_at=now()+interval '2 minutes'"} {
		agExec(e, update)
		if _, _, _, err := fiatAtomicQuote(ctx, e.DB, 10_000, "USD", "BTC"); err != errFiatRatesUnavailable {
			t.Fatalf("unavailable quote escaped: %v", err)
		}
	}
}

func TestFiatQuotePinsRateAndSourceSnapshotUntilCommit(t *testing.T) {
	e := newTestApp(t)
	agExec(e, `INSERT INTO fiat_rates(fiat,coin,rate,as_of,sources,checked_at,available) VALUES('USD','BTC','60000',now(),ARRAY['coingecko','kraken','coinpaprika'],now(),true)`)
	ctx := context.Background()
	quote, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer quote.Rollback()
	if _, _, _, err := fiatAtomicQuote(ctx, quote, 100, "USD", "BTC"); err != nil {
		t.Fatal(err)
	}
	refresh, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer refresh.Rollback()
	var rate string
	if err := refresh.QueryRowContext(ctx, "SELECT rate FROM fiat_rates WHERE fiat='USD' AND coin='BTC' FOR UPDATE NOWAIT").Scan(&rate); err == nil {
		t.Fatal("refresher replaced a rate/source snapshot before quote committed")
	}
	if err := refresh.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := quote.Commit(); err != nil {
		t.Fatal(err)
	}
	refresh, err = e.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer refresh.Rollback()
	if err := refresh.QueryRowContext(ctx, "SELECT rate FROM fiat_rates WHERE fiat='USD' AND coin='BTC' FOR UPDATE NOWAIT").Scan(&rate); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fiatAtomicQuote(ctx, e.DB, 100, "USD", "BTC"); err != errFiatRatesUnavailable {
		t.Fatalf("refresh in progress did not refuse promptly: %v", err)
	}
}

func TestFiatCacheSharedLeaderAndFailedSourcePause(t *testing.T) {
	e := newTestApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	var mu sync.Mutex
	var observations []fiatObservation
	var requests int
	for _, fiat := range []string{"USD", "GBP", "EUR"} {
		for _, coin := range []string{"BTC", "XMR"} {
			for _, source := range []string{"coingecko", "kraken", "coinpaprika"} {
				observations = append(observations, fiatObservation{source, fiat, coin, "100", now})
			}
		}
	}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		_ = json.NewEncoder(w).Encode(struct {
			Rates []fiatObservation `json:"rates"`
		}{observations})
	}))
	defer relay.Close()
	var err error
	e.A.fiatRates, err = newFiatRateService(func(k string) string {
		if k == "FIAT_RATE_RELAY_URL" {
			return relay.URL
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	leader, err := fiatAcquireLeader(ctx, e.DB)
	if err != nil || leader == nil {
		t.Fatalf("leader acquisition %v", err)
	}
	t.Cleanup(func() {
		if leader != nil {
			fiatReleaseLeader(leader)
		}
	})
	if err := e.A.pollFiatRates(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := requests
	mu.Unlock()
	if n != 0 {
		t.Fatal("second worker fetched despite existing leader")
	}
	if err := e.A.updateFiatRates(ctx, leader); err != nil {
		t.Fatal(err)
	}
	fiatReleaseLeader(leader)
	leader = nil
	if _, _, _, err := fiatAtomicQuote(ctx, e.DB, 100, "USD", "BTC"); err != nil {
		t.Fatalf("accepted cache unreadable %v", err)
	}
	mu.Lock()
	observations = nil
	mu.Unlock()
	if err := e.A.pollFiatRates(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fiatAtomicQuote(ctx, e.DB, 100, "USD", "BTC"); err != errFiatRatesUnavailable {
		t.Fatalf("failed feed used previous fresh quote: %v", err)
	}
}

func TestFiatCachePersistsJumpPauseAndRejectsReplayedObservations(t *testing.T) {
	e := newTestApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	var mu sync.Mutex
	observations := fiatTestObservations(now, "70", "70", "70")
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(struct {
			Rates []fiatObservation `json:"rates"`
		}{observations})
	}))
	defer relay.Close()
	var err error
	e.A.fiatRates, err = newFiatRateService(func(k string) string {
		if k == "FIAT_RATE_RELAY_URL" {
			return relay.URL
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, `INSERT INTO fiat_rates(fiat,coin,rate,as_of,sources,checked_at,available) VALUES('USD','BTC','100',now(),ARRAY['coingecko','kraken','coinpaprika'],now(),true)`)
	ctx := context.Background()
	if err := e.A.pollFiatRates(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fiatAtomicQuote(ctx, e.DB, 100, "USD", "BTC"); err != errFiatRatesUnavailable {
		t.Fatalf("flash move was not paused: %v", err)
	}
	var previous, pending string
	var pendingAsOf time.Time
	if err := e.DB.QueryRow("SELECT rate,pending_rate,pending_as_of FROM fiat_rates WHERE fiat='USD' AND coin='BTC'").Scan(&previous, &pending, &pendingAsOf); err != nil {
		t.Fatal(err)
	}
	if previous != "100" || pending != "70" || !pendingAsOf.Equal(now) {
		t.Fatalf("lost baseline or pending source timestamp: %s %s %v", previous, pending, pendingAsOf)
	}
	// Advance the scheduling checkpoint without altering the observation stamp.
	// A second poll replaying the same source cache must still remain paused.
	agExec(e, "UPDATE fiat_rates SET pending_at=now()-interval '5 minutes' WHERE fiat='USD' AND coin='BTC'")
	if err := e.A.pollFiatRates(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fiatAtomicQuote(ctx, e.DB, 100, "USD", "BTC"); err != errFiatRatesUnavailable {
		t.Fatalf("replayed flash observation accepted: %v", err)
	}
	mu.Lock()
	for i := range observations {
		observations[i].AsOf = now.Add(time.Second)
	}
	mu.Unlock()
	if err := e.A.pollFiatRates(ctx); err != nil {
		t.Fatal(err)
	}
	_, rate, _, err := fiatAtomicQuote(ctx, e.DB, 100, "USD", "BTC")
	if err != nil || rate != "70" {
		t.Fatalf("fresh later corroboration not accepted: %s %v", rate, err)
	}
}
